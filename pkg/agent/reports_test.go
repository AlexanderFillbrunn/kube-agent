package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
)

// logStore is a persistor that is also readable, which is what the inbound
// half needs: it writes a report and then reads the arc back to work it.
type logStore struct {
	mu     sync.Mutex
	events []*events.Event
	next   int
}

func (s *logStore) Persist(_ context.Context, evs ...*events.Event) ([]*events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := make([]*events.Event, 0, len(evs))
	for _, ev := range evs {
		one := *ev
		one.Seq = s.next
		if one.ID == "" {
			one.ID = "ev-" + string(rune('a'+s.next))
		}
		if one.Timestamp.IsZero() {
			one.Timestamp = time.Unix(int64(s.next), 0)
		}
		s.next++
		s.events = append(s.events, &one)
		stored = append(stored, &one)
	}
	return stored, nil
}

// GetEventsForArc applies the projection's cutoff the way the real store does:
// everything up to the arc's own opening, plus everything correlated to it.
func (s *logStore) GetEventsForArc(_ context.Context, arcID string) ([]*events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	opening := -1
	for _, ev := range s.events {
		if ev.Typ == events.UserMessageEventType && ev.CorrelationID != nil && *ev.CorrelationID == arcID {
			opening = ev.Seq
			break
		}
	}
	var selected []*events.Event
	for _, ev := range s.events {
		if ev.Seq <= opening || (ev.CorrelationID != nil && *ev.CorrelationID == arcID) {
			selected = append(selected, ev)
		}
	}
	return selected, nil
}

func (s *logStore) types() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return typesOf(s.events)
}

// runningTaskStore is a log holding one arc that started a task and answered
// without waiting for it, which is where every report arrives.
func runningTaskStore(t *testing.T) *logStore {
	t.Helper()
	arc := testArcID
	actor := users.Actor{UserID: "alice", TenantID: "acme"}
	store := &logStore{}
	for _, ev := range []*events.Event{
		{Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: actor,
			Payload: events.UserMessagePayload{Content: "audit the cluster", Actor: actor}},
		{ID: "msg-1", Typ: events.AssistantMessageEventType, CorrelationID: &arc,
			Payload: events.AssistantMessagePayload{ModelID: "gpt-5", Content: "starting the audit"}},
		{ID: "task-1", Typ: events.AsyncTaskInvokeEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskInvokePayload{TaskName: "auditor"}},
	} {
		if _, err := store.Persist(context.Background(), ev); err != nil {
			t.Fatalf("persist: %v", err)
		}
	}
	// The arc's opening has to carry the arc's own ID, which the store only
	// assigns to events that arrive without one.
	store.events[0].ID = arc
	return store
}

func reportsFor(t *testing.T, store *logStore, replies ...*LLMResponse) *TaskReports {
	t.Helper()
	agent, err := NewAgent(nil, nil, &scriptedLLM{replies: replies})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	worker, err := NewArcWorker("session", store, store, agent, nil)
	if err != nil {
		t.Fatalf("NewArcWorker: %v", err)
	}
	reports, err := NewTaskReports(worker)
	if err != nil {
		t.Fatalf("NewTaskReports: %v", err)
	}
	return reports
}

func taskRef() TaskRef { return TaskRef{ArcID: testArcID, ID: "task-1", Name: "auditor"} }

// A completion is recorded and the arc worked, so the model says what came of
// the work it started.
func TestACompletionIsRecordedAndGetsTheModelATurn(t *testing.T) {
	store := runningTaskStore(t)
	reports := reportsFor(t, store, &LLMResponse{ModelID: "gpt-5", Content: "the audit found 3 things"})

	worked, err := reports.Report(context.Background(), taskRef(), TaskReport{
		Kind: TaskReportCompleted, Result: "3 findings", Outcome: events.AsyncTaskOutcomeSuccess})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !worked {
		t.Error("the arc was not worked")
	}

	want := "USER_MESSAGE, ASSISTANT_MESSAGE, ASYNC_TASK_INVOKE, ASYNC_TASK_RESULT, ASSISTANT_MESSAGE, ARC_CLOSED"
	if got := store.types(); got != want {
		t.Fatalf("log = %s, want %s", got, want)
	}
}

// An ask gets a turn too, because putting the question to a person is the only
// thing that can move the task on.
func TestAnInputRequestGetsTheModelATurn(t *testing.T) {
	store := runningTaskStore(t)
	reports := reportsFor(t, store, &LLMResponse{ModelID: "gpt-5", Content: "which namespace should I audit?"})

	_, err := reports.Report(context.Background(), taskRef(), TaskReport{
		Kind: TaskReportInputRequested, Message: "which namespace?"})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	want := "USER_MESSAGE, ASSISTANT_MESSAGE, ASYNC_TASK_INVOKE, ASYNC_TASK_INPUT_REQUESTED, ASSISTANT_MESSAGE"
	if got := store.types(); got != want {
		t.Fatalf("log = %s, want %s", got, want)
	}
}

// A notification is recorded and the arc is worked all the same. There is
// nothing for the model to do, and it is the log that says so rather than a
// second opinion here: the run reads the arc, finds nothing owed, and writes
// nothing.
func TestANotificationIsRecordedAndSaysNothing(t *testing.T) {
	store := runningTaskStore(t)
	reports := reportsFor(t, store)

	_, err := reports.Report(context.Background(), taskRef(), TaskReport{
		Kind: TaskReportNotification, Message: "scanned 40 namespaces"})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	want := "USER_MESSAGE, ASSISTANT_MESSAGE, ASYNC_TASK_INVOKE, ASYNC_TASK_EVENT"
	if got := store.types(); got != want {
		t.Fatalf("log = %s, want %s", got, want)
	}
}

