package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
)

type Sink[E any] interface {
	Consume(ev E) error
}

type FuncSink[E any] struct {
	fn func(i E) error
}

func (fs FuncSink[E]) Consume(i E) error {
	return fs.fn(i)
}

func SinkFromFunc[E any](f func(i E) error) Sink[E] {
	return FuncSink[E]{
		fn: f,
	}
}

type EventPersistor interface {
	// Persist appends the events in the order given, all of them or none. A
	// caller that writes two events to say one thing must not have one of
	// them survive without the other.
	Persist(ctx context.Context, evs ...*events.Event) ([]*events.Event, error)
}

type EventSource interface {
	GetEventsForArc(ctx context.Context, ref ArcRef) ([]*events.Event, error)
}

// EventSink is told about events once they have been written. It is the
// notification half of a write and never part of it.
//
// Consume returns nothing, so a sink cannot fail a write. That is the point
// rather than an oversight: a sink that could return an error would eventually
// have a caller that checked it, and a screen that had gone away would be able
// to end a run. An implementation that cares whether its publishes land says
// so through its own logging.
//
// It must also return promptly. It is called between a write and the next step
// of the agent loop, so an implementation that talks to something remote
// buffers internally and drops rather than blocking.
//
// A subscriber is therefore owed nothing. Every event carries its session, its
// arc and its sequence number, so a gap is the subscriber's cue to read the
// log again rather than this sink's cue to retry.
type EventSink interface {
	Consume(ctx context.Context, evs ...*events.Event)
}

// PublishEvents hands every batch that was written to a sink.
//
// It goes outermost of the persistor chain, so a write refused further in
// publishes nothing, and it publishes the events as they were stored, sequence
// numbers and all. A nil sink leaves the chain alone.
//
// Notification hangs here rather than off the agent because the persistor is
// what every writer has in common. A user's message, a cancel, what a task
// reports and what the agent records all pass through one of these, and an
// interface following only the agent would miss the first three.
func PublishEvents(persistor EventPersistor, sink EventSink) EventPersistor {
	if sink == nil {
		return persistor
	}
	return &publishingPersistor{persistor: persistor, sink: sink}
}

type publishingPersistor struct {
	persistor EventPersistor
	sink      EventSink
}

func (p *publishingPersistor) Persist(ctx context.Context, evs ...*events.Event) ([]*events.Event, error) {
	stored, err := p.persistor.Persist(ctx, evs...)
	if err != nil {
		return nil, err
	}
	// The batch goes whole because it was written whole: a subscriber handed a
	// reply without the calls beside it reads an answer the model never gave.
	// Synchronously, because a goroutine per batch would reorder them.
	p.sink.Consume(ctx, stored...)
	return stored, nil
}

// MintIdentity gives an event an ID and a timestamp. Every write goes through
// one of these, because a store is left to assign the sequence number and
// nothing else.
//
// It does not give an event a session. Every writer names the session it
// writes for, since nothing in the chain is tied to one session any more, and
// an event that arrives without one is a bug in the writer rather than
// something to fill in.
//
// The timestamp is taken here rather than at the append, so two events can
// carry times in the opposite order to their sequence numbers. The sequence
// number is the order.
func MintIdentity(persistor EventPersistor) EventPersistor {
	return &mintingPersistor{persistor: persistor}
}

type mintingPersistor struct {
	persistor EventPersistor
}

func (m *mintingPersistor) Persist(ctx context.Context, evs ...*events.Event) ([]*events.Event, error) {
	minted := make([]*events.Event, 0, len(evs))
	for _, ev := range evs {
		if ev.SessionID == "" {
			return nil, fmt.Errorf("%s event names no session", ev.Typ)
		}
		// The caller's event is left alone, since they may still be holding it.
		one := *ev
		// An ID or a time already set is kept, which is how a caller ties two
		// events together before either is written. Uniqueness is not checked.
		if one.ID == "" {
			one.ID = NewEventID()
		}
		if one.Timestamp.IsZero() {
			one.Timestamp = time.Now().UTC()
		}
		minted = append(minted, &one)
	}
	return m.persistor.Persist(ctx, minted...)
}

// NewEventID mints an identifier for an event.
func NewEventID() string {
	var buf [16]byte
	rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
