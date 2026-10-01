package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cancelledArcs reports a cancel for the arcs it was told about.
type cancelledArcs struct {
	mu   sync.Mutex
	arcs map[string]*events.Event
	err  error
	asks int
}

func (c *cancelledArcs) Cancelled(_ context.Context, ref ArcRef) (*events.Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asks++
	if c.err != nil {
		return nil, c.err
	}
	return c.arcs[ref.ArcID], nil
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
	guarded := GuardCancellation(persistor, cancels)
	agent, err := NewAgent(nil, registry, &scriptedLLM{replies: []*LLMResponse{
		{ModelID: "gpt-5", ToolCalls: []LLMToolCall{{VendorID: "call_abc", ToolName: "list_pods"}}},
		{ModelID: "gpt-5", Content: "two are pending"},
	}})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	h := userArcHistory(t)
	runErr := agent.Run(context.Background(), RunOptions{Persistor: guarded, History: h, ArcID: testArcID})
	if ran && cancels.arcs[testArcID] != nil {
		t.Error("the tool ran for a cancelled arc")
	}
	return h, persistor, runErr
}

// The guard stops the run before the tool, and hands back the cancel so the
// arc the run is reading is settled rather than left looking open.
func TestTheGuardStopsARunOnACancelledArc(t *testing.T) {
	h, persistor, err := runGuarded(t, &cancelledArcs{arcs: map[string]*events.Event{testArcID: cancelOf(testArcID)}})
	if !errors.Is(err, ErrCancelled) {
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
		if _, err := guarded.Persist(context.Background(), &events.Event{
			SessionID: testSessionID, Typ: typ, CorrelationID: &arc,
		}); err != nil {
			t.Fatalf("Persist(%s): %v", typ, err)
		}
	}
	if cancels.asks != 0 {
		t.Errorf("the guard asked about %d writes that start nothing", cancels.asks)
	}

	if _, err := guarded.Persist(context.Background(), &events.Event{
		SessionID: testSessionID, Typ: events.ToolDispatchedEventType, CorrelationID: &arc,
	}); err != nil {
		t.Fatalf("Persist(dispatch): %v", err)
	}
	if cancels.asks != 1 {
		t.Errorf("the guard asked %d times about a dispatch, want once", cancels.asks)
	}
}

// The guard holds nothing between writes. It used to remember the arcs it had
// refused, which raced between the runs sharing one persistor and saved about
// one read, since a refusal ends the run at the first dispatch it reaches.
func TestTheGuardRemembersNothingBetweenWrites(t *testing.T) {
	cancels := &cancelledArcs{arcs: map[string]*events.Event{testArcID: cancelOf(testArcID)}}
	guarded := GuardCancellation(&fakePersistor{next: 1}, cancels)
	arc := testArcID

	for range 3 {
		_, err := guarded.Persist(context.Background(), &events.Event{
			SessionID: testSessionID, Typ: events.ToolDispatchedEventType, CorrelationID: &arc,
		})
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("Persist returned %v, want every dispatch refused", err)
		}
	}
	if cancels.asks != 3 {
		t.Errorf("the guard asked %d times, want it to read the cancel afresh each time", cancels.asks)
	}
}

// One persistor serves every run in a process, so arcs being worked at once
// write through the same guard. Half of these are cancelled and half are not,
// which is what makes the guard take both paths at once. Run with -race, which
// is what this is here for.
func TestTheGuardServesConcurrentRuns(t *testing.T) {
	const arcs = 8
	cancels := &cancelledArcs{arcs: map[string]*events.Event{}}
	for i := range arcs {
		if i%2 == 0 {
			arc := fmt.Sprintf("arc-%d", i)
			cancels.arcs[arc] = cancelOf(arc)
		}
	}
	guarded := GuardCancellation(&fakePersistor{next: 1}, cancels)

	var wg sync.WaitGroup
	for i := range arcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			arc := fmt.Sprintf("arc-%d", i)
			for range 8 {
				_, err := guarded.Persist(context.Background(), &events.Event{
					SessionID: testSessionID, Typ: events.ToolDispatchedEventType, CorrelationID: &arc,
				})
				if refused := errors.Is(err, ErrCancelled); refused != (i%2 == 0) {
					t.Errorf("arc %s: Persist returned %v, want refused=%v", arc, err, i%2 == 0)
					return
				}
			}
		}()
	}
	wg.Wait()
}
