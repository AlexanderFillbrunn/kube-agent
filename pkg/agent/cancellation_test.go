package agent

import (
	"context"
	"errors"
	"testing"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cancelledArcs reports a cancel for the arcs it was told about.
type cancelledArcs struct {
	arcs map[string]*events.Event
	err  error
	asks int
}

func (c *cancelledArcs) Cancelled(_ context.Context, arcID string) (*events.Event, error) {
	c.asks++
	if c.err != nil {
		return nil, c.err
	}
	return c.arcs[arcID], nil
}

// cancelOf is the record a cancel leaves, as a store would hand it back.
func cancelOf(arcID string) *events.Event {
	reason := "the user stopped it"
	return &events.Event{
		ID: "cancel-1", Typ: events.CancelEventType, CorrelationID: &arcID,
		Payload: events.CancelPayload{Reason: &reason},
	}
}

// runGuarded runs one arc whose model asks for a tool, against a guard that
// already knows the arc was cancelled.
func runGuarded(t *testing.T, cancels *cancelledArcs) (*history.History, *fakePersistor, error) {
	t.Helper()
	var ran bool
	registry := registryWith(t, "list_pods", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ran = true
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "2 pending"}}}, nil
	})

	persistor := &fakePersistor{next: 1}
	agent, err := NewAgent(nil, registry, GuardCancellation(persistor, cancels), &scriptedLLM{replies: []*LLMResponse{
		{ModelID: "gpt-5", ToolCalls: []LLMToolCall{{VendorID: "call_abc", ToolName: "list_pods"}}},
		{ModelID: "gpt-5", Content: "two are pending"},
	}})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	runErr := agent.Run(context.Background(), "session", h, testArcID)
	if ran && cancels.arcs[testArcID] != nil {
		t.Error("the tool ran for a cancelled arc")
	}
	return h, persistor, runErr
}

// The guard stops the run before the tool, and hands back the cancel so the
// arc the run is reading is settled rather than left looking open.
func TestTheGuardStopsARunOnACancelledArc(t *testing.T) {
	h, persistor, err := runGuarded(t, &cancelledArcs{arcs: map[string]*events.Event{testArcID: cancelOf(testArcID)}})
	if !errors.Is(err, CancelledError) {
		t.Fatalf("Run returned %v, want it to report the cancellation", err)
	}

	// The cancel is not folded into the running history: it was recorded
	// before the events this run has written since, and the arc reads it in
	// the right place the next time it is loaded.
	if typesOf(persistor.persisted) != "ASSISTANT_MESSAGE, TOOL_CALL" {
		t.Errorf("wrote %s, want the reply and its request and nothing after", typesOf(persistor.persisted))
	}

	// A cancelled run is not a failed one, and the cancel already says how the
	// arc ended.
	if len(h.Arcs[0].Errors) != 0 {
		t.Errorf("a cancelled run recorded %d failures", len(h.Arcs[0].Errors))
	}
}

// An uncancelled arc is untouched: every write goes through and the run
// finishes.
func TestTheGuardLetsALiveArcThrough(t *testing.T) {
	h, persistor, err := runGuarded(t, &cancelledArcs{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.Arcs[0].Status() != history.ArcStatusDone {
		t.Errorf("arc is %s, want it closed", h.Arcs[0].Status())
	}
	if want := "ASSISTANT_MESSAGE, TOOL_CALL, TOOL_DISPATCHED, TOOL_RESULT, ASSISTANT_MESSAGE, ARC_CLOSED"; typesOf(persistor.persisted) != want {
		t.Errorf("wrote %s, want %s", typesOf(persistor.persisted), want)
	}
}

// A guard that cannot answer must not halt work that is otherwise fine. The
// cancel is still in the log and the run stops at a later step.
func TestAGuardThatCannotAnswerFailsOpen(t *testing.T) {
	_, _, err := runGuarded(t, &cancelledArcs{err: errors.New("the cancel store is down")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// Only a write that is about to start work is worth stopping. A reply and the
// calls it requests cost nothing further, so they are never asked about.
func TestTheGuardOnlyAsksAboutWritesThatStartWork(t *testing.T) {
	cancels := &cancelledArcs{}
	guarded := GuardCancellation(&fakePersistor{next: 1}, cancels)
	arc := testArcID

	for _, typ := range []events.EventType{
		events.AssistantMessageEventType,
		events.ToolCallEventType,
		events.ToolResultEventType,
		events.ArcClosedEventType,
	} {
		if _, err := guarded.Persist(context.Background(), &events.Event{Typ: typ, CorrelationID: &arc}); err != nil {
			t.Fatalf("Persist(%s): %v", typ, err)
		}
	}
	if cancels.asks != 0 {
		t.Errorf("the guard asked about %d writes that start nothing", cancels.asks)
	}

	if _, err := guarded.Persist(context.Background(), &events.Event{
		Typ: events.ToolDispatchedEventType, CorrelationID: &arc,
	}); err != nil {
		t.Fatalf("Persist(dispatch): %v", err)
	}
	if cancels.asks != 1 {
		t.Errorf("the guard asked %d times about a dispatch, want once", cancels.asks)
	}
}

// Once a run has been told, it does not ask again on every write.
func TestTheGuardAsksOnlyOncePerArc(t *testing.T) {
	cancels := &cancelledArcs{arcs: map[string]*events.Event{testArcID: cancelOf(testArcID)}}
	guarded := GuardCancellation(&fakePersistor{next: 1}, cancels)
	arc := testArcID

	for range 3 {
		guarded.Persist(context.Background(), &events.Event{
			Typ: events.ToolDispatchedEventType, CorrelationID: &arc,
		})
	}
	if cancels.asks != 1 {
		t.Errorf("the guard asked %d times, want once for an arc already known to be cancelled", cancels.asks)
	}
}
