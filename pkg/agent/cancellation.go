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
// written. One event that starts work refuses the whole batch it arrived in,
// since a batch is written whole or not at all.
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

func (g *guardedPersistor) Persist(ctx context.Context, evs ...*events.Event) ([]*events.Event, error) {
	// Only a write that is about to start work is worth stopping. Refusing the
	// rest would lose the record without preventing anything.
	arcID, guarded := guardedArc(evs)
	if !guarded {
		return g.persistor.Persist(ctx, evs...)
	}

	if g.seen[arcID] {
		return nil, fmt.Errorf("%w: arc %s", ErrCancelled, arcID)
	}

	cancel, err := g.cancels.Cancelled(ctx, arcID)
	if err != nil {
		// A guard that cannot answer lets the write through. The cancel is
		// still in the log, and the run stops at the next step that can see it.
		return g.persistor.Persist(ctx, evs...)
	}
	if cancel == nil {
		return g.persistor.Persist(ctx, evs...)
	}

	if g.seen == nil {
		g.seen = map[string]bool{}
	}
	g.seen[arcID] = true

	return nil, fmt.Errorf("%w: arc %s, %d events not written", ErrCancelled, arcID, len(evs))
}

// guardedArc returns the arc a batch would start work on, and whether any
// event in it does. One is enough, because the batch is refused whole.
func guardedArc(evs []*events.Event) (string, bool) {
	for _, ev := range evs {
		if startsWork(ev) && ev.CorrelationID != nil {
			return *ev.CorrelationID, true
		}
	}
	return "", false
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
