package agent

import (
	"context"
	"errors"
	"fmt"

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
	Cancelled(ctx context.Context, arcID string) (*events.Event, error)
}

// GuardCancellation wraps a persistor so that a run stops once the arc it is
// working on has been cancelled somewhere else.
//
// The guard sits on the write path because a dispatch is written immediately
// before its tool runs, so a check there stops the tool rather than the run
// that follows it. A refused write is reported as ErrCancelled and nothing is
// written.
func GuardCancellation(persistor EventPersistor, cancels CancelSource) EventPersistor {
	return &guardedPersistor{persistor: persistor, cancels: cancels}
}

type guardedPersistor struct {
	persistor EventPersistor
	cancels   CancelSource

	// seen remembers the arcs already known to be cancelled, so a run that has
	// been told once does not ask again on every write.
	seen map[string]bool
}

func (g *guardedPersistor) Persist(ctx context.Context, ev *events.Event) (*events.Event, error) {
	// Only a write that is about to start work is worth stopping. Refusing the
	// rest would lose the record without preventing anything.
	if !startsWork(ev) || ev.CorrelationID == nil {
		return g.persistor.Persist(ctx, ev)
	}
	arcID := *ev.CorrelationID

	if g.seen[arcID] {
		return nil, fmt.Errorf("%w: arc %s", ErrCancelled, arcID)
	}

	cancel, err := g.cancels.Cancelled(ctx, arcID)
	if err != nil {
		// A guard that cannot answer lets the write through. The cancel is
		// still in the log, and the run stops at the next step that can see it.
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
// rather than after it. Only a dispatch and a task invocation are.
func startsWork(ev *events.Event) bool {
	switch ev.Typ {
	case events.ToolDispatchedEventType, events.AsyncTaskInvokeEventType:
		return true
	}
	return false
}
