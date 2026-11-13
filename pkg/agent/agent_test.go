package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakePersistor stands in for the log. Like a real store it owns the sequence
// number, leaves an ID or a time the caller already set, and is safe to write
// to from more than one run at once.
type fakePersistor struct {
	mu        sync.Mutex
	next      int
	persisted []*events.Event
}

func (p *fakePersistor) Persist(_ context.Context, evs ...*events.Event) ([]*events.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stored := make([]*events.Event, 0, len(evs))
	for _, ev := range evs {
		one := *ev
		one.Seq = p.next
		if one.ID == "" {
			one.ID = fmt.Sprintf("ev-%d", p.next)
		}
		if one.Timestamp.IsZero() {
			one.Timestamp = time.Unix(int64(p.next), 0)
		}
		p.next++
		p.persisted = append(p.persisted, &one)
		stored = append(stored, &one)
	}
	return stored, nil
}

// failingPersistor refuses every write.
type failingPersistor struct{}

func (failingPersistor) Persist(context.Context, ...*events.Event) ([]*events.Event, error) {
	return nil, errors.New("the log is unreachable")
}

// A tool call names the message that requested it, and that name is the ID the
// persistor assigned, which is why the agent records the reply rather than the
// LLM.
func TestRecordResponseNamesThePersistedMessage(t *testing.T) {
	h := userArcHistory(t)
	persistor := &fakePersistor{next: 1}
	agent, err := NewAgent(nil, nil, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	state := &agentState{
		history:   h,
		arc:       h.Arcs[0],
		foldState: history.FoldStateFromHistory(h),
		persistor: persistor,
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
	if err := agent.recordResponse(context.Background(), response, "message-1", state); err != nil {
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
	if *message.CorrelationID != testArcID {
		t.Errorf("message filed under arc %v, want %s", message.CorrelationID, testArcID)
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

func (l *scriptedLLM) StreamResponse(ctx context.Context, _ *history.History, _ *LLMOptions, _ Sink[RawToken]) (*LLMResponse, error) {
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

	persistor := &fakePersistor{next: 1}
	agent, err := NewAgent(&AgentConfig{}, registry, llm)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); err != nil {
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

	persistor := &fakePersistor{next: 1}
	agent, err := NewAgent(nil, registry, llm)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); err != nil {
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

func TestNewAgentRequiresAModel(t *testing.T) {
	if _, err := NewAgent(nil, nil, nil); err == nil {
		t.Error("an agent with no model was accepted")
	}
	agent, err := NewAgent(nil, nil, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if agent.tools == nil || agent.config == nil {
		t.Error("a defaulted agent is missing its registry or config")
	}
}

// A run with nowhere to write is stopped before the model is called, since
// everything it decided would be lost the moment it decided it.
func TestRunRequiresAPersistor(t *testing.T) {
	llm := &scriptedLLM{}
	agent, err := NewAgent(nil, nil, llm)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := agent.Run(context.Background(), RunOptions{Persistor: nil, History: userArcHistory(t), ArcID: testArcID}); err == nil {
		t.Error("a run with no persistor was accepted")
	}
	if llm.calls != 0 {
		t.Error("the model was called for a run that had nowhere to record it")
	}
}

// runFailing runs one arc against a model that fails, and returns the history
// it left behind.
func runFailing(t *testing.T, failure error) *history.History {
	t.Helper()
	persistor := &fakePersistor{next: 1}
	agent, err := NewAgent(nil, nil, &scriptedLLM{failure: failure})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); !errors.Is(err, failure) {
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
	agent, err := NewAgent(nil, nil,
		&scriptedLLM{failure: fmt.Errorf("calling the model: %w", context.Canceled)})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); !errors.Is(err, context.Canceled) {
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
	agent, err := NewAgent(nil, nil, &scriptedLLM{failure: failure})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	err = agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: persistor, History: userArcHistory(t), ArcID: testArcID})
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

func (p *brokenPersistor) Persist(context.Context, ...*events.Event) ([]*events.Event, error) {
	return nil, errors.New("the log is full")
}

// A tool call is synchronous, so a handler that returned has said what
// happened — including when the run was cancelled out from under it. Dropping
// that would leave the call looking like one nobody ever heard back from.
func TestAHandlerThatReturnsUnderCancellationIsStillRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	registry := registryWith(t, "restart_deployment", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cancel()
		return nil, errors.New("the API call was interrupted")
	})

	persistor := &fakePersistor{next: 1}
	agent, err := NewAgent(nil, registry, &scriptedLLM{replies: []*LLMResponse{
		{ModelID: "gpt-5", ToolCalls: []LLMToolCall{{VendorID: "call_abc", ToolName: "restart_deployment"}}},
	}})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(ctx, RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want the cancellation", err)
	}

	want := "ASSISTANT_MESSAGE, TOOL_CALL, TOOL_DISPATCHED, TOOL_RESULT"
	if got := typesOf(persistor.persisted); got != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}

	call := h.Arcs[0].Responses[0].ToolCalls[0]
	if call.Status != history.ToolCallStatusFailed {
		t.Errorf("the call is %s, want it settled as failed", call.Status)
	}
}

// contextPersistor refuses a write on a cancelled context, the way a store that
// honours the context it is handed does and fakePersistor does not.
type contextPersistor struct {
	fakePersistor
}

func (p *contextPersistor) Persist(ctx context.Context, evs ...*events.Event) ([]*events.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.fakePersistor.Persist(ctx, evs...)
}

// The record of what a tool did outlives the context it ran under. Written on
// the run's own context, the one write that says what happened is the write a
// store refuses.
func TestAToolResultIsWrittenAfterTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	registry := registryWith(t, "restart_deployment", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cancel()
		return nil, errors.New("the API call was interrupted")
	})

	persistor := &contextPersistor{fakePersistor: fakePersistor{next: 1}}
	agent, err := NewAgent(nil, registry, &scriptedLLM{replies: []*LLMResponse{
		{ModelID: "gpt-5", ToolCalls: []LLMToolCall{{VendorID: "call_abc", ToolName: "restart_deployment"}}},
	}})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(ctx, RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want the cancellation", err)
	}

	want := "ASSISTANT_MESSAGE, TOOL_CALL, TOOL_DISPATCHED, TOOL_RESULT"
	if got := typesOf(persistor.persisted); got != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}
}