// The arc in a reference arrives from outside, so it says where to look and
// never what is true. A report about a task that arc does not own is refused
// before anything is written.
func TestAReportNamingTheWrongArcIsRefused(t *testing.T) {
	store := runningTaskStore(t)
	reports := reportsFor(t, store)

	_, err := reports.Report(context.Background(), TaskRef{ArcID: testArcID, ID: "not-this-one"}, TaskReport{
		Kind: TaskReportCompleted, Result: "done", Outcome: events.AsyncTaskOutcomeSuccess})
	if err == nil {
		t.Fatal("a report about a task the arc does not hold was accepted")
	}
	if got := store.types(); strings.Contains(got, "ASYNC_TASK_RESULT") {
		t.Errorf("log = %s, want nothing written", got)
	}
}

// A completion that does not say how it ended is not a completion.
func TestACompletionWithoutAnOutcomeIsRefused(t *testing.T) {
	store := runningTaskStore(t)
	reports := reportsFor(t, store)

	_, err := reports.Report(context.Background(), taskRef(), TaskReport{Kind: TaskReportCompleted, Result: "done"})
	if err == nil {
		t.Fatal("a completion with no outcome was accepted")
	}
}

// An arc somebody else is working is left to them. The report is in the log,
// which is where whoever holds the arc will read it.
func TestAReportOnAHeldArcIsRecordedAndLeft(t *testing.T) {
	store := runningTaskStore(t)
	lease := NewLocalLease()
	agent, err := NewAgent(nil, nil, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	worker, err := NewArcWorker("session", store, store, agent, lease)
	if err != nil {
		t.Fatalf("NewArcWorker: %v", err)
	}
	reports, err := NewTaskReports(worker)
	if err != nil {
		t.Fatalf("NewTaskReports: %v", err)
	}

	release, ok := lease.Acquire(testArcID)
	if !ok {
		t.Fatal("the arc was already held")
	}
	defer release()

	// The model is scripted with no replies, so a run here would fail rather
	// than quietly do the wrong thing.
	worked, err := reports.Report(context.Background(), taskRef(), TaskReport{
		Kind: TaskReportCompleted, Result: "3 findings", Outcome: events.AsyncTaskOutcomeSuccess})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if worked {
		t.Error("an arc somebody else held was worked anyway")
	}

	want := "USER_MESSAGE, ASSISTANT_MESSAGE, ASYNC_TASK_INVOKE, ASYNC_TASK_RESULT"
	if got := store.types(); got != want {
		t.Fatalf("log = %s, want %s", got, want)
	}
}

func TestALeaseIsReleased(t *testing.T) {
	lease := NewLocalLease()
	release, ok := lease.Acquire("arc-1")
	if !ok {
		t.Fatal("a free arc could not be taken")
	}
	if _, again := lease.Acquire("arc-1"); again {
		t.Error("a held arc was taken twice")
	}
	if _, other := lease.Acquire("arc-2"); !other {
		t.Error("holding one arc blocked another")
	}
	release()
	if _, after := lease.Acquire("arc-1"); !after {
		t.Error("a released arc could not be taken again")
	}
}

// A task whose runner died with its process is settled, or its arc stays open
// for ever and every later turn is told work is in flight that will never
// report.
func TestAbandonedTasksAreSettledAsUnknown(t *testing.T) {
	store := runningTaskStore(t)
	h, err := history.Replay("session", valuesOf(store.events))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	settled, err := SettleAbandonedTasks(context.Background(), store, h)
	if err != nil {
		t.Fatalf("SettleAbandonedTasks: %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled %d tasks, want the one that was running", settled)
	}

	recorded := store.events[len(store.events)-1]
	payload, ok := recorded.Payload.(events.AsyncTaskResultPayload)
	if !ok {
		t.Fatalf("payload = %#v, want a task result", recorded.Payload)
	}
	if payload.Outcome != events.AsyncTaskOutcomeFailed {
		t.Errorf("outcome = %s, want it settled as failed rather than cancelled: nobody stopped it", payload.Outcome)
	}
	if text, _ := payload.Result.(string); !strings.Contains(text, "not known") {
		t.Errorf("result = %q, want it to say whether the work happened is not known", text)
	}
}

// A task that already reported is not settled a second time.
func TestSettlingAbandonedTasksLeavesFinishedOnesAlone(t *testing.T) {
	store := runningTaskStore(t)
	arc := testArcID
	if _, err := store.Persist(context.Background(), &events.Event{
		Typ: events.AsyncTaskResultEventType, CorrelationID: &arc,
		Payload: events.AsyncTaskResultPayload{
			CausationID: "task-1", Result: "3 findings", Outcome: events.AsyncTaskOutcomeSuccess},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	h, err := history.Replay("session", valuesOf(store.events))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	settled, err := SettleAbandonedTasks(context.Background(), store, h)
	if err != nil {
		t.Fatalf("SettleAbandonedTasks: %v", err)
	}
	if settled != 0 {
		t.Errorf("settled %d tasks, want none: it had already reported", settled)
	}
}

func valuesOf(stored []*events.Event) []events.Event {
	out := make([]events.Event, 0, len(stored))
	for _, ev := range stored {
		out = append(out, *ev)
	}
	return out
}
