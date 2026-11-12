package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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
	runErr := agent.Run(context.Background(), RunOptions{AgentID: testAgentID, Persistor: guarded, History: h, ArcID: testArcID})
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

// recordingController is a TaskController that remembers what it was asked to
// stop, and can refuse.
type recordingController struct {
	mu      sync.Mutex
	stopped []TaskRef
	reasons []string
	err     error
}

func (c *recordingController) Cancel(_ context.Context, ref TaskRef, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.stopped = append(c.stopped, ref)
	c.reasons = append(c.reasons, reason)
	return nil
}

func (c *recordingController) ProvideInput(context.Context, TaskRef, string, string) error {
	return nil
}

// cancelledTaskArc is an arc with a task still running and a cancel recorded
// against it: what the log holds when somebody withdrew an arc whose task is
// off doing something.
func cancelledTaskArc(t *testing.T, reason string) *logStore {
	t.Helper()
	store := runningTaskStore(t)
	arc := testArcID
	if _, err := store.Persist(context.Background(), &events.Event{
		SessionID: testSessionID, Typ: events.CancelEventType, CorrelationID: &arc,
		Payload: events.CancelPayload{Reason: &reason},
	}); err != nil {
		t.Fatalf("persist the cancel: %v", err)
	}
	return store
}

// workerOver builds a worker whose agent can start and reach tasks.
func workerOver(t *testing.T, store *logStore, controller TaskController, options ...ArcWorkerOption) *ArcWorker {
	t.Helper()
	tasks := NewTaskRegistry()
	if err := tasks.RegisterTask(&mcp.Tool{Name: "auditor"}, func(context.Context, TaskRef, any) error {
		return nil
	}); err != nil {
		t.Fatalf("RegisterTask: %v", err)
	}
	agent, err := NewAgent(nil, nil, &scriptedLLM{}, WithTasks(tasks, controller))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	worker, err := NewArcWorker(store, store, NewFixedResolver(agent), nil, options...)
	if err != nil {
		t.Fatalf("NewArcWorker: %v", err)
	}
	return worker
}

// Nobody who cancels can reach work running somewhere else, and an arc whose
// only unfinished work is a task has no run on it to notice. So the reach
// happens when a worker is next given the arc.
func TestWorkingACancelledArcStopsItsTasks(t *testing.T) {
	store := cancelledTaskArc(t, "changed my mind")
	controller := &recordingController{}

	worked, err := workerOver(t, store, controller).Work(context.Background(), testRef(), testAgentID)
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !worked {
		t.Fatal("the arc was not worked")
	}

	if len(controller.stopped) != 1 {
		t.Fatalf("stopped %d tasks, want the one that was running", len(controller.stopped))
	}
	if got := controller.stopped[0]; got.ID != "task-1" || got.ArcRef != testRef() {
		t.Errorf("stopped %+v, want task-1 under %s", got, testRef())
	}
	// The reason the human gave travels to the runner, which is the only place
	// it can say why the work was called off.
	if got := controller.reasons[0]; got != "changed my mind" {
		t.Errorf("stopped with reason %q, want the one the cancel carried", got)
	}

	// Nothing is written. The cancel ends the waiting, and what became of work
	// already under way is the runner's to report.
	want := "USER_MESSAGE, ASSISTANT_MESSAGE, ASYNC_TASK_INVOKE, CANCEL"
	if got := store.types(); got != want {
		t.Errorf("log = %s, want %s", got, want)
	}
}

// A task that could not be reached fails the run, so whoever handed the arc
// over comes back with it. It is not recorded as a failure: the arc is already
// closed by the cancel, and a controller nobody could reach is not a second
// thing to put in the log.
func TestATaskThatCannotBeStoppedFailsTheRunWithoutRecordingIt(t *testing.T) {
	store := cancelledTaskArc(t, "changed my mind")
	controller := &recordingController{err: errors.New("the runner is unreachable")}

	_, err := workerOver(t, store, controller).Work(context.Background(), testRef(), testAgentID)
	if err == nil {
		t.Fatal("Work reported success although the task was never told")
	}

	want := "USER_MESSAGE, ASSISTANT_MESSAGE, ASYNC_TASK_INVOKE, CANCEL"
	if got := store.types(); got != want {
		t.Errorf("log = %s, want %s", got, want)
	}
}

