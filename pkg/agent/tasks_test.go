package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeController records what it was asked to do and answers with whatever it
// was told to. It holds no idea of what is running, which is the point: a
// controller is asked to do things and never asked how a task is doing.
type fakeController struct {
	cancelled []TaskRef
	inputs    []string
	requests  []string
	refs      []TaskRef
	failure   error
}

func (c *fakeController) Cancel(_ context.Context, ref TaskRef, _ string) error {
	if c.failure != nil {
		return c.failure
	}
	c.cancelled = append(c.cancelled, ref)
	return nil
}

func (c *fakeController) ProvideInput(_ context.Context, ref TaskRef, requestID, input string) error {
	if c.failure != nil {
		return c.failure
	}
	c.refs = append(c.refs, ref)
	c.requests = append(c.requests, requestID)
	c.inputs = append(c.inputs, input)
	return nil
}

// taskRegistryWith returns a registry holding one kind of task.
func taskRegistryWith(t *testing.T, name string, start Starter) *TaskRegistry {
	t.Helper()
	registry := NewTaskRegistry()
	if err := registry.RegisterTask(&mcp.Tool{Name: name}, start); err != nil {
		t.Fatalf("register task: %v", err)
	}
	return registry
}

func toolNames(tools []*mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// N kinds of task are N+2 tools: one to start each, and one each to stop and to
// answer whichever is running. The two are addressed by ID, so there is no
// reason to have one per kind.
func TestARegistryOffersOneToolPerTaskPlusTwo(t *testing.T) {
	registry := NewTaskRegistry()
	for _, name := range []string{"auditor", "migrator"} {
		if err := registry.RegisterTask(&mcp.Tool{Name: name}, func(context.Context, TaskRef, any) error { return nil }); err != nil {
			t.Fatalf("register task: %v", err)
		}
	}

	want := []string{"auditor", "migrator", CancelTaskTool, ProvideTaskInputTool}
	if got := toolNames(registry.All()); !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// With nothing to start there is nothing to stop or answer, so an empty
// registry costs the model no tools at all.
func TestAnEmptyRegistryOffersNothing(t *testing.T) {
	if tools := NewTaskRegistry().All(); len(tools) != 0 {
		t.Errorf("tools = %v, want none", toolNames(tools))
	}
	if tools := (*TaskRegistry)(nil).All(); len(tools) != 0 {
		t.Errorf("nil registry tools = %v, want none", toolNames(tools))
	}
}

func TestATaskCannotTakeTheNameOfTheToolsThatAddressTasks(t *testing.T) {
	registry := NewTaskRegistry()
	err := registry.RegisterTask(&mcp.Tool{Name: CancelTaskTool}, func(context.Context, TaskRef, any) error { return nil })
	if err == nil {
		t.Fatal("a task was registered under the name of the tool that cancels tasks")
	}
}

func TestATaskNeedsAWayToStart(t *testing.T) {
	if err := NewTaskRegistry().RegisterTask(&mcp.Tool{Name: "auditor"}, nil); err == nil {
		t.Fatal("a task with no starter was registered")
	}
}

// The model picks a tool by name and the agent routes by the same name, so one
// name meaning two things is a call that reaches whichever is checked first.
func TestANameCannotBeBothAToolAndATask(t *testing.T) {
	tools := registryWith(t, "auditor", nil)
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })

	_, err := NewAgent(nil, tools, &scriptedLLM{}, WithTasks(tasks, &fakeController{}))
	if err == nil {
		t.Fatal("an agent was built with one name registered as both")
	}
}

// A registry with nobody to reach the tasks it starts is half an agent.
func TestTasksNeedAController(t *testing.T) {
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })
	if _, err := NewAgent(nil, nil, &scriptedLLM{}, WithTasks(tasks, nil)); err == nil {
		t.Fatal("an agent was built with tasks it could never stop or answer")
	}
}

