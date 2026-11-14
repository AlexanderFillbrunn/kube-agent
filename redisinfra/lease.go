// Package redisinfra holds the distributed halves the agent framework left
// local: the lease that keeps two runs off one arc when they are in two
// processes, the cancellation notice that reaches a run already under way,
// the queue that delivers the runs a write owes an arc, and the event log
// those processes share.
//
// Everything here talks to Redis by address from configuration. Nothing
// listens, nothing is reachable from outside, and nothing knows about an
// agent beyond the interfaces in pkg/agent it implements.
//
// The three pieces share one stance on failure: the log is the source of
// truth, and every Redis structure here is a hint about it. A lost lease, a
// missed notification or a redelivered item costs a replay or a run that does
// nothing, never a wrong write.
package redisinfra

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"fillbrunn.ai/kube-agent/pkg/agent"
)

// leaseKey names the key one arc's lease lives under. The session and the arc
// ID are both in the key, because nothing that locks by arc can be
// partitioned by session without being told the session as well.
func leaseKey(ref agent.ArcRef) string {
	return "kube-agent:lease:" + ref.SessionID + ":" + ref.ArcID
}

// releaseScript deletes the lease only while it still names the holder asking
// for the deletion. A holder whose TTL has expired finds the key naming
// somebody else and deletes nothing, which is what keeps a late release from
// taking the next holder's lease away.
var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// renewScript extends the TTL only while the key still names the holder
// asking for the extension. A renewal that cannot prove it still holds the
// lease does nothing, so a heartbeat can never resurrect a lease that
// expired and was taken by somebody else.
var renewScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("pexpire", KEYS[1], ARGV[2])
end
return 0
`)

// RedisLease is an ArcLease for many processes, on the same rule as the
// local one: one holder per arc, and an arc left to whoever holds it.
//
// The lease is a Redis key with a TTL. The TTL is the backstop for a holder
// that dies without releasing, and it is also the honest limit of what the
// lease promises: the existing ArcLease contract has no fencing, so a holder
// whose TTL expired can still be writing, and the persistor cannot detect
// it. A generous TTL makes that a run finishing late rather than a run
// overlapping its successor, which is why the default is minutes and not
// seconds.
//
// A holder that stays alive keeps its lease by heartbeating it before the
// TTL runs out. The heartbeat proves it still holds the lease before
// extending it, so a slow run does not lose the arc to its own expiry, and a
// run whose lease was lost anyway does not steal it back. Renewal is
// optional: a lease configured without a renewal interval is held for the
// TTL and no longer.
type RedisLease struct {
	client *redis.Client
	ttl    time.Duration
	renew  time.Duration
}

// LeaseOption configures a RedisLease.
type LeaseOption func(*RedisLease)

// WithLeaseTTL sets how long a lease survives a holder that stops
// heartbeating it. It should stay generous: shrinking it trades a rare
// overlapping run for a common stranded arc.
func WithLeaseTTL(ttl time.Duration) LeaseOption {
	return func(l *RedisLease) { l.ttl = ttl }
}

// WithRenewalInterval sets how often a holder extends its lease, or 0 to
// hold for the TTL alone.
func WithRenewalInterval(interval time.Duration) LeaseOption {
	return func(l *RedisLease) { l.renew = interval }
}

// NewRedisLease builds a lease over a Redis client. The client is used by
// every acquisition, so it must not be closed while a lease may be taken.
func NewRedisLease(client *redis.Client, options ...LeaseOption) *RedisLease {
	l := &RedisLease{client: client, ttl: time.Minute, renew: 20 * time.Second}
	for _, option := range options {
		option(l)
	}
	return l
}

var _ agent.ArcLease = (*RedisLease)(nil)

// Acquire takes the arc by writing a holder token under its key, which
// fails without error when somebody else's token is already there.
//
// The three outcomes are the contract's, and they are kept apart exactly:
// ok false means the arc is held and is not a failure, and an error means
// Redis could not be reached and the caller learns nothing about who holds
// the arc. A caller that read an unreachable lock service as "nobody holds
// it" would run the arc twice, so nothing here turns a reachability failure
// into a refusal.
//
// The release func returned on success stops the heartbeat and then deletes
// the key only if it still names this holder. It runs on its own context, so
// a run cancelled through its own context still lets go of the arc.
func (l *RedisLease) Acquire(ctx context.Context, ref agent.ArcRef) (func(), bool, error) {
	token, err := newLeaseToken()
	if err != nil {
		return nil, false, fmt.Errorf("could not mint a holder token for arc %s: %w", ref, err)
	}
	ok, err := l.client.SetNX(ctx, leaseKey(ref), token, l.ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("could not reach the lease on arc %s: %w", ref, err)
	}
	if !ok {
		return nil, false, nil
	}
	stopHeartbeat := l.startHeartbeat(ctx, ref, token)
	return func() {
		stopHeartbeat()
		// The release runs even when the run's context is done, since an
		// arc held past its run is an arc nobody else can take for a TTL.
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := releaseScript.Run(releaseCtx, l.client, []string{leaseKey(ref)}, token).Result(); err != nil {
			// A release that cannot reach Redis leaves the key to expire
			// on its TTL. That is the backstop the TTL exists for, and a
			// caller has nothing left to do about it.
			_ = err
		}
	}, true, nil
}

// startHeartbeat extends the lease on a ticker until it is stopped, and
// returns the stop. The heartbeat holds nothing the run needs: it is a
// goroutine that ends when the lease is released, lost, or its parent
// context cancelled, whichever comes first.
//
// A renewal that cannot reach Redis keeps trying, because a blip in the
// connection is not evidence the lease is gone. A renewal that reaches Redis
// and finds another holder's token is evidence the lease is gone, and the
// heartbeat stops: the run keeps writing, which is the unfenced-writer hole
// the ArcLease contract already owns.
func (l *RedisLease) startHeartbeat(ctx context.Context, ref agent.ArcRef, token string) (stop func()) {
	if l.renew <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(l.renew)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				held, err := renewScript.Run(ctx, l.client, []string{leaseKey(ref)}, token, l.ttl.Milliseconds()).Int()
				if err != nil {
					continue
				}
				if held == 0 {
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// newLeaseToken mints the value a holder writes under the lease key. It is
// random per acquisition, so a release or a renewal can tell its own lease
// from one taken over after its TTL expired.
func newLeaseToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
