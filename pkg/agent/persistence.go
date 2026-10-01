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

func (s *stampingPersistor) Persist(ctx context.Context, ev *events.Event) (*events.Event, error) {
	// The caller's event is left alone, since they may still be holding it.
	stamped := *ev
	stamped.SessionID = s.sessionID
	// An ID or a time already set is kept, which is how a caller ties two
	// events together before either is written. Uniqueness is not checked.
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