// The model is given one list, and it holds both what it can call and what it
// can start.
func TestTheModelIsOfferedToolsAndTasksTogether(t *testing.T) {
	tools := registryWith(t, "list_pods", nil)
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })

	agent, err := NewAgent(nil, tools, &scriptedLLM{}, WithTasks(tasks, &fakeController{}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	want := []string{"list_pods", "auditor", CancelTaskTool, ProvideTaskInputTool}
	if got := toolNames(CombineTools(agent.tools, agent.tasks).All()); !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// An agent with no tasks offers what it always offered, and every name is an
// ordinary tool's.
func TestAnAgentWithoutTasksIsUnchanged(t *testing.T) {
	tools := registryWith(t, "list_pods", nil)
	agent, err := NewAgent(nil, tools, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if got := toolNames(CombineTools(agent.tools, agent.tasks).All()); !slices.Equal(got, []string{"list_pods"}) {
		t.Errorf("tools = %v, want the one registered tool", got)
	}
	if agent.tasks.classify(CancelTaskTool) != opTool {
		t.Error("a name that addresses tasks routed somewhere with no tasks to address")
	}
}

func TestClassifyNamesTheOperation(t *testing.T) {
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })

	for name, want := range map[string]taskOperation{
		"auditor":            opStart,
		CancelTaskTool:       opCancel,
		ProvideTaskInputTool: opProvideInput,
		"list_pods":          opTool,
	} {
		if got := tasks.classify(name); got != want {
			t.Errorf("classify(%s) = %v, want %v", name, got, want)
		}
	}
}

var errUnreachable = errors.New("the runner is not answering")

// taskConversation is a log in which an arc started a task and a later arc is
// about to address it: the shape every cancel and every answer arrives in,
// since the task's own arc has already had its turn.
//
// waiting says whether the task stopped to ask for something.
func taskConversation(t *testing.T, waiting bool) *history.History {
	t.Helper()
	first, second := testArcID, "arc-2"
	actor := users.Actor{UserID: "alice", TenantID: "acme"}
	at := time.Unix(0, 0)

	log := []events.Event{
		{ID: first, Seq: 0, Timestamp: at, Typ: events.UserMessageEventType, CorrelationID: &first, Actor: actor,
			Payload: events.UserMessagePayload{Content: "audit the cluster", Actor: actor}},
		{ID: "msg-1", Seq: 1, Timestamp: at, Typ: events.AssistantMessageEventType, CorrelationID: &first,
			Payload: events.AssistantMessagePayload{ModelID: "gpt-5", Content: "starting the audit"}},
		{ID: "task-1", Seq: 2, Timestamp: at, Typ: events.AsyncTaskInvokeEventType, CorrelationID: &first,
			Payload: events.AsyncTaskInvokePayload{TaskName: "auditor"}},
	}
	if waiting {
		log = append(log, events.Event{ID: "ask-1", Seq: 3, Timestamp: at, Typ: events.AsyncTaskInputRequestedEventType, CorrelationID: &first,
			Payload: events.AsyncTaskInputRequestedPayload{CausationID: "task-1", Message: "which namespace?"}})
	}
	log = append(log, events.Event{ID: second, Seq: 4, Timestamp: at, Typ: events.UserMessageEventType, CorrelationID: &second, Actor: actor,
		Payload: events.UserMessagePayload{Content: "kube-system", Actor: actor}})

	h, err := history.Replay("session", log)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return h
}

// runCallInArc drives one turn of the named arc in which the model makes the
// given call, then says it is done.
func runCallInArc(t *testing.T, h *history.History, arcID string, tasks *TaskRegistry, controller TaskController, call LLMToolCall) *fakePersistor {
	t.Helper()
	persistor := &fakePersistor{next: 10}
	agent, err := NewAgent(nil, nil, &scriptedLLM{replies: []*LLMResponse{
		{ModelID: "gpt-5", ToolCalls: []LLMToolCall{call}},
		{ModelID: "gpt-5", Content: "done"},
	}}, WithTasks(tasks, controller))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := agent.Run(context.Background(), RunOptions{Persistor: persistor, History: h, ArcID: arcID}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return persistor
}

// lastCallIn returns the last tool call the named arc requested.
func lastCallIn(t *testing.T, h *history.History, arcID string) *history.ToolCall {
	t.Helper()
	for _, arc := range h.Arcs {
		if arc.ID != arcID {
			continue
		}
		for _, response := range slices.Backward(arc.Responses) {
			if len(response.ToolCalls) > 0 {
				return response.ToolCalls[len(response.ToolCalls)-1]
			}
		}
	}
	t.Fatalf("no tool call in arc %s", arcID)
	return nil
}

func resultText(t *testing.T, call *history.ToolCall) string {
	t.Helper()
	text, ok := call.Result.(string)
	if !ok {
		t.Fatalf("result = %#v, want text", call.Result)
	}
	return text
}

// The invocation is written before the task is started, because the record is
// what names the task and that name is what the task reports back under.
func TestStartingATaskNamesItBeforeItRuns(t *testing.T) {
	var started TaskRef
	tasks := taskRegistryWith(t, "auditor", func(_ context.Context, ref TaskRef, _ any) error {
		started = ref
		return nil
	})

	h := userArcHistory(t)
	persistor := runCallInArc(t, h, testArcID, tasks, &fakeController{}, LLMToolCall{
		VendorID: "call_1", ToolName: "auditor", Args: map[string]any{"namespace": "kube-system"}})

	want := "ASSISTANT_MESSAGE, TOOL_CALL, TOOL_DISPATCHED, ASYNC_TASK_INVOKE, TOOL_RESULT, ASSISTANT_MESSAGE"
	if got := typesOf(persistor.persisted); got != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}

	task := h.Arcs[0].AsyncTasks[0]
	if started.ID != task.ID {
		t.Errorf("the task was started as %q, want the id of the record that named it (%q)", started.ID, task.ID)
	}
	if started.ArcID != testArcID || started.Name != "auditor" {
		t.Errorf("ref = %#v, want it to carry the arc and the kind of task", started)
	}

	// The model is told the id, because it is the only way it can answer the
	// task or stop it later.
	if text := resultText(t, lastCallIn(t, h, testArcID)); !strings.Contains(text, task.ID) {
		t.Errorf("result = %q, want it to carry the task id", text)
	}
}

// A task the arc started keeps the arc open.
func TestAStartedTaskLeavesTheArcOpen(t *testing.T) {
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })
	h := userArcHistory(t)
	runCallInArc(t, h, testArcID, tasks, &fakeController{}, LLMToolCall{VendorID: "call_1", ToolName: "auditor"})

	if status := h.Arcs[0].Status(); status != history.ArcStatusPending {
		t.Errorf("arc is %s, want it held open by the task it started", status)
	}
}

