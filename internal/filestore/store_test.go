package filestore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/users"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// userMessage opens an arc.
func userMessage(arcID, content string) *events.Event {
	actor := users.Actor{UserID: "dev", TenantID: "dev"}
	return &events.Event{
		SessionID:     "session",
		Typ:           events.UserMessageEventType,
		CorrelationID: &arcID,
		Actor:         actor,
		Payload:       events.UserMessagePayload{Content: content, Actor: actor},
	}
}

func assistantMessage(arcID, content string) *events.Event {
	return &events.Event{
		SessionID:     "session",
		Typ:           events.AssistantMessageEventType,
		CorrelationID: &arcID,
		Payload:       events.AssistantMessagePayload{ModelID: "gpt-5", Content: content},
	}
}

func TestPersistAssignsIdentityFromZero(t *testing.T) {
	store := openTemp(t)

	writtenFirst, err := store.Persist(context.Background(), userMessage("arc-1", "hello"))
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	first := writtenFirst[0]
	if first.Seq != 0 {
		t.Errorf("first event is seq %d, want 0", first.Seq)
	}
	if first.ID == "" || first.Timestamp.IsZero() {
		t.Errorf("event stored without an identity: %+v", first)
	}

	writtenSecond, err := store.Persist(context.Background(), assistantMessage("arc-1", "hi"))
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	second := writtenSecond[0]
	if second.Seq != 1 {
		t.Errorf("second event is seq %d, want 1", second.Seq)
	}
	if second.ID == first.ID {
		t.Error("two events share an ID")
	}
}

// A reopened log carries on where it stopped, or the fold rejects the next
// event for going backwards.
func TestOpenResumesTheSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, ev := range []*events.Event{userMessage("arc-1", "hello"), assistantMessage("arc-1", "hi")} {
		if _, err := first.Persist(context.Background(), ev); err != nil {
			t.Fatalf("Persist: %v", err)
		}
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	written, err := second.Persist(context.Background(), userMessage("arc-2", "still there?"))
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	next := written[0]
	if next.Seq != 2 {
		t.Errorf("resumed at seq %d, want 2", next.Seq)
	}

	stored, err := second.Events(context.Background())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(stored) != 3 {
		t.Fatalf("log holds %d events, want 3", len(stored))
	}
	if payload, ok := stored[0].Payload.(events.UserMessagePayload); !ok || payload.Content != "hello" {
		t.Errorf("first event decoded as %#v", stored[0].Payload)
	}
}

// The cutoff is the contract the projection rests on: everything up to the
// arc's own user message, plus everything correlated to it afterwards, and
// nothing else.
func TestGetEventsForArcAppliesTheCutoff(t *testing.T) {
	store := openTemp(t)
	ctx := context.Background()

	for _, ev := range []*events.Event{
		userMessage("arc-1", "first question"),     // seq 0
		assistantMessage("arc-1", "first answer"),  // seq 1
		userMessage("arc-2", "second question"),    // seq 2
		assistantMessage("arc-2", "second answer"), // seq 3
	} {
		if _, err := store.Persist(ctx, ev); err != nil {
			t.Fatalf("Persist: %v", err)
		}
	}
	// An arc can be added to after a later arc opened, and that record still
	// belongs to it.
	if _, err := store.Persist(ctx, assistantMessage("arc-1", "late addition")); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	selected, err := store.GetEventsForArc(ctx, "arc-1")
	if err != nil {
		t.Fatalf("GetEventsForArc: %v", err)
	}

	var seqs []int
	for _, ev := range selected {
		seqs = append(seqs, ev.Seq)
	}
	// 0 and 1 are the arc's own; 2 and 3 belong to arc-2 and fall after the
	// cutoff; 4 is correlated to arc-1 however late it arrived.
	want := []int{0, 1, 4}
	if len(seqs) != len(want) {
		t.Fatalf("selected seqs %v, want %v", seqs, want)
	}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("selected seqs %v, want %v", seqs, want)
		}
	}

	// A later arc sees the earlier one whole, because it is conversation
	// history by then.
	selected, err = store.GetEventsForArc(ctx, "arc-2")
	if err != nil {
		t.Fatalf("GetEventsForArc: %v", err)
	}
	if len(selected) != 4 {
		t.Errorf("arc-2 selected %d events, want the 3 before it plus its own reply", len(selected))
	}
}

func TestGetEventsForArcRejectsAnUnknownArc(t *testing.T) {
	store := openTemp(t)
	if _, err := store.GetEventsForArc(context.Background(), "absent"); err == nil {
		t.Error("an unknown arc was accepted")
	}
}

// Sessions are kept in a directory of their own, and the first run is the one
// that has to make it.
func TestOpenCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions", "log.jsonl")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if _, err := store.Persist(context.Background(), userMessage("arc-1", "hello")); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the log was not written: %v", err)
	}
}

// A log that is not there yet is an empty one, since a session starts by
// writing to it.
func TestOpenToleratesAMissingFile(t *testing.T) {
	store := openTemp(t)
	stored, err := store.Events(context.Background())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("a fresh log holds %d events", len(stored))
	}
}
