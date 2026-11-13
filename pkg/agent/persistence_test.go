package agent

import (
	"context"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
)

// Nothing in the chain invents a session. A writer that did not name one has a
// bug, and filling it in would file the events somewhere plausible instead of
// saying so.
func TestMintIdentityRefusesAnEventWithNoSession(t *testing.T) {
	store := &fakePersistor{}
	persistor := MintIdentity(store)

	arcID := testArcID
	_, err := persistor.Persist(context.Background(), &events.Event{
		Typ:           events.UserMessageEventType,
		CorrelationID: &arcID,
		Payload:       events.UserMessagePayload{Content: "what is running?"},
	})
	if err == nil {
		t.Fatal("an event naming no session was accepted")
	}
	if len(store.persisted) != 0 {
		t.Errorf("the log was written to anyway: %d events", len(store.persisted))
	}
}

// The session the writer named is the one the log is handed.
func TestMintIdentityKeepsTheWritersSession(t *testing.T) {
	store := &fakePersistor{}
	persistor := MintIdentity(store)

	arcID := testArcID
	written, err := persistor.Persist(context.Background(), &events.Event{
		SessionID:     testSessionID,
		AgentID:       testAgentID,
		Typ:           events.UserMessageEventType,
		CorrelationID: &arcID,
		Payload:       events.UserMessagePayload{Content: "what is running?"},
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if written[0].SessionID != testSessionID {
		t.Errorf("stored under session %q, want %q", written[0].SessionID, testSessionID)
	}
	if got := store.persisted[0].SessionID; got != testSessionID {
		t.Errorf("the log was handed session %q, want %q", got, testSessionID)
	}
}

// The store is handed an event that already knows what it is, so a store with
// no opinion about IDs or clocks still writes a complete record.
func TestMintIdentityMintsAnIDAndATime(t *testing.T) {
	store := &fakePersistor{}
	persistor := MintIdentity(store)

	before := time.Now().UTC()
	writtenFirst, err := persistor.Persist(context.Background(), &events.Event{
		SessionID: testSessionID,
		AgentID:   testAgentID,
		Typ:       events.UserMessageEventType,
		Payload:   events.UserMessagePayload{Content: "what is running?"},
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	first := writtenFirst[0]
	writtenSecond, err := persistor.Persist(context.Background(), &events.Event{
		SessionID: testSessionID,
		AgentID:   testAgentID,
		Typ:       events.UserMessageEventType,
		Payload:   events.UserMessagePayload{Content: "and now?"},
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
func TestMintIdentityKeepsWhatTheCallerSet(t *testing.T) {
	persistor := MintIdentity(&fakePersistor{})

	at := time.Unix(1000, 0).UTC()
	written, err := persistor.Persist(context.Background(), &events.Event{
		SessionID: testSessionID,
		AgentID:   testAgentID,
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
func TestMintIdentityLeavesTheCallersEventAlone(t *testing.T) {
	persistor := MintIdentity(&fakePersistor{})

	original := &events.Event{
		SessionID: testSessionID,
		AgentID:   testAgentID,
		Typ:       events.UserMessageEventType,
		Payload:   events.UserMessagePayload{Content: "what is running?"},
	}
	if _, err := persistor.Persist(context.Background(), original); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if original.ID != "" || !original.Timestamp.IsZero() {
		t.Errorf("the caller's event was stamped: %+v", original)
	}
}

// recordingSink keeps what it was published, in the batches it arrived in.
type recordingSink struct {
	batches [][]*events.Event
}

func (s *recordingSink) Consume(_ context.Context, evs ...*events.Event) {
	s.batches = append(s.batches, evs)
}

// A subscriber is handed the events as they were stored, sequence numbers and
// all, since that number is how it knows what it missed.
func TestPublishEventsPublishesWhatWasStored(t *testing.T) {
	sink := &recordingSink{}
	persistor := PublishEvents(&fakePersistor{next: 7}, sink)

	arcID := testArcID
	if _, err := persistor.Persist(context.Background(),
		&events.Event{SessionID: testSessionID, Typ: events.AssistantMessageEventType, CorrelationID: &arcID,
			Payload: events.AssistantMessagePayload{Content: "checking"}},
		&events.Event{SessionID: testSessionID, Typ: events.ToolCallEventType, CorrelationID: &arcID,
			Payload: events.ToolCallPayload{MessageID: "m", ToolName: "list_pods"}},
	); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// One batch, because it was one write: a subscriber handed the reply
	// without the call beside it reads an answer the model never gave.
	if len(sink.batches) != 1 || len(sink.batches[0]) != 2 {
		t.Fatalf("published %d batches, want one of two events", len(sink.batches))
	}
	if sink.batches[0][0].Seq != 7 || sink.batches[0][1].Seq != 8 {
		t.Errorf("published seqs %d and %d, want the ones the store assigned",
			sink.batches[0][0].Seq, sink.batches[0][1].Seq)
	}
}

// A write that failed is not news.
func TestPublishEventsSaysNothingAboutAFailedWrite(t *testing.T) {
	sink := &recordingSink{}
	persistor := PublishEvents(failingPersistor{}, sink)

	arcID := testArcID
	if _, err := persistor.Persist(context.Background(), &events.Event{
		SessionID: testSessionID,
		AgentID:   testAgentID, Typ: events.ArcClosedEventType, CorrelationID: &arcID,
		Payload: events.ArcClosedPayload{CausationID: "m"},
	}); err == nil {
		t.Fatal("a failed write was reported as a success")
	}
	if len(sink.batches) != 0 {
		t.Errorf("published %d batches for a write that did not happen", len(sink.batches))
	}
}

// The agent writes through whatever persistor it is handed and has no notion
// of anybody watching, so a run publishes its whole log without being told to.
func TestARunPublishesEverythingItWrites(t *testing.T) {
	sink := &recordingSink{}
	store := &fakePersistor{next: 1}
	agent, err := NewAgent(nil, nil, &scriptedLLM{replies: []*LLMResponse{
		{ModelID: "gpt-5", Content: "two are pending"},
	}})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(context.Background(), RunOptions{
		Persistor: PublishEvents(store, sink),
		History:   h,
		ArcID:     testArcID,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var published []*events.Event
	for _, batch := range sink.batches {
		published = append(published, batch...)
	}
	if typesOf(published) != typesOf(store.persisted) {
		t.Errorf("published %s, wrote %s", typesOf(published), typesOf(store.persisted))
	}
}

func TestMintIdentityRefusesAnEventWithNoAgent(t *testing.T) {
	store := &fakePersistor{}
	persistor := MintIdentity(store)

	arcID := testArcID
	_, err := persistor.Persist(context.Background(), &events.Event{
		SessionID:     testSessionID,
		Typ:           events.UserMessageEventType,
		CorrelationID: &arcID,
		Payload:       events.UserMessagePayload{Content: "what is running?"},
	})
	if err == nil {
		t.Fatal("an event naming no agent was accepted")
	}
	if len(store.persisted) != 0 {
		t.Errorf("the log was written to anyway: %d events", len(store.persisted))
	}
}
