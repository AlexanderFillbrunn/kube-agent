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
	// Persist appends the events in the order given, all of them or none. A
	// caller that writes two events to say one thing must not have one of
	// them survive without the other.
	Persist(ctx context.Context, evs ...*events.Event) ([]*events.Event, error)
}

type EventSource interface {
	GetEventsForArc(ctx context.Context, id string) ([]*events.Event, error)
}

// StampIdentity gives an event the session it belongs to, an ID and a
// timestamp. The sequence number stays the store's to assign.
//
// The timestamp is taken here rather than at the append, so two events can
// carry times in the opposite order to their sequence numbers. The sequence
// number is the order.
func StampIdentity(persistor EventPersistor, sessionID string) EventPersistor {
	return &stampingPersistor{persistor: persistor, sessionID: sessionID}
}

type stampingPersistor struct {
	persistor EventPersistor
	sessionID string
}

func (s *stampingPersistor) Persist(ctx context.Context, evs ...*events.Event) ([]*events.Event, error) {
	stamped := make([]*events.Event, 0, len(evs))
	for _, ev := range evs {
		// The caller's event is left alone, since they may still be holding it.
		one := *ev
		one.SessionID = s.sessionID
		// An ID or a time already set is kept, which is how a caller ties two
		// events together before either is written. Uniqueness is not checked.
		if one.ID == "" {
			one.ID = NewEventID()
		}
		if one.Timestamp.IsZero() {
			one.Timestamp = time.Now().UTC()
		}
		stamped = append(stamped, &one)
	}
	return s.persistor.Persist(ctx, stamped...)
}

// NewEventID mints an identifier for an event.
func NewEventID() string {
	var buf [16]byte
	rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
