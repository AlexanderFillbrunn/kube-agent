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