// A launch that fails may still have taken hold, so the task is settled and the
// text says the effect is unknown rather than claiming nothing happened.
func TestATaskThatCouldNotBeStartedIsSettledAsUnknown(t *testing.T) {
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return errUnreachable })
	h := userArcHistory(t)
	persistor := runCallInArc(t, h, testArcID, tasks, &fakeController{}, LLMToolCall{VendorID: "call_1", ToolName: "auditor"})

	want := "ASSISTANT_MESSAGE, TOOL_CALL, TOOL_DISPATCHED, ASYNC_TASK_INVOKE, ASYNC_TASK_RESULT, TOOL_RESULT, ASSISTANT_MESSAGE, ARC_CLOSED"
	if got := typesOf(persistor.persisted); got != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}
	if status := h.Arcs[0].AsyncTasks[0].Status; status != history.AsyncTaskFailed {
		t.Errorf("task is %s, want it settled", status)
	}
	if text := resultText(t, lastCallIn(t, h, testArcID)); !strings.Contains(text, "not known") {
		t.Errorf("result = %q, want it to say the effect is unknown", text)
	}
}

// A cancel asks the task to stop and writes nothing about it. What became of
// work already under way is the runner's to report.
func TestCancellingATaskWritesNothingAboutIt(t *testing.T) {
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })
	controller := &fakeController{}
	h := taskConversation(t, false)

	persistor := runCallInArc(t, h, "arc-2", tasks, controller, LLMToolCall{
		VendorID: "call_2", ToolName: CancelTaskTool,
		Args: map[string]any{"task_id": "task-1", "reason": "no longer needed"}})

	want := "ASSISTANT_MESSAGE, TOOL_CALL, TOOL_DISPATCHED, TOOL_RESULT, ASSISTANT_MESSAGE, ARC_CLOSED"
	if got := typesOf(persistor.persisted); got != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}
	if len(controller.cancelled) != 1 || controller.cancelled[0].ID != "task-1" {
		t.Fatalf("cancelled = %#v, want the one task named", controller.cancelled)
	}
	// The ref carries the arc that owns the task, not the arc the call was
	// made in, since that is where a report about it has to land.
	if got := controller.cancelled[0].ArcID; got != testArcID {
		t.Errorf("cancelled arc = %q, want the task's own arc %q", got, testArcID)
	}
	if status := h.Arcs[0].AsyncTasks[0].Status; status != history.AsyncTaskPending {
		t.Errorf("task is %s, want it left for its runner to settle", status)
	}
}

