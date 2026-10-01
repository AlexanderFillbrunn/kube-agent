package agent

import (
	"context"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
)

// The session reaches the log through the persistor, so an event written with
// none of its own comes back filed under the session it was written for.
func TestStampIdentityFilesEventsUnderItsSession(t *testing.T) {
	store := &fakePersistor{}
	persistor := StampIdentity(store, "session")

	arcID := testArcID
	written, err := persistor.Persist(context.Background(), &events.Event{
		Typ:           events.UserMessageEventType,
		CorrelationID: &arcID,
		Payload:       events.UserMessagePayload{Content: "what is running?"},
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	stored := written[0]
	if stored.SessionID != "session" {
		t.Errorf("stored under session %q, want %q", stored.SessionID, "session")
	}
	if got := store.persisted[0].SessionID; got != "session" {
		t.Errorf("the log was handed session %q, want %q", got, "session")
	}
}

// The store is handed an event that already knows what it is, so a store with
// no opinion about IDs or clocks still writes a complete record.
func TestStampIdentityMintsAnIDAndATime(t *testing.T) {
	store := &fakePersistor{}
	persistor := StampIdentity(store, "session")

	before := time.Now().UTC()
	writtenFirst, err := persistor.Persist(context.Background(), &events.Event{
		Typ:     events.UserMessageEventType,
		Payload: events.UserMessagePayload{Content: "what is running?"},
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	first := writtenFirst[0]
	writtenSecond, err := persistor.Persist(context.Background(), &events.Event{
		Typ:     events.UserMessageEventType,
		Payload: events.UserMessagePayload{Content: "and now?"},
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	second := writtenSecond[0]

	if first.ID == "" || first.ID == second.ID {
		t.Errorf("minted IDs %q and %q, want two distinct ones", first.ID, second.ID)
	}
	if first.Timestamp.Before(before) || first.Timestamp.After(time.Now().UTC()) {
		t.Errorf("stamped %v, want a time from during the call", first.Timestamp)
	}
}

// An event that arrives knowing its own identity keeps it: that is how a
// caller ties two events together before either is written, and how a record
// of something that happened earlier keeps the time it happened.
func TestStampIdentityKeepsWhatTheCallerSet(t *testing.T) {
	persistor := StampIdentity(&fakePersistor{}, "session")

	at := time.Unix(1000, 0).UTC()
	written, err := persistor.Persist(context.Background(), &events.Event{
		Typ:       events.UserMessageEventType,
		ID:        "chosen-id",
		Timestamp: at,
		Payload:   events.UserMessagePayload{Content: "what is running?"},
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	stored := written[0]
	if stored.ID != "chosen-id" || !stored.Timestamp.Equal(at) {
		t.Errorf("stored as %q at %v, want the caller's id and time", stored.ID, stored.Timestamp)
	}
}

// A caller may still be holding the event it handed over, so the stamp lands
// on the copy that goes to the log and not on theirs.
func TestStampIdentityLeavesTheCallersEventAlone(t *testing.T) {
	persistor := StampIdentity(&fakePersistor{}, "session")

	original := &events.Event{
		Typ:     events.UserMessageEventType,
		Payload: events.UserMessagePayload{Content: "what is running?"},
	}
	if _, err := persistor.Persist(context.Background(), original); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if original.SessionID != "" || original.ID != "" || !original.Timestamp.IsZero() {
		t.Errorf("the caller's event was stamped: %+v", original)
	}
}
