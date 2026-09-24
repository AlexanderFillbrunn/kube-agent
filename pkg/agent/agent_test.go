package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakePersistor stands in for the log, assigning the identity an event gets
// once it is written.
type fakePersistor struct {
	next      int
	persisted []*events.Event
}

func (p *fakePersistor) Persist(_ context.Context, ev *events.Event) (*events.Event, error) {
	stored := *ev
	stored.ID = fmt.Sprintf("ev-%d", p.next)
	stored.Seq = p.next
	stored.Timestamp = time.Unix(int64(p.next), 0)
	p.next++
	p.persisted = append(p.persisted, &stored)
	return &stored, nil
}

// A tool call names the message that requested it, and that name is the ID the
// persistor assigned, which is why the agent records the reply rather than the
// LLM.
func TestRecordResponseNamesThePersistedMessage(t *testing.T) {
	h := userArcHistory(t)
	persistor := &fakePersistor{next: 1}
	agent, err := NewAgent(nil, nil, persistor, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	state := &agentState{
		history:   h,
		arc:       h.Arcs[0],
		foldState: history.FoldStateFromHistory(h),
		sessionID: "session",
	}

	vendorID := "chatcmpl-9"
	response := &LLMResponse{
		VendorID: &vendorID,
		ModelID:  "gpt-5",
		Content:  "checking",
		Usage:    &TokenUsage{Input: 120, CachedInput: 64, Output: 18},
		ToolCalls: []LLMToolCall{{
			VendorID: "call_abc",
			ToolName: "list_pods",
			Args:     map[string]any{"namespace": "kube-system"},
		}},
	}
	if err := agent.recordResponse(context.Background(), response, state); err != nil {
		t.Fatalf("recordResponse: %v", err)
	}

	if len(persistor.persisted) != 2 {
		t.Fatalf("persisted %d events, want the message and its call", len(persistor.persisted))
	}
	message, call := persistor.persisted[0], persistor.persisted[1]
	if message.Typ != events.AssistantMessageEventType || call.Typ != events.ToolCallEventType {
		t.Fatalf("persisted %s then %s", message.Typ, call.Typ)
	}
	if got := call.Payload.(events.ToolCallPayload).MessageID; got != message.ID {
		t.Errorf("call names message %q, want %q", got, message.ID)
	}
	if message.SessionID != "session" || *message.CorrelationID != testArcID {
		t.Errorf("message filed under session %q arc %v", message.SessionID, message.CorrelationID)
	}

	// The fold is what the next step reads, so the reply has to have landed in
	// the history and not only in the log.
	folded := state.arc.Responses
	if len(folded) != 1 || len(folded[0].ToolCalls) != 1 {
		t.Fatalf("arc holds %d responses after recording one reply", len(folded))
	}
	if folded[0].ToolCalls[0].VendorID != "call_abc" {
		t.Errorf("folded call = %+v, want it under call_abc", folded[0].ToolCalls[0])
	}
	if folded[0].ToolCalls[0].Status != history.ToolCallStatusPending {
		t.Errorf("folded call is %s, want it pending for the tool step", folded[0].ToolCalls[0].Status)
	}

	// The cost has to reach both the log and the fold, or a total over the
	// session can only be had by replaying the raw events.
	want := events.TokenUsage{Input: 120, CachedInput: 64, Output: 18}
	if got := message.Payload.(events.AssistantMessagePayload).Usage; got == nil || *got != want {
		t.Errorf("persisted usage = %+v, want %+v", got, want)
	}
	if got := folded[0].Usage; got == nil || *got != want {
		t.Errorf("folded usage = %+v, want %+v", got, want)
	}
}

// scriptedLLM replies with a canned response per turn, or fails.
type scriptedLLM struct {
	replies []*LLMResponse
	failure error
	calls   int
}

func (l *scriptedLLM) StreamResponse(ctx context.Context, _ *history.History, _ *LLMOptions, _ Sink[string]) (*LLMResponse, error) {
	if l.failure != nil {
		l.calls++
		return nil, l.failure
	}
	if l.calls >= len(l.replies) {
		return nil, fmt.Errorf("the model was asked %d times, with %d replies scripted", l.calls+1, len(l.replies))
	}
	reply := l.replies[l.calls]
	l.calls++
	return reply, nil
}

func (l *scriptedLLM) Response(ctx context.Context, h *history.History, opts *LLMOptions) (*LLMResponse, error) {
	return l.StreamResponse(ctx, h, opts, nil)
}

// A call the model requests has to be run and settled, and the arc has to
// reach its close. An unsettled call is asked about forever, so this test
// hanging is the failure it guards.
func TestRunCallsToolsAndClosesTheArc(t *testing.T) {
	registry := registryWith(t, "list_pods", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "2 pending"}}}, nil
	})

	llm := &scriptedLLM{replies: []*LLMResponse{
		{
			ModelID: "gpt-5",
			Content: "checking",
			ToolCalls: []LLMToolCall{{
				VendorID: "call_abc",
				ToolName: "list_pods",
				Args:     map[string]any{"namespace": "kube-system"},
			}},
		},
		{ModelID: "gpt-5", Content: "two pods are pending"},
	}}

	agent, err := NewAgent(&AgentConfig{}, registry, &fakePersistor{next: 1}, llm)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(context.Background(), "session", h, testArcID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	arc := h.Arcs[0]
	if arc.Status() != history.ArcStatusDone {
		t.Errorf("arc is %s, want it closed", arc.Status())
	}
	if len(arc.Responses) != 2 {
		t.Fatalf("arc holds %d responses, want the tool turn and the answer", len(arc.Responses))
	}

	call := arc.Responses[0].ToolCalls[0]
	if call.Status != history.ToolCallStatusSuccess {
		t.Errorf("call is %s, want SUCCESS", call.Status)
	}
	if call.Result != "2 pending" {
		t.Errorf("call result = %#v, want what the tool returned", call.Result)
	}
	if call.Dispatches != 1 || call.DispatchedSeq == nil {
		t.Errorf("call was dispatched %d times, with seq %v", call.Dispatches, call.DispatchedSeq)
	}
	if call.FinishedSeq == nil || *call.FinishedSeq <= *call.DispatchedSeq {
		t.Error("the call settled before it was dispatched")
	}
}