// Input reaches the task before the log says it did, and the record lands on
// the arc that owns the task rather than the one the call was made in — that
// arc is the one waiting on the answer.
func TestInputIsGivenToTheTaskBeforeItIsRecorded(t *testing.T) {
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })
	controller := &fakeController{}
	h := taskConversation(t, true)

	persistor := runCallInArc(t, h, "arc-2", tasks, controller, LLMToolCall{
		VendorID: "call_2", ToolName: ProvideTaskInputTool,
		Args: map[string]any{"task_id": "task-1", "input": "kube-system"}})

	want := "ASSISTANT_MESSAGE, TOOL_CALL, TOOL_DISPATCHED, ASYNC_TASK_INPUT_PROVIDED, TOOL_RESULT, ASSISTANT_MESSAGE, ARC_CLOSED"
	if got := typesOf(persistor.persisted); got != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}
	if len(controller.inputs) != 1 || controller.inputs[0] != "kube-system" {
		t.Fatalf("inputs = %#v, want the one answer", controller.inputs)
	}
	// The answer names the question, so a task that has moved on can refuse it.
	if len(controller.requests) != 1 || controller.requests[0] != "ask-1" {
		t.Errorf("requests = %#v, want the question the task is stopped at", controller.requests)
	}

	recorded := persistor.persisted[3]
	if recorded.CorrelationID == nil || *recorded.CorrelationID != testArcID {
		t.Errorf("the record landed on %v, want the arc that owns the task", recorded.CorrelationID)
	}
	if status := h.Arcs[0].AsyncTasks[0].Status; status != history.AsyncTaskPending {
		t.Errorf("task is %s, want it running again", status)
	}
}

// A controller that could not reach the task must not leave the log saying the
// input arrived: the task would be waiting on an answer everyone believes it
// has.
func TestInputThatNeverReachedTheTaskIsNotRecorded(t *testing.T) {
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })
	controller := &fakeController{failure: errUnreachable}
	h := taskConversation(t, true)

	persistor := runCallInArc(t, h, "arc-2", tasks, controller, LLMToolCall{
		VendorID: "call_2", ToolName: ProvideTaskInputTool,
		Args: map[string]any{"task_id": "task-1", "input": "kube-system"}})

	if got := typesOf(persistor.persisted); strings.Contains(got, "ASYNC_TASK_INPUT_PROVIDED") {
		t.Fatalf("wrote %s, want no record of input the task never got", got)
	}
	if status := h.Arcs[0].AsyncTasks[0].Status; status != history.AsyncTaskWaiting {
		t.Errorf("task is %s, want it still waiting", status)
	}
	if text := resultText(t, lastCallIn(t, h, "arc-2")); !strings.Contains(text, "did not take the input") {
		t.Errorf("result = %q, want it to say the input did not land", text)
	}
}

