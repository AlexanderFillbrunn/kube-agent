package agent

import (
	"context"

	"fillbrunn.ai/kube-agent/pkg/events"
)

type Sink[E any] interface {
	Consume(ev E) error
}

type FuncSink[E any] struct {
	fn func(i E) error
}

func (fs FuncSink[E])Consume(i E) error {
	return fs.fn(i)
}

func SinkFromFunc[E any](f func(i E) error) Sink[E] {
	return FuncSink[E]{
		fn: f,
	}
}

type EventPersistor interface {
	Persist(ctx context.Context, e *events.Event) (*events.Event, error)
}

type EventSource interface {
	GetEventsForArc(ctx context.Context, id string) ([]*events.Event, error)
}

// StampSession files everything written through it under one session.
//
// The session is a property of the conversation, not of the run that happens
// to be extending it: the agent decides what to record, and where those
// records belong is settled once, here, for the user message that opens an arc
// as much as for what the agent writes afterwards.
func StampSession(persistor EventPersistor, sessionID string) EventPersistor {
	return &stampingPersistor{persistor: persistor, sessionID: sessionID}
}

type stampingPersistor struct {
	persistor EventPersistor
	sessionID string
}

func (s *stampingPersistor) Persist(ctx context.Context, ev *events.Event) (*events.Event, error) {
	// The caller's event is left alone, since it is theirs and they may still
	// be holding it.
	stamped := *ev
	stamped.SessionID = s.sessionID
	return s.persistor.Persist(ctx, &stamped)
}
