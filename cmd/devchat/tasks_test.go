package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/internal/filestore"
	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
)

// scriptLLM answers with a canned reply per turn. The order is the order the
// agent asks in, which is what the test is checking as much as anything.
type scriptLLM struct {
	replies []*agent.LLMResponse
	calls   int
}

func (l *scriptLLM) StreamResponse(_ context.Context, _ *history.History, _ *agent.LLMOptions, _ agent.Sink[agent.RawToken]) (*agent.LLMResponse, error) {
	if l.calls >= len(l.replies) {
		return &agent.LLMResponse{ModelID: "test", Content: "(nothing scripted)"}, nil
	}
	reply := l.replies[l.calls]
	l.calls++
	return reply, nil
}

func (l *scriptLLM) Response(ctx context.Context, h *history.History, opts *agent.LLMOptions) (*agent.LLMResponse, error) {
	return l.StreamResponse(ctx, h, opts, nil)
}

// harness is devchat's wiring with a scripted model in place of a real one.
type harness struct {
	store   *filestore.Store
	session *session
	reports *agent.TaskReports
	runner  *localTasks
	llm     *scriptLLM
}

func newHarness(t *testing.T, replies ...*agent.LLMResponse) *harness {
	t.Helper()
	// Long enough that nothing finishes before the test looks, short enough
	// that a whole audit fits in a test run.
	auditDelay = 5 * time.Millisecond
	t.Cleanup(func() { auditDelay = 4 * time.Second })

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store, err := filestore.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	tasks := agent.NewTaskRegistry()
	runner := newLocalTasks()
	if err := runner.register(tasks); err != nil {
		t.Fatalf("register task: %v", err)
	}

	llm := &scriptLLM{replies: replies}
	chat, err := agent.NewAgent(nil, agent.NewToolRegistry(), llm, agent.WithTasks(tasks, runner))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	persistor := agent.MintIdentity(agent.GuardCancellation(store, store))
	cancels := agent.NewLocalCancels()
	worker, err := agent.NewArcWorker(persistor, store, agent.NewFixedResolver(chat), agent.NewLocalLease(),
		agent.WithCancelWatcher(cancels))
	if err != nil {
		t.Fatalf("NewArcWorker: %v", err)
	}
	reports, err := agent.NewTaskReports(worker)
	if err != nil {
		t.Fatalf("NewTaskReports: %v", err)
	}
	runner.attach(reports, nil, nil)

	return &harness{
		store:   store,
		reports: reports,
		runner:  runner,
		llm:     llm,
		session: &session{
			id:        path,
			store:     store,
			persistor: persistor,
			worker:    worker,
			cancels:   cancels,
			actor:     users.Actor{UserID: "dev", TenantID: "dev"},
		},
	}
}

// waitFor polls the log until it satisfies the condition, because a task runs
// on its own and reports when it is ready.
func (h *harness) waitFor(t *testing.T, what string, done func(*history.History) bool) *history.History {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		folded, err := h.session.fullHistory(context.Background())
		if err != nil {
			t.Fatalf("read log: %v", err)
		}
		if done(folded) {
			return folded
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; log holds %s", what, logTypes(t, h))
		}
		time.Sleep(time.Millisecond)
	}
}

func logTypes(t *testing.T, h *harness) string {
	t.Helper()
	stored, err := h.store.Events(context.Background())
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var names []string
	for _, ev := range stored {
		names = append(names, string(ev.Typ))
	}
	return strings.Join(names, ", ")
}

func firstTask(h *history.History) *history.AsyncTask {
	for _, arc := range h.Arcs {
		if len(arc.AsyncTasks) > 0 {
			return arc.AsyncTasks[0]
		}
	}
	return nil
}