// batchPersistor remembers how the agent grouped its writes.
type batchPersistor struct {
	fakePersistor
	batches [][]*events.Event
}

func (p *batchPersistor) Persist(ctx context.Context, evs ...*events.Event) ([]*events.Event, error) {
	p.batches = append(p.batches, evs)
	return p.fakePersistor.Persist(ctx, evs...)
}

// A reply and the calls it requested reach the log as one write. Written
// separately, a failure between them leaves a message with no calls, which the
// next run reads as an answer and closes the arc on.
func TestAReplyAndItsCallsAreOneWrite(t *testing.T) {
	persistor := &batchPersistor{fakePersistor: fakePersistor{next: 1}}
	agent, err := NewAgent(nil, nil, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	h := userArcHistory(t)
	state := &agentState{
		history:   h,
		arc:       h.Arcs[0],
		foldState: history.FoldStateFromHistory(h),
		persistor: persistor,
	}

	response := &LLMResponse{ModelID: "gpt-5", Content: "restarting", ToolCalls: []LLMToolCall{
		{VendorID: "call_a", ToolName: "restart_deployment"},
		{VendorID: "call_b", ToolName: "list_pods"},
	}}
	if err := agent.recordResponse(context.Background(), response, "message-1", state); err != nil {
		t.Fatalf("recordResponse: %v", err)
	}

	if len(persistor.batches) != 1 {
		t.Fatalf("the reply took %d writes, want one", len(persistor.batches))
	}
	if got := typesOf(persistor.batches[0]); got != "ASSISTANT_MESSAGE, TOOL_CALL, TOOL_CALL" {
		t.Fatalf("the write held %s", got)
	}

	// The calls name the message although nothing had been written when they
	// were built.
	message := persistor.batches[0][0]
	for _, call := range persistor.batches[0][1:] {
		if got := call.Payload.(events.ToolCallPayload).MessageID; got != message.ID {
			t.Errorf("call names message %q, want %q", got, message.ID)
		}
	}
}

// cancellingLLM answers and then cancels the run, which is the window between
// the model having spoken and the log having heard it.
type cancellingLLM struct {
	reply  *LLMResponse
	cancel context.CancelFunc
}

func (l *cancellingLLM) StreamResponse(_ context.Context, _ *history.History, _ *LLMOptions, _ Sink[RawToken]) (*LLMResponse, error) {
	l.cancel()
	return l.reply, nil
}

func (l *cancellingLLM) Response(context.Context, *history.History, *LLMOptions) (*LLMResponse, error) {
	l.cancel()
	return l.reply, nil
}

// A reply and the calls it requested are one record written twice. A cancel
// landing between them would leave a reply the arc reads as a final answer,
// with the calls it asked for missing. The dispatch that would have followed
// is refused, because that one starts work.
func TestAReplyKeepsItsCallsWhenTheRunIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	llm := &cancellingLLM{cancel: cancel, reply: &LLMResponse{
		ModelID:   "gpt-5",
		Content:   "restarting",
		ToolCalls: []LLMToolCall{{VendorID: "call_abc", ToolName: "restart_deployment"}},
	}}

	registry := registryWith(t, "restart_deployment", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		t.Error("the tool ran although the run was cancelled before it was dispatched")
		return nil, nil
	})

	persistor := &contextPersistor{fakePersistor: fakePersistor{next: 1}}
	agent, err := NewAgent(nil, registry, llm)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	if err := agent.Run(ctx, RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want the cancellation", err)
	}

	want := "ASSISTANT_MESSAGE, TOOL_CALL"
	if got := typesOf(persistor.persisted); got != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}
}

