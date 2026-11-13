package agent

import (
	"context"
	"errors"
	"testing"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
)

// streamingLLM writes a reply a piece at a time, the way a provider does.
type streamingLLM struct {
	pieces []string
	reply  *LLMResponse
}

func (l *streamingLLM) StreamResponse(_ context.Context, _ *history.History, _ *LLMOptions, tokens Sink[RawToken]) (*LLMResponse, error) {
	for i, piece := range l.pieces {
		if err := tokens.Consume(RawToken{Content: piece, Seq: i}); err != nil {
			return nil, err
		}
	}
	return l.reply, nil
}

func (l *streamingLLM) Response(ctx context.Context, h *history.History, opts *LLMOptions) (*LLMResponse, error) {
	return l.StreamResponse(ctx, h, opts, SinkFromFunc(func(RawToken) error { return nil }))
}

// A run nobody is watching streams into nothing and records the same log, so a
// worker with no interface attached does not take a different path through the
// agent from one that has.
func TestARunWithNoTokenSinkRecordsTheSameLog(t *testing.T) {
	log := func(tokens Sink[Token]) string {
		t.Helper()
		store := &fakePersistor{next: 1}
		agent, err := NewAgent(nil, nil, &streamingLLM{
			pieces: []string{"two ", "are ", "pending"},
			reply:  &LLMResponse{ModelID: "gpt-5", Content: "two are pending"},
		})
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}
		if err := agent.Run(context.Background(), RunOptions{
			Persistor: store, History: userArcHistory(t), ArcID: testArcID, Tokens: tokens,
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return typesOf(store.persisted)
	}

	watched := log(SinkFromFunc(func(Token) error { return nil }))
	if unwatched := log(nil); unwatched != watched {
		t.Errorf("unwatched run wrote %s, watched run wrote %s", unwatched, watched)
	}
}

// Every token names the message that will record it, and it is the message the
// reply is actually written under: the ID is minted before the model is called
// so that both can carry it.
func TestStreamedTokensNameTheMessageThatRecordsThem(t *testing.T) {
	store := &fakePersistor{next: 1}
	agent, err := NewAgent(nil, nil, &streamingLLM{
		pieces: []string{"two ", "are ", "pending"},
		reply:  &LLMResponse{ModelID: "gpt-5", Content: "two are pending"},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	var streamed []Token
	if err := agent.Run(context.Background(), RunOptions{
		Persistor: store,
		History:   userArcHistory(t),
		ArcID:     testArcID,
		Tokens: SinkFromFunc(func(tok Token) error {
			streamed = append(streamed, tok)
			return nil
		}),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(streamed) != 3 {
		t.Fatalf("streamed %d tokens, want the three the model wrote", len(streamed))
	}

	var recorded string
	for _, ev := range store.persisted {
		if ev.Typ == events.AssistantMessageEventType {
			recorded = ev.ID
			break
		}
	}
	if recorded == "" {
		t.Fatal("the reply was never recorded")
	}

	var text string
	for _, tok := range streamed {
		if tok.MessageID != recorded {
			t.Fatalf("token %q names message %q, want %q", tok.Content, tok.MessageID, recorded)
		}
		text += tok.Content
	}
	if text != "two are pending" {
		t.Errorf("streamed %q, want the reply as it was written", text)
	}
}

// A watcher that fails cannot end a run. A screen going away is not a reason
// to stop working, and the reply is recorded whole either way.
func TestAFailingTokenSinkDoesNotFailTheRun(t *testing.T) {
	store := &fakePersistor{next: 1}
	agent, err := NewAgent(nil, nil, &streamingLLM{
		pieces: []string{"two ", "are ", "pending"},
		reply:  &LLMResponse{ModelID: "gpt-5", Content: "two are pending"},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(context.Background(), RunOptions{
		Persistor: store, History: h, ArcID: testArcID,
		Tokens: SinkFromFunc(func(Token) error { return errors.New("the watcher is gone") }),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.Arcs[0].Status() != history.ArcStatusDone {
		t.Errorf("arc is %s, want it closed", h.Arcs[0].Status())
	}
}

// collectedTokens is a TokenSink that keeps what it was handed.
type collectedTokens struct{ tokens []StreamToken }

func (c *collectedTokens) Token(_ context.Context, tok StreamToken) {
	c.tokens = append(c.tokens, tok)
}

// The agent knows the message and the worker knows the arc, so what reaches a
// subscriber is addressed well enough to route from another process.
func TestAWorkerAddressesTheTokensItStreams(t *testing.T) {
	store := runningTaskStore(t)
	agent, err := NewAgent(nil, nil, &streamingLLM{
		pieces: []string{"the audit ", "found 3"},
		reply:  &LLMResponse{ModelID: "gpt-5", Content: "the audit found 3"},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// The task is settled first, since an arc with one still running has no
	// turn for the model to take.
	if _, err := store.Persist(context.Background(), &events.Event{
		SessionID: testSessionID, Typ: events.AsyncTaskResultEventType, CorrelationID: ptr(testArcID),
		Payload: events.AsyncTaskResultPayload{
			CausationID: "task-1", Result: "3 findings", Outcome: events.AsyncTaskOutcomeSuccess},
	}); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	collected := &collectedTokens{}
	worker, err := NewArcWorker(store, store, NewFixedResolver(agent), nil, WithTokenSink(collected))
	if err != nil {
		t.Fatalf("NewArcWorker: %v", err)
	}
	if _, err := worker.Work(context.Background(), testRef(), testAgentID); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if len(collected.tokens) != 2 {
		t.Fatalf("collected %d tokens, want the two the model wrote", len(collected.tokens))
	}
	for _, tok := range collected.tokens {
		if tok.SessionID != testSessionID || tok.ArcID != testArcID {
			t.Errorf("token %q came from %s/%s, want %s", tok.Content, tok.SessionID, tok.ArcID, testRef())
		}
		if tok.MessageID == "" {
			t.Errorf("token %q names no message", tok.Content)
		}
	}
}

func ptr[T any](v T) *T { return &v }
