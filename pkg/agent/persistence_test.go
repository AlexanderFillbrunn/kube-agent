package agent

import (
	"context"
	"testing"

	"fillbrunn.ai/kube-agent/pkg/events"
)

// The session reaches the log through the persistor, so an event written with
// none of its own comes back filed under the session it was written for.
func TestStampSessionFilesEventsUnderItsSession(t *testing.T) {
	store := &fakePersistor{}
	persistor := StampSession(store, "session")

	arcID := testArcID
	stored, err := persistor.Persist(context.Background(), &events.Event{
		Typ:           events.UserMessageEventType,
		CorrelationID: &arcID,
		Payload:       events.UserMessagePayload{Content: "what is running?"},
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if stored.SessionID != "session" {
		t.Errorf("stored under session %q, want %q", stored.SessionID, "session")
	}
	if got := store.persisted[0].SessionID; got != "session" {
		t.Errorf("the log was handed session %q, want %q", got, "session")
	}
}

// A caller may still be holding the event it handed over, so the stamp lands
// on the copy that goes to the log and not on theirs.
func TestStampSessionLeavesTheCallersEventAlone(t *testing.T) {
	persistor := StampSession(&fakePersistor{}, "session")

	original := &events.Event{
		Typ:     events.UserMessageEventType,
		Payload: events.UserMessagePayload{Content: "what is running?"},
	}
	if _, err := persistor.Persist(context.Background(), original); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if original.SessionID != "" {
		t.Errorf("the caller's event was stamped %q", original.SessionID)
	}
}