// abandonedArc is an arc whose call reached its tool and whose run then
// stopped without reporting: what a dead worker leaves behind.
func abandonedArc(t *testing.T) *history.History {
	t.Helper()
	arc := testArcID
	actor := users.Actor{UserID: "alice", TenantID: "acme"}
	at := time.Unix(0, 0)

	h, err := history.Replay("session", []events.Event{
		{ID: arc, Seq: 0, Timestamp: at, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: actor,
			Payload: events.UserMessagePayload{Content: "restart the ingress controller", Actor: actor}},
		{ID: "msg-1", Seq: 1, Timestamp: at, Typ: events.AssistantMessageEventType, CorrelationID: &arc,
			Payload: events.AssistantMessagePayload{ModelID: "gpt-5", Content: "restarting"}},
		{ID: "tc-1", Seq: 2, Timestamp: at, Typ: events.ToolCallEventType, CorrelationID: &arc,
			Payload: events.ToolCallPayload{MessageID: "msg-1", VendorID: "call_abc", ToolName: "restart_deployment"}},
		{ID: "td-1", Seq: 3, Timestamp: at, Typ: events.ToolDispatchedEventType, CorrelationID: &arc,
			Payload: events.ToolDispatchedPayload{CausationID: "tc-1"}},
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return h
}

// recoverAbandoned picks the arc up again with a tool carrying the given
// annotations, and reports whether the tool ran.
func recoverAbandoned(t *testing.T, annotations *mcp.ToolAnnotations) (*history.ToolCall, bool) {
	t.Helper()
	var ran bool
	registry := NewToolRegistry()
	err := registry.RegisterTool(
		&mcp.Tool{Name: "restart_deployment", Annotations: annotations},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ran = true
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "restarted"}}}, nil
		})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	agent, err := NewAgent(nil, registry, &scriptedLLM{replies: []*LLMResponse{{ModelID: "gpt-5", Content: "done"}}})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := abandonedArc(t)
	if err := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: &fakePersistor{next: 10}, History: h, ArcID: testArcID}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return h.Arcs[0].Responses[0].ToolCalls[0], ran
}

// A call that may already have restarted a deployment is not restarted again
// on the strength of nobody having written down what happened.
func TestAnAbandonedCallIsSettledRatherThanRepeated(t *testing.T) {
	call, ran := recoverAbandoned(t, nil)

	if ran {
		t.Error("a tool that never said it was safe to repeat was run twice")
	}
	if call.Status != history.ToolCallStatusFailed {
		t.Errorf("the call is %s, want it settled", call.Status)
	}
	if call.Dispatches != 1 {
		t.Errorf("the call was dispatched %d times, want the one an earlier run wrote", call.Dispatches)
	}

	// The status says it did not succeed; only the text can say the tool may
	// have done the work anyway.
	result, _ := call.Result.(string)
	if !strings.Contains(result, "not known") {
		t.Errorf("result = %q, want it to say the effect is unknown", result)
	}
}