// A call naming a task this conversation cannot see is answered rather than
// failing the run. The model made a mistake it can correct.
func TestAnsweringAnUnknownTaskIsAnErrorTheModelCanRead(t *testing.T) {
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })
	controller := &fakeController{}
	h := taskConversation(t, true)

	runCallInArc(t, h, "arc-2", tasks, controller, LLMToolCall{
		VendorID: "call_2", ToolName: ProvideTaskInputTool,
		Args: map[string]any{"task_id": "made-up", "input": "kube-system"}})

	call := lastCallIn(t, h, "arc-2")
	if call.Status != history.ToolCallStatusFailed {
		t.Errorf("call is %s, want it settled as failed", call.Status)
	}
	if text := resultText(t, call); !strings.Contains(text, "made-up") {
		t.Errorf("result = %q, want it to name the task that was not found", text)
	}
	if len(controller.inputs) != 0 {
		t.Errorf("inputs = %#v, want the controller left alone", controller.inputs)
	}
}

// A task that is not waiting has nothing to be told.
func TestAnsweringATaskThatIsNotWaitingIsRefused(t *testing.T) {
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })
	controller := &fakeController{}
	h := taskConversation(t, false)

	runCallInArc(t, h, "arc-2", tasks, controller, LLMToolCall{
		VendorID: "call_2", ToolName: ProvideTaskInputTool,
		Args: map[string]any{"task_id": "task-1", "input": "kube-system"}})

	if text := resultText(t, lastCallIn(t, h, "arc-2")); !strings.Contains(text, "not waiting for input") {
		t.Errorf("result = %q, want it to say the task was not asking", text)
	}
	if len(controller.inputs) != 0 {
		t.Errorf("inputs = %#v, want the controller left alone", controller.inputs)
	}
}

// Stopping something that already stopped is not a stop, and saying so as an
// error is what stops the model reporting that it cancelled the work.
func TestCancellingAFinishedTaskSaysSo(t *testing.T) {
	tasks := taskRegistryWith(t, "auditor", func(context.Context, TaskRef, any) error { return nil })
	controller := &fakeController{}
	h := taskConversation(t, false)
	settleTask(t, h, "task-1")

	runCallInArc(t, h, "arc-2", tasks, controller, LLMToolCall{
		VendorID: "call_2", ToolName: CancelTaskTool, Args: map[string]any{"task_id": "task-1"}})

	call := lastCallIn(t, h, "arc-2")
	if call.Status != history.ToolCallStatusFailed {
		t.Errorf("call is %s, want it settled as failed", call.Status)
	}
	if text := resultText(t, call); !strings.Contains(text, "nothing to stop") {
		t.Errorf("result = %q, want it to say there was nothing to stop", text)
	}
	if len(controller.cancelled) != 0 {
		t.Errorf("cancelled = %#v, want the controller left alone", controller.cancelled)
	}
}

// settleTask folds a completion into a history that is already built.
func settleTask(t *testing.T, h *history.History, taskID string) {
	t.Helper()
	arc := testArcID
	ev := &events.Event{
		ID: "result-" + taskID, Seq: 5, Timestamp: time.Unix(0, 0),
		Typ: events.AsyncTaskResultEventType, CorrelationID: &arc,
		Payload: events.AsyncTaskResultPayload{
			CausationID: taskID, Result: "14 findings", Outcome: events.AsyncTaskOutcomeSuccess},
	}
	if err := h.Update(ev, history.FoldStateFromHistory(h)); err != nil {
		t.Fatalf("fold: %v", err)
	}
}
