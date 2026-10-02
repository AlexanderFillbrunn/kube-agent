package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"fillbrunn.ai/kube-agent/pkg/events"
)

// ErrCancelled reports that the arc was cancelled while the agent was working
// on it. The arc is already closed, so a caller seeing this has nothing left
// to write.
var ErrCancelled = errors.New("the arc was cancelled")

// CancelSource answers whether an arc has been cancelled. The answer is
// advisory: a cancel recorded after the read still slips through.
type CancelSource interface {
	// Cancelled returns the cancel recorded against an arc, or nil where
	// there is none.
	Cancelled(ctx context.Context, ref ArcRef) (*events.Event, error)
}

// GuardCancellation wraps a persistor so that a run stops once the arc it is
// working on has been cancelled somewhere else.
//
// The guard sits on the write path because a dispatch is written immediately
// before its tool runs, so a check there stops the tool rather than the run
// that follows it. A refused write is reported as ErrCancelled and nothing is
// written. One event that starts work refuses the whole batch it arrived in,
// since a batch is written whole or not at all.
//
// It remembers nothing between writes. A cancelled arc is read again on every
// guarded write, which costs one read for a run that is about to end anyway:
// the refusal ends the run at the first dispatch it reaches, so a cache of
// already-cancelled arcs saved about one read while racing between the runs
// that share a persistor and growing for the life of the process.
func GuardCancellation(persistor EventPersistor, cancels CancelSource) EventPersistor {
	return &guardedPersistor{persistor: persistor, cancels: cancels}
}

type guardedPersistor struct {
	persistor EventPersistor
	cancels   CancelSource
}

func (g *guardedPersistor) Persist(ctx context.Context, evs ...*events.Event) ([]*events.Event, error) {
	// Only a write that is about to start work is worth stopping. Refusing the
	// rest would lose the record without preventing anything.
	ref, guarded := guardedArc(evs)
	if !guarded {
		return g.persistor.Persist(ctx, evs...)
	}

	cancel, err := g.cancels.Cancelled(ctx, ref)
	if err != nil {
		// A guard that cannot answer lets the write through. The cancel is
		// still in the log, and the run stops at the next step that can see it.
		return g.persistor.Persist(ctx, evs...)
	}
	if cancel == nil {
		return g.persistor.Persist(ctx, evs...)
	}

	return nil, fmt.Errorf("%w: arc %s, %d events not written", ErrCancelled, ref, len(evs))
}

// guardedArc returns the arc a batch would start work on, and whether any
// event in it does. One is enough, because the batch is refused whole.
func guardedArc(evs []*events.Event) (ArcRef, bool) {
	for _, ev := range evs {
		if startsWork(ev) && ev.CorrelationID != nil {
			return ArcRef{SessionID: ev.SessionID, ArcID: *ev.CorrelationID}, true
		}
	}
	return ArcRef{}, false
}

// startsWork reports whether an event is written before something happens
// rather than after it. Only a dispatch and a task invocation are.
func startsWork(ev *events.Event) bool {
	switch ev.Typ {
	case events.ToolDispatchedEventType, events.AsyncTaskInvokeEventType:
		return true
	}
	return false
}

// CancelNotifier tells watchers that an arc has been cancelled.
//
// It is best effort, and a failure here does not fail the cancel: the record
// is in the log, and the guard stops the run at its next step whatever happens
// to the notification.
type CancelNotifier interface {
	NotifyCancel(ctx context.Context, ref ArcRef) error
}

// CancelWatcher reports that an arc has been cancelled, as soon as it can.
//
// It is what reaches work already under way, which the guard cannot: a tool
// handler selecting on its context, or a model stream mid-token. Without it
// those run to the end and their cost is spent whatever the log says.
//
// Nothing it promises is relied on. A notification that never arrives costs
// time and money, never correctness.
type CancelWatcher interface {
	// Watch returns a channel that is closed when the arc is cancelled, and a
	// stop to call once the caller is done with the arc. A watcher that
	// cannot subscribe returns a channel nobody will close, because the run
	// has to go on either way.
	Watch(ctx context.Context, ref ArcRef) (cancelled <-chan struct{}, stop func())
}

// LocalCancels is a CancelNotifier and a CancelWatcher for one process.
type LocalCancels struct {
	mu       sync.Mutex
	watchers map[ArcRef]map[chan struct{}]bool
}

func NewLocalCancels() *LocalCancels {
	return &LocalCancels{watchers: map[ArcRef]map[chan struct{}]bool{}}
}

func (c *LocalCancels) Watch(_ context.Context, ref ArcRef) (<-chan struct{}, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.watchers == nil {
		c.watchers = map[ArcRef]map[chan struct{}]bool{}
	}
	if c.watchers[ref] == nil {
		c.watchers[ref] = map[chan struct{}]bool{}
	}
	cancelled := make(chan struct{})
	c.watchers[ref][cancelled] = true

	return cancelled, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		// A channel the notifier has already closed is gone from the map, so
		// stopping after a cancel does nothing and never closes twice.
		if watching, ok := c.watchers[ref]; ok {
			delete(watching, cancelled)
			if len(watching) == 0 {
				delete(c.watchers, ref)
			}
		}
	}
}

// NotifyCancel closes every channel watching the arc. Watching again after
// this gets a channel nobody closes, which is correct: the cancel is in the
// log, and a run that starts after it reads it there.
func (c *LocalCancels) NotifyCancel(_ context.Context, ref ArcRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for cancelled := range c.watchers[ref] {
		close(cancelled)
	}
	delete(c.watchers, ref)
	return nil
}