// A tool whose author says repeating it is harmless gets its second attempt.
func TestAnIdempotentCallIsRepeated(t *testing.T) {
	call, ran := recoverAbandoned(t, &mcp.ToolAnnotations{IdempotentHint: true})
	if !ran {
		t.Error("an idempotent tool was not tried again")
	}
	if call.Status != history.ToolCallStatusSuccess {
		t.Errorf("the call is %s, want its real result", call.Status)
	}
	if call.Dispatches != 2 {
		t.Errorf("the call was dispatched %d times, want the first and the retry", call.Dispatches)
	}
}

func TestAReadOnlyCallIsRepeated(t *testing.T) {
	if _, ran := recoverAbandoned(t, &mcp.ToolAnnotations{ReadOnlyHint: true}); !ran {
		t.Error("a read-only tool was not tried again")
	}
}

// A call nobody has dispatched is untouched by any of this: it runs.
func TestAFreshCallIsNotTreatedAsAbandoned(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.RegisterTool(&mcp.Tool{Name: "list_pods"}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if SafeToRepeat(&mcp.Tool{Name: "list_pods"}) {
		t.Error("a tool with no annotations reads as safe to repeat")
	}
	if SafeToRepeat(nil) {
		t.Error("no tool at all reads as safe to repeat")
	}
	if !SafeToRepeat(&mcp.Tool{Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}) {
		t.Error("a read-only tool reads as unsafe to repeat")
	}
}

// awaitedTaskArc is an arc that started a task, was answered with a reply of
// its own, and then heard back from the task: the shape a run picking the arc
// up again finds after a task has settled. The result lands at the sequence
// number given, so a caller can put it on either side of that reply.
func awaitedTaskArc(t *testing.T, resultSeq int) *history.History {
	t.Helper()
	arc := testArcID
	actor := users.Actor{UserID: "alice", TenantID: "acme"}
	at := time.Unix(0, 0)

	log := []events.Event{
		{ID: arc, Seq: 0, Timestamp: at, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: actor,
			Payload: events.UserMessagePayload{Content: "audit the cluster", Actor: actor}},
		{ID: "msg-1", Seq: 1, Timestamp: at, Typ: events.AssistantMessageEventType, CorrelationID: &arc,
			Payload: events.AssistantMessagePayload{ModelID: "gpt-5", Content: "starting the audit"}},
		{ID: "a-1", Seq: 2, Timestamp: at, Typ: events.AsyncTaskInvokeEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskInvokePayload{TaskName: "auditor"}},
		{ID: "ar-1", Seq: resultSeq, Timestamp: at, Typ: events.AsyncTaskResultEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskResultPayload{CausationID: "a-1", Result: "3 findings", Outcome: events.AsyncTaskOutcomeSuccess}},
		{ID: "msg-2", Seq: 4, Timestamp: at, Typ: events.AssistantMessageEventType, CorrelationID: &arc,
			Payload: events.AssistantMessagePayload{ModelID: "gpt-5", Content: "the audit is running"}},
	}
	slices.SortFunc(log, func(a, b events.Event) int { return a.Seq - b.Seq })

	h, err := history.Replay("session", log)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return h
}

// A task that finished after the arc's last reply is work the model has never
// reported on. The arc gets one more turn rather than closing on a reply
// written while the task was still running.
func TestALateTaskResultGetsTheModelATurn(t *testing.T) {
	h := awaitedTaskArc(t, 5)
	persistor := &fakePersistor{next: 10}
	agent, err := NewAgent(nil, nil, &scriptedLLM{replies: []*LLMResponse{
		{ModelID: "gpt-5", Content: "the audit found 3 things"},
	}})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if err := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := "ASSISTANT_MESSAGE, ARC_CLOSED"
	if got := typesOf(persistor.persisted); got != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}

	// The close names the reply that reported the result, not the one written
	// while the task was still running.
	closing := persistor.persisted[1].Payload.(events.ArcClosedPayload)
	if closing.CausationID != persistor.persisted[0].ID {
		t.Errorf("the close names %s, want the reply that reported the result", closing.CausationID)
	}
}