// A tool that breaks settles its call as failed rather than ending the run,
// because the model has to be told in order to do something else.
func TestRunSettlesABrokenToolAsFailed(t *testing.T) {
	registry := registryWith(t, "list_pods", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("connection refused")
	})

	llm := &scriptedLLM{replies: []*LLMResponse{
		{ModelID: "gpt-5", ToolCalls: []LLMToolCall{{VendorID: "call_abc", ToolName: "list_pods"}}},
		{ModelID: "gpt-5", Content: "the cluster is unreachable"},
	}}

	agent, err := NewAgent(nil, registry, &fakePersistor{next: 1}, llm)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(context.Background(), "session", h, testArcID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	call := h.Arcs[0].Responses[0].ToolCalls[0]
	if call.Status != history.ToolCallStatusFailed {
		t.Errorf("call is %s, want FAILED", call.Status)
	}
	if h.Arcs[0].Status() != history.ArcStatusDone {
		t.Errorf("arc is %s, want it closed", h.Arcs[0].Status())
	}
}

func TestNewAgentRequiresAPersistorAndAModel(t *testing.T) {
	if _, err := NewAgent(nil, nil, nil, &scriptedLLM{}); err == nil {
		t.Error("an agent with no persistor was accepted")
	}
	if _, err := NewAgent(nil, nil, &fakePersistor{}, nil); err == nil {
		t.Error("an agent with no model was accepted")
	}
	agent, err := NewAgent(nil, nil, &fakePersistor{}, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if agent.tools == nil || agent.config == nil {
		t.Error("a defaulted agent is missing its registry or config")
	}
}

// runFailing runs one arc against a model that fails, and returns the history
// it left behind.
func runFailing(t *testing.T, failure error) *history.History {
	t.Helper()
	agent, err := NewAgent(nil, nil, &fakePersistor{next: 1}, &scriptedLLM{failure: failure})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(context.Background(), "session", h, testArcID); !errors.Is(err, failure) {
		t.Fatalf("Run returned %v, want the failure it hit", err)
	}
	return h
}

// A run that stops has to say so. An arc left open is one the log cannot tell
// from work still in progress, and every later request is projected against
// that.
func TestAFailedRunRecordsAFatalError(t *testing.T) {
	failure := errors.New("model not found")
	arc := runFailing(t, failure).Arcs[0]

	if arc.Status() != history.ArcStatusError {
		t.Errorf("arc is %s, want ERROR", arc.Status())
	}
	if len(arc.Errors) != 1 {
		t.Fatalf("arc holds %d errors, want the one that ended it", len(arc.Errors))
	}
	if !arc.Errors[0].IsFatal {
		t.Error("the error that ended the arc is not fatal")
	}
	if !strings.Contains(arc.Errors[0].Message, "model not found") {
		t.Errorf("recorded %q, want what went wrong", arc.Errors[0].Message)
	}
}

// The point of recording it: what the model is told about the dead arc next
// time has to be true.
func TestALaterTurnIsToldTheArcFailed(t *testing.T) {
	h := runFailing(t, errors.New("model not found"))

	// A second question opens its own arc, and the failed one is foreign to it.
	next := "arc-2"
	actor := users.Actor{UserID: "alice", TenantID: "acme"}
	if err := h.Update(&events.Event{
		SessionID: "session", ID: next, Seq: h.LastSeq + 1, Timestamp: time.Unix(9, 0),
		Typ: events.UserMessageEventType, CorrelationID: &next, Actor: actor,
		Payload: events.UserMessagePayload{Content: "what about now?", Actor: actor},
	}, history.FoldStateFromHistory(h)); err != nil {
		t.Fatalf("fold the next question: %v", err)
	}

	transcript, err := h.Project(next, history.DefaultModes())
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	var notes []string
	for _, message := range transcript.Messages {
		if note, ok := message.(history.ModelNoteMessage); ok {
			notes = append(notes, note.Content)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("the transcript holds notes %v, want one about the failed arc", notes)
	}
	if !strings.Contains(notes[0], "failed and was never answered") {
		t.Errorf("the model is told %q, want that the arc failed", notes[0])
	}
	if strings.Contains(notes[0], "still being worked on") {
		t.Error("the model is told a dead arc is still in flight")
	}
}

// Cancelling is not the agent's decision, so it is not the agent's to record.
// Whoever decided it writes the cancel and then stops the run; the run says
// what stopped it and writes nothing.
func TestACancelledRunRecordsNothing(t *testing.T) {
	persistor := &fakePersistor{next: 1}
	agent, err := NewAgent(nil, nil, persistor,
		&scriptedLLM{failure: fmt.Errorf("calling the model: %w", context.Canceled)})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(context.Background(), "session", h, testArcID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want an error carrying the cancellation", err)
	}

	if len(persistor.persisted) != 0 {
		t.Errorf("a cancelled run wrote %s", typesOf(persistor.persisted))
	}
	arc := h.Arcs[0]
	if arc.Cancel != nil {
		t.Error("the agent recorded a cancel that was not its to record")
	}
	if len(arc.Errors) != 0 {
		t.Errorf("a cancelled run was recorded as %d failures", len(arc.Errors))
	}
}

func typesOf(stored []*events.Event) string {
	names := make([]string, 0, len(stored))
	for _, ev := range stored {
		names = append(names, string(ev.Typ))
	}
	return strings.Join(names, ", ")
}

// A store that cannot take the record must not swallow what went wrong.
func TestAnUnrecordableFailureKeepsItsCause(t *testing.T) {
	failure := errors.New("model not found")
	persistor := &brokenPersistor{fakePersistor: fakePersistor{next: 1}}
	agent, err := NewAgent(nil, nil, persistor, &scriptedLLM{failure: failure})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	err = agent.Run(context.Background(), "session", userArcHistory(t), testArcID)
	if !errors.Is(err, failure) {
		t.Errorf("Run returned %v, want the cause it started with", err)
	}
	if !strings.Contains(err.Error(), "the log is full") {
		t.Errorf("Run returned %v, want it to also say the record failed", err)
	}
}

// brokenPersistor refuses everything, which is what a store that has given up
// looks like to the agent.
type brokenPersistor struct {
	fakePersistor
}

func (p *brokenPersistor) Persist(context.Context, *events.Event) (*events.Event, error) {
	return nil, errors.New("the log is full")
}
