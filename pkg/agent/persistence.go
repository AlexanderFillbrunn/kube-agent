package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

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

// StampIdentity gives an event the identity it is filed under: the session it
// belongs to, an ID, and the time it happened.
//
// None of the three needs to know anything about the log, so none of them is
// the store's to invent: a store that minted them would be reimplementing the
// same three lines, and differently. What a store does own is the sequence
// number, which is the one part of an identity that is a fact about the log
// rather than about the event, and which only the writer holding the log can
// assign.
//
// The time is taken here rather than at the append, so it reads as when the
// agent did the thing rather than when the write reached disk. A write that
// waits behind something can therefore carry a timestamp slightly before an
// event with a lower sequence number; where the two disagree, the sequence
// number is the order.
func StampIdentity(persistor EventPersistor, sessionID string) EventPersistor {
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
	// An ID or a time already set is kept, which is how a caller ties two
	// events together before either is written, or replays a record of
	// something that happened earlier. Nothing here checks such an ID is
	// unique.
	if stamped.ID == "" {
		stamped.ID = NewEventID()
	}
	if stamped.Timestamp.IsZero() {
		stamped.Timestamp = time.Now().UTC()
	}
	return s.persistor.Persist(ctx, &stamped)
}

// NewEventID mints an identifier for an event.
func NewEventID() string {
	var buf [16]byte
	rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