// The whole round trip: the model starts a task, the task asks for something,
// the model puts the question to the user, the user answers in an arc of their
// own, the answer reaches the task, and the task's result gets the arc that
// started it a turn to report.
//
// Every report goes through the ingress rather than into the log directly, so
// what this exercises is the path a task running anywhere else would take.
func TestATaskAsksTheUserAndFinishes(t *testing.T) {
	h := newHarness(t,
		// The first turn starts the audit and says so.
		&agent.LLMResponse{ModelID: "test", ToolCalls: []agent.LLMToolCall{
			{VendorID: "call_1", ToolName: "audit_namespaces", Args: map[string]any{"depth": "full"}}}},
		&agent.LLMResponse{ModelID: "test", Content: "the audit is running"},
		// Woken by the task's question, it puts that question to the user.
		&agent.LLMResponse{ModelID: "test", Content: "which namespace should I look at?"},
		// The user answers in an arc of their own, and it is handed on.
		&agent.LLMResponse{ModelID: "test", ToolCalls: []agent.LLMToolCall{
			{VendorID: "call_2", ToolName: agent.ProvideTaskInputTool,
				Args: map[string]any{"task_id": "", "input": "kube-system"}}}},
		&agent.LLMResponse{ModelID: "test", Content: "passed it on"},
		// Woken by the result, it reports what came of the work.
		&agent.LLMResponse{ModelID: "test", Content: "the audit found two problems"},
	)

	ctx := context.Background()
	first := h.session.newArcID()
	if err := h.session.turn(ctx, first, "audit the cluster"); err != nil {
		t.Fatalf("first turn: %v", err)
	}

	// The arc stays open while its task runs, however final the reply reads.
	folded, err := h.session.fullHistory(ctx)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if status := folded.Arcs[0].Status(); status != history.ArcStatusPending {
		t.Fatalf("arc is %s, want it held open by its task", status)
	}

	// The task reports its question through the ingress, which gives the arc a
	// turn, and the model uses that turn to ask the user. The wait is for the
	// reply rather than for the task's status: the record of the question is
	// written first and the turn it earns comes after it.
	folded = h.waitFor(t, "the question to be put to the user", func(folded *history.History) bool {
		return len(folded.Arcs[0].Responses) >= 3
	})
	task := firstTask(folded)
	if task.Status != history.AsyncTaskWaiting {
		t.Fatalf("task is %s, want it waiting on an answer", task.Status)
	}

	replies := folded.Arcs[0].Responses
	if last := replies[len(replies)-1].Content; !strings.Contains(last, "which namespace") {
		t.Errorf("the last reply is %q, want the task's question put to the user", last)
	}

	// The user answers. It opens an arc of its own, which is why the task's ID
	// has to reach the model as data rather than as position.
	h.scriptAnswer(task.ID)
	second := h.session.newArcID()
	if err := h.session.turn(ctx, second, "kube-system"); err != nil {
		t.Fatalf("second turn: %v", err)
	}

	folded = h.waitFor(t, "the audit to finish", func(folded *history.History) bool {
		task := firstTask(folded)
		return task != nil && task.FinishedSeq != nil
	})
	task = firstTask(folded)
	if task.Status != history.AsyncTaskSuccess {
		t.Fatalf("task is %s, want it finished", task.Status)
	}
	if result, _ := task.Result.(string); !strings.Contains(result, "kube-system") {
		t.Errorf("result = %q, want the audit to have used the answer it was given", result)
	}

	// The arc that started the work is the arc that reports it, and only then
	// is it closed.
	folded = h.waitFor(t, "the arc to close", func(folded *history.History) bool {
		return folded.Arcs[0].Status() == history.ArcStatusDone
	})
	replies = folded.Arcs[0].Responses
	if last := replies[len(replies)-1].Content; !strings.Contains(last, "two problems") {
		t.Errorf("the last reply is %q, want it to report what the task found", last)
	}
}

// scriptAnswer fills in the task ID the model could not know when the replies
// were written, which is the same thing the real model does from the notes.
func (h *harness) scriptAnswer(taskID string) {
	for _, reply := range h.llm.replies {
		for i := range reply.ToolCalls {
			if reply.ToolCalls[i].ToolName != agent.ProvideTaskInputTool {
				continue
			}
			args := reply.ToolCalls[i].Args.(map[string]any)
			args["task_id"] = taskID
		}
	}
}

