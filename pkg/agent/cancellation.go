package agent

import (
	"context"
	"errors"
	"fmt"

	"fillbrunn.ai/kube-agent/pkg/events"
)

// ErrCancelled reports that the arc was cancelled while the agent was
// working on it. Whoever decided the cancel recorded it, so a caller seeing
// this has nothing left to write: the arc is closed and the run is over.
var ErrCancelled = errors.New("the arc was cancelled")

// CancelSource answers whether an arc has been cancelled.
//
// It is a plain read, which makes it advisory: a cancel recorded after the
// read still slips through. That is enough for what it is for, which is
// stopping the next step rather than the current one. An event store that can
// append conditionally enforces the same rule atomically, and the agent does
// not change when it does.
type CancelSource interface {
	// Cancelled returns the cancel recorded against an arc, or nil where
	// there is none.
	Cancelled(ctx context.Context, arcID string) (*events.Event, error)
}

// GuardCancellation wraps a persistor so that a run stops once the arc it is
// working on has been cancelled somewhere else.
//
// The guard sits on the write path because that is where a run is about to do
// something: a dispatch is written immediately before its tool runs, so a
// check there is the difference between a stopped run and one that goes on
// spending money and changing clusters until its context catches up.
//
// A refused write is reported as CancelledError and nothing is written. The
// cancel itself is not folded into the running history, because it was
// recorded before the events this run has written since and a history does not
// take events out of order. The log is where the cancel belongs, and an arc
// loaded again reads it in the place it was written.
func GuardCancellation(persistor EventPersistor, cancels CancelSource) EventPersistor {
	return &guardedPersistor{persistor: persistor, cancels: cancels}
}

type guardedPersistor struct {
	persistor EventPersistor
	cancels   CancelSource

	// seen remembers the arcs already known to be cancelled, so that a run
	// which has been told once does not ask again on every write.
	seen map[string]bool
}

func (g *guardedPersistor) Persist(ctx context.Context, ev *events.Event) (*events.Event, error) {
	// Only a write that is about to start work is worth stopping. Everything
	// else records something that already happened, and refusing those would
	// lose the record without preventing anything.
	if !startsWork(ev) || ev.CorrelationID == nil {
		return g.persistor.Persist(ctx, ev)
	}
	arcID := *ev.CorrelationID

	if g.seen[arcID] {
		return nil, fmt.Errorf("%w: arc %s", ErrCancelled, arcID)
	}

	cancel, err := g.cancels.Cancelled(ctx, arcID)
	if err != nil {
		// A guard that cannot answer must not halt work that is otherwise
		// fine. The cancel is still in the log, and the run stops at the next
		// step that can see it.
		return g.persistor.Persist(ctx, ev)
	}
	if cancel == nil {
		return g.persistor.Persist(ctx, ev)
	}

	if g.seen == nil {
		g.seen = map[string]bool{}
	}
	g.seen[arcID] = true

	return nil, fmt.Errorf("%w: arc %s, %s not written", ErrCancelled, arcID, ev.Typ)
}

// startsWork reports whether an event is written before something happens
// rather than after it.
//
// Only two are. A dispatch precedes its tool call and a task invocation
// precedes the task. A reply and the calls it requests cost nothing further:
// nothing runs until a dispatch, and a request recorded against an arc nobody
// will work on stays a request.
func startsWork(ev *events.Event) bool {
	switch ev.Typ {
	case events.ToolDispatchedEventType, events.AsyncTaskInvokeEventType:
		return true
	}
	return false
}
