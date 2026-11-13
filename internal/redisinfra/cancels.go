package redisinfra

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"fillbrunn.ai/kube-agent/pkg/agent"
)

// unsubscribeTimeout bounds the one Redis call a watcher's stop makes, so
// that stopping a watch never hangs the run it belongs to.
const unsubscribeTimeout = 5 * time.Second

// cancelChannel names the pub/sub channel one arc's cancellation is announced
// on. A watcher subscribes to exactly its arc's channel, so an announcement
// for one arc reaches nobody watching another.
func cancelChannel(ref agent.ArcRef) string {
	return "kube-agent:cancel:" + ref.SessionID + ":" + ref.ArcID
}

// RedisCancels is a CancelNotifier and a CancelWatcher for many processes.
//
// It is pub/sub and nothing more, because the ordering that pub/sub cannot
// promise is already handled by the caller: Work subscribes before it
// replays the arc, so a cancel written before the subscription is in the
// replay, and one written after it is delivered here. A watcher that misses
// a message anyway only costs latency, never correctness, because the guard
// refuses the run's next guarded write whatever happened to the
// notification. A replayable log here would be paying for a guarantee the
// caller does not need.
type RedisCancels struct {
	client *redis.Client

	// mu guards watchers and the pub/sub connection they share. One
	// connection serves every watcher in the process, since a process with
	// several arcs under way should not hold a connection per arc.
	mu       sync.Mutex
	watchers map[agent.ArcRef]map[chan struct{}]struct{}
	pubsub   *redis.PubSub
}

// NewRedisCancels builds a cancellation hub over a Redis client.
func NewRedisCancels(client *redis.Client) *RedisCancels {
	return &RedisCancels{client: client, watchers: map[agent.ArcRef]map[chan struct{}]struct{}{}}
}

var _ agent.CancelNotifier = (*RedisCancels)(nil)
var _ agent.CancelWatcher = (*RedisCancels)(nil)

// NotifyCancel announces the arc's cancellation to whoever is watching it.
//
// It is best effort, and a failure here does not fail the cancel: the record
// is in the log, and the guard stops the run at its next step whatever
// happens to the announcement. A watcher that never receives it costs time
// and money, never correctness.
func (c *RedisCancels) NotifyCancel(ctx context.Context, ref agent.ArcRef) error {
	payload, err := json.Marshal(ref)
	if err != nil {
		return fmt.Errorf("could not announce the cancel of arc %s: %w", ref, err)
	}
	if err := c.client.Publish(ctx, cancelChannel(ref), payload).Err(); err != nil {
		return fmt.Errorf("could not announce the cancel of arc %s: %w", ref, err)
	}
	return nil
}

// Watch returns a channel that is closed when the arc is cancelled, and a
// stop to call once the caller is done with the arc.
//
// A watcher that cannot subscribe returns a channel nobody will close,
// because the run has to go on either way, which is the failure the
// CancelWatcher contract already names. Stopping after a cancel does
// nothing, and a channel is never closed twice, since a notified arc's
// watchers are removed when they are closed.
func (c *RedisCancels) Watch(ctx context.Context, ref agent.ArcRef) (<-chan struct{}, func()) {
	cancelled := make(chan struct{})
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.subscribe(ctx, ref); err != nil {
		return cancelled, func() {}
	}
	if c.watchers[ref] == nil {
		c.watchers[ref] = map[chan struct{}]struct{}{}
	}
	c.watchers[ref][cancelled] = struct{}{}
	return cancelled, func() { c.unwatch(ref, cancelled) }
}

// subscribe adds the arc's channel to the shared subscription, connecting
// once for the whole process and starting the loop that fans messages out
// to the watchers.
func (c *RedisCancels) subscribe(ctx context.Context, ref agent.ArcRef) error {
	if c.pubsub == nil {
		c.pubsub = c.client.Subscribe(ctx, cancelChannel(ref))
		go c.dispatch()
		return nil
	}
	return c.pubsub.Subscribe(ctx, cancelChannel(ref))
}

// dispatch fans published messages out to the watchers of the arc they
// name. It exits when the subscription's channel closes, which leaves the
// remaining watchers holding channels nobody will close: the same failure
// the contract names for a subscription that never started, and for the
// same reason, the runs have to go on.
func (c *RedisCancels) dispatch() {
	for msg := range c.pubsub.Channel() {
		var ref agent.ArcRef
		if json.Unmarshal([]byte(msg.Payload), &ref) != nil {
			continue
		}
		c.mu.Lock()
		for cancelled := range c.watchers[ref] {
			close(cancelled)
		}
		delete(c.watchers, ref)
		c.mu.Unlock()
	}
}

// unwatch removes a watcher, and drops the arc's subscription when it was
// the last watcher on the arc.
func (c *RedisCancels) unwatch(ref agent.ArcRef, cancelled chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	watching, ok := c.watchers[ref]
	if !ok {
		return
	}
	delete(watching, cancelled)
	if len(watching) > 0 {
		return
	}
	delete(c.watchers, ref)
	unsubscribeCtx, cancel := context.WithTimeout(context.Background(), unsubscribeTimeout)
	defer cancel()
	if err := c.pubsub.Unsubscribe(unsubscribeCtx, cancelChannel(ref)); err != nil {
		// A subscription that cannot be dropped costs a message nobody is
		// waiting for, which is the cheapest failure this package has.
		_ = err
	}
}