// Stopping the arc stops what it started, and the task says so itself: a
// cancel does not know whether work already under way stopped or what it did
// first.
func TestCancellingAnArcStopsItsTask(t *testing.T) {
	h := newHarness(t,
		&agent.LLMResponse{ModelID: "test", ToolCalls: []agent.LLMToolCall{
			{VendorID: "call_1", ToolName: "audit_namespaces"}}},
		&agent.LLMResponse{ModelID: "test", Content: "the audit is running"},
	)

	ctx := context.Background()
	arc := h.session.newArcID()
	if err := h.session.turn(ctx, arc, "audit the cluster"); err != nil {
		t.Fatalf("turn: %v", err)
	}

	if err := h.session.cancel(ctx, arc, "changed my mind"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The cancel is recorded and announced, and neither reaches a task running
	// somewhere else. That is the worker's, so the arc is owed the run that
	// does it, which is what the interface queues after a cancel.
	if _, err := h.session.worker.Work(ctx, h.session.ref(arc), localAgentID); err != nil {
		t.Fatalf("Work: %v", err)
	}

	folded := h.waitFor(t, "the task to report that it stopped", func(folded *history.History) bool {
		task := firstTask(folded)
		return task != nil && task.FinishedSeq != nil
	})
	if status := firstTask(folded).Status; status != history.AsyncTaskCancelled {
		t.Errorf("task is %s, want %s", status, history.AsyncTaskCancelled)
	}
}

// A task whose process died is settled on the way back in, or its arc is open
// for ever and every later turn is told work is in flight that will never
// report.
func TestReopeningALogSettlesAbandonedTasks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	arc := "arc-1"
	for _, ev := range []*events.Event{
		{SessionID: h.session.id, AgentID: localAgentID, Typ: events.UserMessageEventType, ID: arc, CorrelationID: &arc, Actor: h.session.actor,
			Payload: events.UserMessagePayload{Content: "audit the cluster", Actor: h.session.actor}},
		{SessionID: h.session.id, AgentID: localAgentID, Typ: events.AssistantMessageEventType, ID: "msg-1", CorrelationID: &arc,
			Payload: events.AssistantMessagePayload{ModelID: "test", Content: "running"}},
		{SessionID: h.session.id, AgentID: localAgentID, Typ: events.AsyncTaskInvokeEventType, ID: "task-1", CorrelationID: &arc,
			Payload: events.AsyncTaskInvokePayload{TaskName: "audit_namespaces"}},
	} {
		if _, err := h.session.persistor.Persist(ctx, ev); err != nil {
			t.Fatalf("persist: %v", err)
		}
	}

	folded, err := h.session.fullHistory(ctx)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	settled, err := agent.SettleAbandonedTasks(ctx, agent.AsAgent(localAgentID, h.session.persistor), folded)
	if err != nil {
		t.Fatalf("SettleAbandonedTasks: %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled %d, want the one task nothing is behind", settled)
	}

	folded, err = h.session.fullHistory(ctx)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	task := firstTask(folded)
	if task.Status != history.AsyncTaskFailed {
		t.Errorf("task is %s, want it settled", task.Status)
	}
	if result, _ := task.Result.(string); !strings.Contains(result, "not known") {
		t.Errorf("result = %q, want it to say whether the work happened is not known", result)
	}
}

// Two answers to one question, which is what two arcs holding the same answer
// produce. Only the runner can tell them apart: each caller decided from a
// replay taken before it called, and both read that the task was waiting.
func TestOnlyTheFirstAnswerToAQuestionReachesTheTask(t *testing.T) {
	tasks := newLocalTasks()
	ref := agent.TaskRef{ID: "task-1"}
	tasks.running[ref.ID] = &localTask{input: make(chan string, 1)}
	tasks.await(ref.ID, "ask-1")

	if err := tasks.ProvideInput(context.Background(), ref, "ask-1", "kube-system"); err != nil {
		t.Fatalf("first answer: %v", err)
	}
	if err := tasks.ProvideInput(context.Background(), ref, "ask-1", "ingress"); err == nil {
		t.Fatal("the second answer to one question was accepted")
	}

	if got := <-tasks.running[ref.ID].input; got != "kube-system" {
		t.Errorf("the task was given %q, want the answer that arrived first", got)
	}
}

// An answer to a question the task has already moved past is refused, rather
// than taken for the one it is waiting on now.
func TestAnAnswerToASupersededQuestionIsRefused(t *testing.T) {
	tasks := newLocalTasks()
	ref := agent.TaskRef{ID: "task-1"}
	tasks.running[ref.ID] = &localTask{input: make(chan string, 1)}
	tasks.await(ref.ID, "ask-2")

	if err := tasks.ProvideInput(context.Background(), ref, "ask-1", "kube-system"); err == nil {
		t.Fatal("an answer to the wrong question was accepted")
	}
	if len(tasks.running[ref.ID].input) != 0 {
		t.Error("the task was handed input it had not asked for")
	}
}
