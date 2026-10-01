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