// A result the last reply already accounts for is not reported twice. Without
// this the arc answers itself for as long as the loop keeps reading it.
func TestATaskResultTheModelHasSeenClosesTheArc(t *testing.T) {
	h := awaitedTaskArc(t, 3)
	persistor := &fakePersistor{next: 10}
	agent, err := NewAgent(nil, nil, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if err := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := typesOf(persistor.persisted); got != "ARC_CLOSED" {
		t.Fatalf("wrote %s, want ARC_CLOSED", got)
	}
}

// waitingTaskArc is an arc whose task stopped to ask for input. The ask lands
// at the sequence number given, so a caller can put it on either side of the
// arc's last reply, and answered says whether the input arrived after it.
func waitingTaskArc(t *testing.T, askSeq int, answered bool) *history.History {
	t.Helper()
	arc := testArcID
	actor := users.Actor{UserID: "alice", TenantID: "acme"}
	at := time.Unix(0, 0)

	log := []events.Event{
		{ID: arc, Seq: 0, Timestamp: at, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: actor,
			Payload: events.UserMessagePayload{Content: "audit the cluster", Actor: actor}},
		{ID: "msg-1", Seq: 1, Timestamp: at, Typ: events.AssistantMessageEventType, CorrelationID: &arc,
			Payload: events.AssistantMessagePayload{ModelID: "gpt-5", Content: "starting the audit"}},
		{ID: "a-1", Seq: 2, Timestamp: at, Typ: events.AsyncTaskInvokeEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskInvokePayload{TaskName: "auditor"}},
		{ID: "aq-1", Seq: askSeq, Timestamp: at, Typ: events.AsyncTaskInputRequestedEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskInputRequestedPayload{CausationID: "a-1", Message: "which namespace?"}},
		{ID: "msg-2", Seq: 4, Timestamp: at, Typ: events.AssistantMessageEventType, CorrelationID: &arc,
			Payload: events.AssistantMessagePayload{ModelID: "gpt-5", Content: "the audit is running"}},
	}
	if answered {
		log = append(log, events.Event{ID: "ap-1", Seq: 6, Timestamp: at, Typ: events.AsyncTaskInputProvidedEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskInputProvidedPayload{CausationID: "a-1", Message: "kube-system"}})
	}
	slices.SortFunc(log, func(a, b events.Event) int { return a.Seq - b.Seq })

	h, err := history.Replay("session", log)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return h
}

// A task waiting on input nobody has been asked for yet gets the model a turn,
// which is the only way the question reaches the person who can answer it. The
// arc stays open, because the task is still there waiting.
func TestATaskWaitingOnInputGetsTheModelATurn(t *testing.T) {
	h := waitingTaskArc(t, 5, false)
	persistor := &fakePersistor{next: 10}
	agent, err := NewAgent(nil, nil, &scriptedLLM{replies: []*LLMResponse{
		{ModelID: "gpt-5", Content: "which namespace should I audit?"},
	}})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if err := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := typesOf(persistor.persisted); got != "ASSISTANT_MESSAGE" {
		t.Fatalf("wrote %s, want ASSISTANT_MESSAGE alone", got)
	}
	if status := h.Arcs[0].Status(); status != history.ArcStatusPending {
		t.Errorf("the arc is %s, want it left open for the task still waiting", status)
	}
}

// The question the agent has already put to the user is not put again. Without
// this the arc asks for as long as the task waits to be answered.
func TestAnAskAlreadyPutToTheUserIsNotRepeated(t *testing.T) {
	h := waitingTaskArc(t, 3, false)
	persistor := &fakePersistor{next: 10}
	agent, err := NewAgent(nil, nil, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if err := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := typesOf(persistor.persisted); got != "" {
		t.Fatalf("wrote %s, want nothing", got)
	}
}

// An ask the task already has its answer to is the task's business. The agent
// has nothing to say about it and waits for what the task makes of it.
func TestAnAnsweredAskGetsNoTurn(t *testing.T) {
	h := waitingTaskArc(t, 5, true)
	persistor := &fakePersistor{next: 10}
	agent, err := NewAgent(nil, nil, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if err := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: persistor, History: h, ArcID: testArcID}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := typesOf(persistor.persisted); got != "" {
		t.Fatalf("wrote %s, want nothing", got)
	}
}