// An arc cancelled with nothing running is the cheap no-op the queue deals in.
func TestWorkingACancelledArcWithNoTasksDoesNothing(t *testing.T) {
	store := cancelledTaskArc(t, "")
	// Settle the task, so the arc is cancelled with nothing left to reach.
	if _, err := store.Persist(context.Background(), &events.Event{
		SessionID: testSessionID, Typ: events.AsyncTaskResultEventType, CorrelationID: ptr(testArcID),
		Payload: events.AsyncTaskResultPayload{
			CausationID: "task-1", Result: "done", Outcome: events.AsyncTaskOutcomeSuccess},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	controller := &recordingController{}

	if _, err := workerOver(t, store, controller).Work(context.Background(), testRef(), testAgentID); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(controller.stopped) != 0 {
		t.Errorf("stopped %+v, want nothing", controller.stopped)
	}
}

// openArcStore is an arc with nothing but the user's message in it.
func openArcStore(t *testing.T) *logStore {
	t.Helper()
	store := &logStore{}
	if _, err := store.Persist(context.Background(), &events.Event{
		SessionID: testSessionID, Typ: events.UserMessageEventType, CorrelationID: ptr(testArcID),
		Payload: events.UserMessagePayload{Content: "wait for the rollout"},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	store.events[0].ID = testArcID
	return store
}

// The guard cannot stop a tool that is already running: it refuses the next
// dispatch, which is one tool too late. The watcher is the half that reaches
// inside, and the tool's own account of what it did still reaches the log.
func TestTheWatcherStopsAToolAlreadyRunning(t *testing.T) {
	cancels := NewLocalCancels()
	running := make(chan struct{})
	registry := registryWith(t, "wait", func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(running)
		<-ctx.Done()
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "stopped before the rollout finished"}},
		}, nil
	})

	store := openArcStore(t)
	chat, err := NewAgent(nil, registry, &scriptedLLM{replies: []*LLMResponse{
		{ModelID: "gpt-5", ToolCalls: []LLMToolCall{{VendorID: "call_abc", ToolName: "wait"}}},
	}})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	worker, err := NewArcWorker(store, store, NewFixedResolver(chat), nil, WithCancelWatcher(cancels))
	if err != nil {
		t.Fatalf("NewArcWorker: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := worker.Work(context.Background(), testRef(), testAgentID)
		done <- err
	}()

	<-running
	if err := cancels.NotifyCancel(context.Background(), testRef()); err != nil {
		t.Fatalf("NotifyCancel: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Work returned %v, want the cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tool was never stopped")
	}

	want := "USER_MESSAGE, ASSISTANT_MESSAGE, TOOL_CALL, TOOL_DISPATCHED, TOOL_RESULT"
	if got := store.types(); got != want {
		t.Fatalf("log = %s, want %s", got, want)
	}
}

// Watching an arc that is cancelled later and one that is not must not be
// confused, or a cancel stops a run on a different arc.
func TestNotifyingOneArcLeavesAnotherAlone(t *testing.T) {
	cancels := NewLocalCancels()
	mine, stopMine := cancels.Watch(context.Background(), testRef())
	defer stopMine()
	theirs, stopTheirs := cancels.Watch(context.Background(), ArcRef{SessionID: testSessionID, ArcID: "arc-2"})
	defer stopTheirs()

	if err := cancels.NotifyCancel(context.Background(), testRef()); err != nil {
		t.Fatalf("NotifyCancel: %v", err)
	}

	select {
	case <-mine:
	case <-time.After(time.Second):
		t.Error("the cancelled arc's watcher was not told")
	}
	select {
	case <-theirs:
		t.Error("another arc's watcher was told")
	default:
	}
}

// A watcher that stops after the cancel must not close a channel twice, and
// one that stops before it must not be told at all.
func TestStoppingAWatchIsSafeEitherSideOfTheCancel(t *testing.T) {
	cancels := NewLocalCancels()

	_, stopFirst := cancels.Watch(context.Background(), testRef())
	if err := cancels.NotifyCancel(context.Background(), testRef()); err != nil {
		t.Fatalf("NotifyCancel: %v", err)
	}
	stopFirst()

	left, stopSecond := cancels.Watch(context.Background(), testRef())
	stopSecond()
	if err := cancels.NotifyCancel(context.Background(), testRef()); err != nil {
		t.Fatalf("NotifyCancel: %v", err)
	}
	select {
	case <-left:
		t.Error("a watch that was stopped was still told")
	default:
	}
}
