package history

import (
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/users"
)

func TestHistoryFoldAppliesEveryNonCancellationEvent(t *testing.T) {
	start := time.Date(2026, time.September, 17, 13, 0, 0, 0, time.UTC)
	arc := "user-message-1"
	user := users.Actor{UserID: "user-1", TenantID: "tenant-1"}
	vendorID := "vendor-1"

	log := []events.Event{
		{ID: arc, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: user, Payload: events.UserMessagePayload{Content: "Check status", Actor: user}},
		{ID: "assistant-message-1", Seq: 1, Timestamp: start.Add(time.Second), Typ: events.AssistantMessageEventType, CorrelationID: &arc, Payload: events.AssistantMessagePayload{VendorID: &vendorID, ModelID: "model-1", Content: "Checking.", Reasoning: []byte("opaque-provider-bytes")}},
		{ID: "tool-call-1", Seq: 2, Timestamp: start.Add(2 * time.Second), Typ: events.ToolCallEventType, CorrelationID: &arc, Payload: events.ToolCallPayload{MessageID: "assistant-message-1", VendorID: "tool-vendor-1", ToolName: "lookup", Args: "status"}},
		{ID: "tool-dispatched-1", Seq: 3, Timestamp: start.Add(3 * time.Second), Typ: events.ToolDispatchedEventType, CorrelationID: &arc, Payload: events.ToolDispatchedPayload{CausationID: "tool-call-1"}},
		{ID: "tool-event-1", Seq: 4, Timestamp: start.Add(4 * time.Second), Typ: events.ToolEventEventType, CorrelationID: &arc, Payload: events.ToolEventPayload{CausationID: "tool-call-1", Payload: "working"}},
		{ID: "tool-result-1", Seq: 5, Timestamp: start.Add(5 * time.Second), Typ: events.ToolResultEventType, CorrelationID: &arc, Payload: events.ToolResultPayload{CausationID: "tool-call-1", Result: "healthy"}},
		{ID: "tool-call-2", Seq: 6, Timestamp: start.Add(6 * time.Second), Typ: events.ToolCallEventType, CorrelationID: &arc, Payload: events.ToolCallPayload{MessageID: "assistant-message-1", ToolName: "restart", Args: "ingress"}},
		{ID: "interrupt-1", Seq: 7, Timestamp: start.Add(7 * time.Second), Typ: events.InterruptEventType, CorrelationID: &arc, Payload: events.InterruptPayload{ToolCallID: "tool-call-2", Reason: "restart needs approval"}},
		{ID: "interrupt-response-1", Seq: 8, Timestamp: start.Add(8 * time.Second), Typ: events.InterruptResponseEventType, CorrelationID: &arc, Payload: events.InterruptResponsePayload{CausationID: "interrupt-1", Response: "approved", Actor: user}},
		{ID: "tool-result-2", Seq: 9, Timestamp: start.Add(9 * time.Second), Typ: events.ToolResultEventType, CorrelationID: &arc, Payload: events.ToolResultPayload{CausationID: "tool-call-2", Result: "restarted"}},
		{ID: "tool-call-3", Seq: 10, Timestamp: start.Add(10 * time.Second), Typ: events.ToolCallEventType, CorrelationID: &arc, Payload: events.ToolCallPayload{MessageID: "assistant-message-1", ToolName: "delete", Args: "namespace"}},
		{ID: "interrupt-2", Seq: 11, Timestamp: start.Add(11 * time.Second), Typ: events.InterruptEventType, CorrelationID: &arc, Payload: events.InterruptPayload{ToolCallID: "tool-call-3", Reason: "deletion needs approval"}},
		{ID: "interrupt-response-2", Seq: 12, Timestamp: start.Add(12 * time.Second), Typ: events.InterruptResponseEventType, CorrelationID: &arc, Payload: events.InterruptResponsePayload{CausationID: "interrupt-2", IsRejected: true, Actor: user}},
		{ID: "task-1", Seq: 13, Timestamp: start.Add(13 * time.Second), Typ: events.AsyncTaskInvokeEventType, CorrelationID: &arc, Payload: events.AsyncTaskInvokePayload{TaskName: "researcher", Args: "status"}},
		{ID: "task-event-1", Seq: 14, Timestamp: start.Add(14 * time.Second), Typ: events.AsyncTaskEventEventType, CorrelationID: &arc, Payload: events.AsyncTaskEventPayload{CausationID: "task-1", Payload: "looking up status"}},
		{ID: "task-input-request-1", Seq: 15, Timestamp: start.Add(15 * time.Second), Typ: events.AsyncTaskInputRequestedEventType, CorrelationID: &arc, Payload: events.AsyncTaskInputRequestedPayload{CausationID: "task-1", Message: "Which cluster?"}},
		{ID: "task-input-provided-1", Seq: 16, Timestamp: start.Add(16 * time.Second), Typ: events.AsyncTaskInputProvidedEventType, CorrelationID: &arc, Payload: events.AsyncTaskInputProvidedPayload{CausationID: "task-1", Message: "production"}},
		{ID: "task-result-1", Seq: 17, Timestamp: start.Add(17 * time.Second), Typ: events.AsyncTaskResultEventType, CorrelationID: &arc, Payload: events.AsyncTaskResultPayload{CausationID: "task-1", Result: "healthy", Outcome: events.AsyncTaskOutcomeSuccess}},
		{ID: "task-2", Seq: 18, Timestamp: start.Add(18 * time.Second), Typ: events.AsyncTaskInvokeEventType, CorrelationID: &arc, Payload: events.AsyncTaskInvokePayload{TaskName: "failing-task"}},
		{ID: "task-result-2", Seq: 19, Timestamp: start.Add(19 * time.Second), Typ: events.AsyncTaskResultEventType, CorrelationID: &arc, Payload: events.AsyncTaskResultPayload{CausationID: "task-2", Result: "unavailable", Outcome: events.AsyncTaskOutcomeFailed}},
		{ID: "error-1", Seq: 20, Timestamp: start.Add(20 * time.Second), Typ: events.ErrorEventType, CorrelationID: &arc, Payload: events.ErrorPayload{Message: "recorded failure", CausationID: stringPtr("tool-call-1")}},
	}

	history := foldEvents(t, "session-1", log)
	if len(history.Arcs) != 1 {
		t.Fatalf("arcs = %#v, want a single arc opened by the user message", history.Arcs)
	}
	got := history.Arcs[0]
	if got.ID != arc || got.Content != "Check status" || got.Actor == nil || got.Actor.UserID != user.UserID {
		t.Errorf("arc = %#v, want the user message that opened it", got)
	}
	if got.Cancel != nil {
		t.Errorf("arc cancel = %#v, want nil for an arc that was never cancelled", got.Cancel)
	}

	if got.Closed != nil {
		t.Errorf("arc closed = %#v, want nil for an arc the agent never closed", got.Closed)
	}

	if len(got.Responses) != 1 {
		t.Fatalf("responses = %#v, want one assistant message nested under the arc", got.Responses)
	}
	response := got.Responses[0]
	if response.Content != "Checking." || response.CorrelationID != arc {
		t.Errorf("response = %#v, want the assistant reply correlated to the arc", response)
	}
	if string(response.Reasoning) != "opaque-provider-bytes" {
		t.Errorf("reasoning = %q, want the bytes the provider sent, unread", response.Reasoning)
	}
	if len(response.ToolCalls) != 3 {
		t.Fatalf("tool calls = %#v, want three calls under the assistant message", response.ToolCalls)
	}
	tool := response.ToolCalls[0]
	if tool.Status != ToolCallStatusSuccess || tool.Result != "healthy" || len(tool.Events) != 1 {
		t.Errorf("tool = %#v, want successful call with one event", tool)
	}
	if tool.Dispatches != 1 || tool.DispatchedSeq == nil || tool.DispatchedAt == nil {
		t.Errorf("tool dispatch = %#v, want the one dispatch the log recorded", tool)
	}
	if tool.Interrupt != nil {
		t.Errorf("tool interrupt = %#v, want nil for a call nobody was asked about", tool.Interrupt)
	}

	if len(got.AsyncTasks) != 2 {
		t.Fatalf("async tasks = %#v, want two tasks nested under the arc", got.AsyncTasks)
	}
	if got.AsyncTasks[0].Status != AsyncTaskSuccess || got.AsyncTasks[0].Result != "healthy" {
		t.Errorf("successful task = %#v", got.AsyncTasks[0])
	}
	if transcript := got.AsyncTasks[0].Events; len(transcript) != 3 || transcript[0].Type != AsyncTaskEventNotification || transcript[1].Type != AsyncTaskEventInputRequested || transcript[2].Type != AsyncTaskEventInputProvided {
		t.Errorf("task event transcript = %#v, want notification, request, provided", transcript)
	}
	if got.AsyncTasks[1].Status != AsyncTaskFailed || got.AsyncTasks[1].Result != "unavailable" {
		t.Errorf("failed task = %#v", got.AsyncTasks[1])
	}

	// An interrupt hangs off the call it held up, and only a refusal settles
	// that call. An approval lets it run, and its own result settles it.
	approved := response.ToolCalls[1]
	if approved.Interrupt == nil || approved.Interrupt.Status != InterruptApproved || approved.Interrupt.Response != "approved" {
		t.Errorf("approved interrupt = %#v", approved.Interrupt)
	}
	if approved.Status != ToolCallStatusSuccess || approved.Result != "restarted" {
		t.Errorf("approved call = %#v, want the result it returned after approval", approved)
	}
	refused := response.ToolCalls[2]
	if refused.Interrupt == nil || refused.Interrupt.Status != InterruptRejected {
		t.Errorf("refused interrupt = %#v", refused.Interrupt)
	}
	if refused.Status != ToolCallStatusRefused || refused.FinishedSeq == nil {
		t.Errorf("refused call = %#v, want %s", refused, ToolCallStatusRefused)
	}
	if refused.Interrupt.ToolCallID != refused.ID {
		t.Errorf("interrupt names call %s, want %s", refused.Interrupt.ToolCallID, refused.ID)
	}
	if len(got.Errors) != 1 || got.Errors[0].CausationID == nil || *got.Errors[0].CausationID != "tool-call-1" {
		t.Errorf("errors = %#v, want recorded error caused by tool call", got.Errors)
	}
}

func TestHistoryFoldAppliesGuardrailAndCancel(t *testing.T) {
	start := time.Date(2026, time.September, 17, 14, 0, 0, 0, time.UTC)
	arc := "user-message-1"
	user := users.Actor{UserID: "user-1"}
	log := []events.Event{
		{ID: arc, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: user, Payload: events.UserMessagePayload{Content: "Start", Actor: user}},
		{ID: "assistant-message-1", Seq: 1, Timestamp: start.Add(time.Second), Typ: events.AssistantMessageEventType, CorrelationID: &arc, Payload: events.AssistantMessagePayload{ModelID: "model-1", Content: "Working."}},
		{ID: "blocked-tool", Seq: 2, Timestamp: start.Add(2 * time.Second), Typ: events.ToolCallEventType, CorrelationID: &arc, Payload: events.ToolCallPayload{MessageID: "assistant-message-1", ToolName: "blocked", VendorID: "vendor"}},
		{ID: "guardrail-1", Seq: 3, Timestamp: start.Add(3 * time.Second), Typ: events.GuardrailBlockedEventType, CorrelationID: &arc, Payload: events.GuardrailBlockedPayload{CausationID: "blocked-tool", Message: "not allowed"}},
		{ID: "pending-tool", Seq: 4, Timestamp: start.Add(4 * time.Second), Typ: events.ToolCallEventType, CorrelationID: &arc, Payload: events.ToolCallPayload{MessageID: "assistant-message-1", ToolName: "pending", VendorID: "vendor"}},
		{ID: "interrupt-1", Seq: 5, Timestamp: start.Add(5 * time.Second), Typ: events.InterruptEventType, CorrelationID: &arc, Payload: events.InterruptPayload{ToolCallID: "pending-tool", Reason: "approval"}},
		{ID: "task-1", Seq: 6, Timestamp: start.Add(6 * time.Second), Typ: events.AsyncTaskInvokeEventType, CorrelationID: &arc, Payload: events.AsyncTaskInvokePayload{TaskName: "researcher"}},
		{ID: "input-request-1", Seq: 7, Timestamp: start.Add(7 * time.Second), Typ: events.AsyncTaskInputRequestedEventType, CorrelationID: &arc, Payload: events.AsyncTaskInputRequestedPayload{CausationID: "task-1", Message: "Need input"}},
		{ID: "cancel-1", Seq: 8, Timestamp: start.Add(8 * time.Second), Typ: events.CancelEventType, CorrelationID: &arc, Actor: user, Payload: events.CancelPayload{Reason: stringPtr("user stopped")}},
	}

	history := foldEvents(t, "session-1", log)
	if len(history.Arcs) != 1 {
		t.Fatalf("arcs = %#v, want a single arc", history.Arcs)
	}
	got := history.Arcs[0]

	calls := got.Responses[0].ToolCalls
	if len(calls) != 2 {
		t.Fatalf("tool calls = %#v, want the blocked and the pending call", calls)
	}
	if calls[0].Status != ToolCallStatusBlocked || calls[0].Result != "not allowed" {
		t.Errorf("guardrailed tool = %#v", calls[0])
	}
	// The call never reached its tool, so the cancel settles it. Why the arc
	// was cancelled belongs to the cancel, not to a call the tool never saw.
	if calls[1].Status != ToolCallStatusCancelled || calls[1].Result != nil {
		t.Errorf("cancelled tool = %#v", calls[1])
	}
	// The task was invoked, so it started, and it runs where the cancel cannot
	// reach it. Cancelling ends the waiting, not the task.
	if got.AsyncTasks[0].Status != AsyncTaskWaiting || got.AsyncTasks[0].FinishedAt != nil {
		t.Errorf("task after the cancel = %#v, want it left unsettled", got.AsyncTasks[0])
	}
	if interrupt := calls[1].Interrupt; interrupt == nil || interrupt.Status != InterruptCancelled {
		t.Errorf("interrupt = %#v, want it cancelled with the call it held up", calls[1].Interrupt)
	}
	if got.Cancel == nil {
		t.Fatalf("arc has no cancel: %#v", got)
	}
	if got.Cancel.Arc != arc || !got.Cancel.TerminatedWork {
		t.Errorf("cancel = %#v, want a cancel on this arc that terminated work", got.Cancel)
	}
}

func TestHistoryFoldRetainsAsyncTaskNotificationsAndInputInteractions(t *testing.T) {
	start := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	arc := "arc-1"
	taskID := "task-1"
	user := users.Actor{UserID: "user-1"}
	notification := map[string]any{"message": "checking status"}
	eventLog := []events.Event{
		{
			ID: arc, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: user,
			Payload: events.UserMessagePayload{Content: "Research this", Actor: user},
		},
		{
			ID: taskID, Seq: 1, Timestamp: start.Add(time.Second), Typ: events.AsyncTaskInvokeEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskInvokePayload{TaskName: "researcher", Args: "topic"},
		},
		{
			ID: "task-event-1", Seq: 2, Timestamp: start.Add(2 * time.Second), Typ: events.AsyncTaskEventEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskEventPayload{CausationID: taskID, Payload: notification},
		},
		{
			ID: "input-request-1", Seq: 3, Timestamp: start.Add(3 * time.Second), Typ: events.AsyncTaskInputRequestedEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskInputRequestedPayload{CausationID: taskID, Message: "Which namespace?"},
		},
		{
			ID: "input-provided-1", Seq: 4, Timestamp: start.Add(4 * time.Second), Typ: events.AsyncTaskInputProvidedEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskInputProvidedPayload{CausationID: taskID, Message: "production"},
		},
		{
			ID: "task-result-1", Seq: 5, Timestamp: start.Add(5 * time.Second), Typ: events.AsyncTaskResultEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskResultPayload{CausationID: taskID, Result: "complete", Outcome: events.AsyncTaskOutcomeSuccess},
		},
	}

	history := NewHistory("session-1")
	state := NewFoldState()
	for i := range eventLog {
		if err := history.Update(&eventLog[i], state); err != nil {
			t.Fatalf("Update(event %d) error = %v", i, err)
		}
		switch i {
		case 3:
			if got := history.Arcs[0].AsyncTasks[0].Status; got != AsyncTaskWaiting {
				t.Errorf("status after input request = %s, want %s", got, AsyncTaskWaiting)
			}
		case 4:
			if got := history.Arcs[0].AsyncTasks[0].Status; got != AsyncTaskPending {
				t.Errorf("status after input provided = %s, want %s", got, AsyncTaskPending)
			}
		}
	}
	if len(history.Arcs) != 1 {
		t.Fatalf("arcs count = %d, want 1", len(history.Arcs))
	}
	if len(history.Arcs[0].AsyncTasks) != 1 {
		t.Fatalf("AsyncTasks count = %d, want 1", len(history.Arcs[0].AsyncTasks))
	}
	task := history.Arcs[0].AsyncTasks[0]
	if task.CorrelationID != arc {
		t.Errorf("task correlation ID = %s, want %s", task.CorrelationID, arc)
	}
	if task.Status != AsyncTaskSuccess {
		t.Errorf("task status = %s, want %s", task.Status, AsyncTaskSuccess)
	}
	if task.Result != "complete" {
		t.Errorf("task result = %#v, want complete", task.Result)
	}
	if len(task.Events) != 3 {
		t.Fatalf("task events count = %d, want 3", len(task.Events))
	}

	gotTypes := []AsyncTaskEventType{task.Events[0].Type, task.Events[1].Type, task.Events[2].Type}
	wantTypes := []AsyncTaskEventType{
		AsyncTaskEventNotification,
		AsyncTaskEventInputRequested,
		AsyncTaskEventInputProvided,
	}
	for i := range wantTypes {
		if gotTypes[i] != wantTypes[i] {
			t.Errorf("task event %d type = %s, want %s", i, gotTypes[i], wantTypes[i])
		}
	}
	if task.Events[0].Payload.(map[string]any)["message"] != "checking status" {
		t.Errorf("notification payload = %#v, want message", task.Events[0].Payload)
	}
	if task.Events[1].Payload != "Which namespace?" {
		t.Errorf("input request payload = %#v, want request message", task.Events[1].Payload)
	}
	if task.Events[2].Payload != "production" {
		t.Errorf("provided input payload = %#v, want provided message", task.Events[2].Payload)
	}
}

func TestHistoryFoldNestsEachUserMessageInItsOwnArc(t *testing.T) {
	start := time.Date(2026, time.September, 17, 16, 0, 0, 0, time.UTC)
	first := "arc-1"
	second := "arc-2"
	user := users.Actor{UserID: "user-1"}

	history := foldEvents(t, "session-1", []events.Event{
		{ID: first, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &first, Actor: user, Payload: events.UserMessagePayload{Content: "First", Actor: user}},
		{ID: "assistant-message-1", Seq: 1, Timestamp: start.Add(time.Second), Typ: events.AssistantMessageEventType, CorrelationID: &first, Payload: events.AssistantMessagePayload{ModelID: "model-1", Content: "Answering first."}},
		{ID: second, Seq: 2, Timestamp: start.Add(2 * time.Second), Typ: events.UserMessageEventType, CorrelationID: &second, Actor: user, Payload: events.UserMessagePayload{Content: "Second", Actor: user}},
		{ID: "assistant-message-2", Seq: 3, Timestamp: start.Add(3 * time.Second), Typ: events.AssistantMessageEventType, CorrelationID: &second, Payload: events.AssistantMessagePayload{ModelID: "model-1", Content: "Answering second."}},
		{ID: "task-1", Seq: 4, Timestamp: start.Add(4 * time.Second), Typ: events.AsyncTaskInvokeEventType, CorrelationID: &first, Payload: events.AsyncTaskInvokePayload{TaskName: "researcher"}},
	})

	if len(history.Arcs) != 2 {
		t.Fatalf("arcs = %#v, want one arc per user message", history.Arcs)
	}
	if history.Arcs[0].Content != "First" || history.Arcs[1].Content != "Second" {
		t.Errorf("arcs = %q, %q, want them in the order the user sent them", history.Arcs[0].Content, history.Arcs[1].Content)
	}
	if len(history.Arcs[0].Responses) != 1 || history.Arcs[0].Responses[0].Content != "Answering first." {
		t.Errorf("first arc responses = %#v, want only the reply to the first message", history.Arcs[0].Responses)
	}
	if len(history.Arcs[1].Responses) != 1 || history.Arcs[1].Responses[0].Content != "Answering second." {
		t.Errorf("second arc responses = %#v, want only the reply to the second message", history.Arcs[1].Responses)
	}
	// A late event still lands on the arc it correlates to, not on the newest one.
	if len(history.Arcs[0].AsyncTasks) != 1 || len(history.Arcs[1].AsyncTasks) != 0 {
		t.Errorf("async tasks = %#v / %#v, want the task on the arc it correlates to", history.Arcs[0].AsyncTasks, history.Arcs[1].AsyncTasks)
	}
}

func TestFoldStateFromHistoryIndexesExistingRecords(t *testing.T) {
	arc := "arc-1"
	now := time.Date(2026, time.September, 17, 15, 0, 0, 0, time.UTC)
	user := users.Actor{UserID: "user-1"}
	history := &History{
		LastSeq: 0,
		Arcs: []*Arc{{
			ID: "arc-1", Seq: 0, Content: "Check status", Timestamp: now, Actor: &user,
			Responses: []*AssistantMessage{{
				ID: "assistant-message-1", Seq: 0, CorrelationID: arc, Timestamp: now,
				ToolCalls: []*ToolCall{{
					ID: "tool-1", Status: ToolCallStatusPending,
					Interrupt: &Interrupt{ID: "interrupt-1", CorrelationID: arc, ToolCallID: "tool-1", Timestamp: now, Status: InterruptPending},
				}},
			}},
			AsyncTasks: []*AsyncTask{{ID: "task-1", CorrelationID: arc, Status: AsyncTaskPending}},
		}},
	}
	state := FoldStateFromHistory(history)
	updates := []events.Event{
		{ID: "tool-result-1", Seq: 1, Timestamp: now, Typ: events.ToolResultEventType, CorrelationID: &arc, Payload: events.ToolResultPayload{CausationID: "tool-1", Result: "done"}},
		{ID: "interrupt-response-1", Seq: 2, Timestamp: now, Typ: events.InterruptResponseEventType, CorrelationID: &arc, Actor: user, Payload: events.InterruptResponsePayload{CausationID: "interrupt-1", Response: "approved", Actor: user}},
		{ID: "task-result-1", Seq: 3, Timestamp: now, Typ: events.AsyncTaskResultEventType, CorrelationID: &arc, Payload: events.AsyncTaskResultPayload{CausationID: "task-1", Result: "done", Outcome: events.AsyncTaskOutcomeSuccess}},
		{ID: "tool-call-2", Seq: 4, Timestamp: now, Typ: events.ToolCallEventType, CorrelationID: &arc, Payload: events.ToolCallPayload{MessageID: "assistant-message-1", ToolName: "lookup"}},
		{ID: "tool-dispatched-1", Seq: 5, Timestamp: now, Typ: events.ToolDispatchedEventType, CorrelationID: &arc, Payload: events.ToolDispatchedPayload{CausationID: "tool-call-2"}},
		{ID: "error-1", Seq: 6, Timestamp: now, Typ: events.ErrorEventType, CorrelationID: &arc, Payload: events.ErrorPayload{Message: "recorded failure"}},
	}
	for i := range updates {
		if err := history.Update(&updates[i], state); err != nil {
			t.Fatalf("Update(existing record event %d) error = %v", i, err)
		}
	}

	got := history.Arcs[0]
	if got.Responses[0].ToolCalls[0].Status != ToolCallStatusSuccess {
		t.Errorf("existing tool call was not indexed for updates: %#v", got.Responses[0].ToolCalls[0])
	}
	if len(got.Responses[0].ToolCalls) != 2 {
		t.Fatalf("existing assistant message was not indexed for new tool calls: %#v", got.Responses[0].ToolCalls)
	}
	if got.Responses[0].ToolCalls[1].Dispatches != 1 {
		t.Errorf("new tool call was not indexed for dispatches: %#v", got.Responses[0].ToolCalls[1])
	}
	if interrupt := got.Responses[0].ToolCalls[0].Interrupt; interrupt.Status != InterruptApproved {
		t.Errorf("existing interrupt was not indexed for updates: %#v", interrupt)
	}
	if got.AsyncTasks[0].Status != AsyncTaskSuccess {
		t.Errorf("existing async task was not indexed for updates: %#v", got.AsyncTasks[0])
	}
	if len(got.Errors) != 1 {
		t.Errorf("existing arc was not indexed for new errors: %#v", got.Errors)
	}
}

func TestToolDispatchRecordsEveryAttemptAndKeepsTheFirst(t *testing.T) {
	start := time.Date(2026, time.September, 22, 9, 0, 0, 0, time.UTC)
	arc := "arc-1"
	user := users.Actor{UserID: "user-1"}
	log := []events.Event{
		{ID: arc, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: user, Payload: events.UserMessagePayload{Content: "Restart it", Actor: user}},
		{ID: "assistant-message-1", Seq: 1, Timestamp: start.Add(time.Second), Typ: events.AssistantMessageEventType, CorrelationID: &arc, Payload: events.AssistantMessagePayload{Content: "Restarting."}},
		{ID: "tool-call-1", Seq: 2, Timestamp: start.Add(2 * time.Second), Typ: events.ToolCallEventType, CorrelationID: &arc, Payload: events.ToolCallPayload{MessageID: "assistant-message-1", ToolName: "restart"}},
		{ID: "tool-dispatched-1", Seq: 3, Timestamp: start.Add(3 * time.Second), Typ: events.ToolDispatchedEventType, CorrelationID: &arc, Payload: events.ToolDispatchedPayload{CausationID: "tool-call-1"}},
		{ID: "tool-dispatched-2", Seq: 4, Timestamp: start.Add(4 * time.Second), Typ: events.ToolDispatchedEventType, CorrelationID: &arc, Payload: events.ToolDispatchedPayload{CausationID: "tool-call-1"}},
	}

	call := foldEvents(t, "session-1", log).Arcs[0].Responses[0].ToolCalls[0]
	if call.Dispatches != 2 {
		t.Errorf("dispatches = %d, want both attempts counted", call.Dispatches)
	}
	if call.DispatchedSeq == nil || *call.DispatchedSeq != 3 {
		t.Errorf("dispatched seq = %v, want the first attempt", call.DispatchedSeq)
	}
	if call.DispatchedAt == nil || !call.DispatchedAt.Equal(start.Add(3*time.Second)) {
		t.Errorf("dispatched at = %v, want the first attempt", call.DispatchedAt)
	}
	if call.Status != ToolCallStatusPending {
		t.Errorf("status = %s, want a dispatch to leave the status alone", call.Status)
	}
	if call.Phase() != ToolCallPhaseDispatched {
		t.Errorf("phase = %s, want %s", call.Phase(), ToolCallPhaseDispatched)
	}
}

// A cancel sweeping an arc races the worker dispatching one of its calls, and
// the dispatch can land second. It is still recorded, because a call that may
// have taken effect is exactly what the marker is for.
func TestToolDispatchIsRecordedAfterTheCallWasSettled(t *testing.T) {
	start := time.Date(2026, time.September, 22, 10, 0, 0, 0, time.UTC)
	arc := "arc-1"
	user := users.Actor{UserID: "user-1"}
	log := []events.Event{
		{ID: arc, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: user, Payload: events.UserMessagePayload{Content: "Restart it", Actor: user}},
		{ID: "assistant-message-1", Seq: 1, Timestamp: start.Add(time.Second), Typ: events.AssistantMessageEventType, CorrelationID: &arc, Payload: events.AssistantMessagePayload{Content: "Restarting."}},
		{ID: "tool-call-1", Seq: 2, Timestamp: start.Add(2 * time.Second), Typ: events.ToolCallEventType, CorrelationID: &arc, Payload: events.ToolCallPayload{MessageID: "assistant-message-1", ToolName: "restart"}},
		{ID: "cancel-1", Seq: 3, Timestamp: start.Add(3 * time.Second), Typ: events.CancelEventType, CorrelationID: &arc, Actor: user, Payload: events.CancelPayload{}},
		{ID: "tool-dispatched-1", Seq: 4, Timestamp: start.Add(4 * time.Second), Typ: events.ToolDispatchedEventType, CorrelationID: &arc, Payload: events.ToolDispatchedPayload{CausationID: "tool-call-1"}},
	}

	call := foldEvents(t, "session-1", log).Arcs[0].Responses[0].ToolCalls[0]
	if call.Dispatches != 1 || call.DispatchedSeq == nil {
		t.Errorf("call = %#v, want the dispatch recorded against a settled call", call)
	}
	if call.Status != ToolCallStatusCancelled {
		t.Errorf("status = %s, want the cancel to stand", call.Status)
	}
	if call.Phase() != ToolCallPhaseSettled {
		t.Errorf("phase = %s, want %s once the call has an outcome", call.Phase(), ToolCallPhaseSettled)
	}
}

func TestToolCallPhaseSeparatesTheWaysACallCanBeUnsettled(t *testing.T) {
	now := time.Date(2026, time.September, 22, 11, 0, 0, 0, time.UTC)
	seq := 7
	cases := []struct {
		name string
		call ToolCall
		want ToolCallPhase
	}{
		{"requested", ToolCall{Status: ToolCallStatusPending}, ToolCallPhaseRequested},
		{"awaiting approval", ToolCall{Status: ToolCallStatusPending, Interrupt: &Interrupt{Status: InterruptPending}}, ToolCallPhaseAwaitingApproval},
		{"dispatched", ToolCall{Status: ToolCallStatusPending, DispatchedSeq: &seq, DispatchedAt: &now}, ToolCallPhaseDispatched},
		{"dispatched after approval", ToolCall{Status: ToolCallStatusPending, DispatchedSeq: &seq, Interrupt: &Interrupt{Status: InterruptApproved}}, ToolCallPhaseDispatched},
		{"settled", ToolCall{Status: ToolCallStatusSuccess, DispatchedSeq: &seq, FinishedSeq: &seq}, ToolCallPhaseSettled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.call.Phase(); got != tc.want {
				t.Errorf("Phase() = %s, want %s", got, tc.want)
			}
		})
	}
}

func foldEvents(t *testing.T, sessionID string, log []events.Event) *History {
	t.Helper()
	history := NewHistory(sessionID)
	state := NewFoldState()
	for i := range log {
		if err := history.Update(&log[i], state); err != nil {
			t.Fatalf("Update(event %d, type %s) error = %v", i, log[i].Typ, err)
		}
	}
	return history
}

func stringPtr(value string) *string {
	return &value
}

func TestArcClosedRecordsWhatClosedTheArc(t *testing.T) {
	start := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	arc := "arc-1"
	user := users.Actor{UserID: "user-1"}

	log := []events.Event{
		{ID: arc, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: user, Payload: events.UserMessagePayload{Content: "how many pods are pending?", Actor: user}},
		{ID: "message-1", Seq: 1, Timestamp: start.Add(time.Second), Typ: events.AssistantMessageEventType, CorrelationID: &arc, Payload: events.AssistantMessagePayload{ModelID: "model-1", Content: "six"}},
		{ID: "closed-1", Seq: 2, Timestamp: start.Add(2 * time.Second), Typ: events.ArcClosedEventType, CorrelationID: &arc, Payload: events.ArcClosedPayload{CausationID: "message-1"}},
		// A second close is dropped, so the first one sticks.
		{ID: "closed-2", Seq: 3, Timestamp: start.Add(3 * time.Second), Typ: events.ArcClosedEventType, CorrelationID: &arc, Payload: events.ArcClosedPayload{CausationID: "message-1"}},
	}

	got := foldEvents(t, "session-1", log).Arcs[0]
	if got.Closed == nil {
		t.Fatal("arc closed = nil, want the close the agent recorded")
	}
	if got.Closed.ID != "closed-1" || got.Closed.Seq != 2 || got.Closed.CausationID != "message-1" {
		t.Errorf("arc closed = %#v, want the first close and the message that caused it", got.Closed)
	}
	if status := got.Status(); status != ArcStatusDone {
		t.Errorf("arc status = %s, want %s", status, ArcStatusDone)
	}
}

func TestArcClosedNeedsAnArcAndAMessageItCanName(t *testing.T) {
	start := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	arc := "arc-1"
	other := "arc-2"
	user := users.Actor{UserID: "user-1"}

	opening := []events.Event{
		{ID: arc, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: user, Payload: events.UserMessagePayload{Content: "how many pods are pending?", Actor: user}},
		{ID: "message-1", Seq: 1, Timestamp: start.Add(time.Second), Typ: events.AssistantMessageEventType, CorrelationID: &arc, Payload: events.AssistantMessagePayload{ModelID: "model-1", Content: "six"}},
	}

	for name, closing := range map[string]events.Event{
		"unknown message": {ID: "closed-1", Seq: 2, Timestamp: start, Typ: events.ArcClosedEventType, CorrelationID: &arc, Payload: events.ArcClosedPayload{CausationID: "nope"}},
		"unknown arc":     {ID: "closed-1", Seq: 2, Timestamp: start, Typ: events.ArcClosedEventType, CorrelationID: &other, Payload: events.ArcClosedPayload{CausationID: "message-1"}},
	} {
		history := foldEvents(t, "session-1", opening)
		state := FoldStateFromHistory(history)
		if err := history.Update(&closing, state); err == nil {
			t.Errorf("Update(close naming an %s) error = nil, want an error", name)
		}
		if history.Arcs[0].Closed != nil {
			t.Errorf("arc closed = %#v after a rejected close, want nil", history.Arcs[0].Closed)
		}
	}
}

func TestArcStatusFollowsTheFirstClosingRecord(t *testing.T) {
	now := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	answer := &AssistantMessage{ID: "message-1", Seq: 1, CorrelationID: "arc-1", Timestamp: now}
	closed := &ArcClosed{ID: "closed-1", Seq: 2, Timestamp: now, CausationID: "message-1"}

	for name, testCase := range map[string]struct {
		arc  Arc
		want ArcStatus
	}{
		"a reply alone does not close an arc": {
			arc:  Arc{ID: "arc-1", Responses: []*AssistantMessage{answer}},
			want: ArcStatusPending,
		},
		"a call waiting on a human holds it open": {
			arc: Arc{ID: "arc-1", Responses: []*AssistantMessage{answer, {
				ID: "message-2", Seq: 2, CorrelationID: "arc-1",
				ToolCalls: []*ToolCall{{
					ID: "tool-1", Seq: 3, Status: ToolCallStatusPending,
					Interrupt: &Interrupt{ID: "interrupt-1", Seq: 4, ToolCallID: "tool-1", Status: InterruptPending},
				}},
			}}},
			want: ArcStatusInterrupted,
		},
		"a close finishes it": {
			arc:  Arc{ID: "arc-1", Responses: []*AssistantMessage{answer}, Closed: closed},
			want: ArcStatusDone,
		},
		"a cancel after the close stopped nothing": {
			arc:  Arc{ID: "arc-1", Closed: closed, Cancel: &Cancel{ID: "cancel-1", Seq: 3}},
			want: ArcStatusDone,
		},
		"a cancel before the close closes it": {
			arc:  Arc{ID: "arc-1", Closed: &ArcClosed{ID: "closed-1", Seq: 4}, Cancel: &Cancel{ID: "cancel-1", Seq: 3}},
			want: ArcStatusCancelled,
		},
		"a fatal error closes it": {
			arc:  Arc{ID: "arc-1", Errors: []*Error{{ID: "error-1", Seq: 2, IsFatal: true}}},
			want: ArcStatusError,
		},
		"a non-fatal error does not": {
			arc:  Arc{ID: "arc-1", Errors: []*Error{{ID: "error-1", Seq: 2}}},
			want: ArcStatusPending,
		},
	} {
		if got := testCase.arc.Status(); got != testCase.want {
			t.Errorf("%s: status = %s, want %s", name, got, testCase.want)
		}
	}
}

func TestCancelTerminatedWorkFollowsTheClose(t *testing.T) {
	start := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	user := users.Actor{UserID: "user-1"}

	// The same log twice, once with the close the agent records when nothing
	// is left running and once without it.
	build := func(t *testing.T, closeIt bool) *Arc {
		t.Helper()
		arc := "arc-1"
		log := []events.Event{
			{ID: arc, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: user, Payload: events.UserMessagePayload{Content: "audit the cluster", Actor: user}},
			{ID: "message-1", Seq: 1, Timestamp: start.Add(time.Second), Typ: events.AssistantMessageEventType, CorrelationID: &arc, Payload: events.AssistantMessagePayload{ModelID: "model-1", Content: "started the audit"}},
		}
		next := 2
		if closeIt {
			log = append(log, events.Event{ID: "closed-1", Seq: next, Timestamp: start.Add(2 * time.Second), Typ: events.ArcClosedEventType, CorrelationID: &arc, Payload: events.ArcClosedPayload{CausationID: "message-1"}})
			next++
		}
		log = append(log, events.Event{ID: "cancel-1", Seq: next, Timestamp: start.Add(time.Duration(next) * time.Second), Typ: events.CancelEventType, CorrelationID: &arc, Actor: user, Payload: events.CancelPayload{}})
		return foldEvents(t, "session-1", log).Arcs[0]
	}

	if cancel := build(t, true).Cancel; cancel.TerminatedWork {
		t.Errorf("cancel = %#v, want TerminatedWork false after the arc was closed", cancel)
	}
	if cancel := build(t, false).Cancel; !cancel.TerminatedWork {
		t.Errorf("cancel = %#v, want TerminatedWork true for an arc the agent never closed", cancel)
	}
}

// A cancel naming an arc the fold never saw is a broken log, and the fold has
// to say so rather than dereference the arc it did not find.
func TestCancelForAnUnknownArcIsRejected(t *testing.T) {
	start := time.Date(2026, time.September, 19, 10, 0, 0, 0, time.UTC)
	user := users.Actor{UserID: "user-1"}

	known, unknown := "arc-1", "arc-2"
	log := []events.Event{
		{ID: known, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &known, Actor: user, Payload: events.UserMessagePayload{Content: "audit the cluster", Actor: user}},
		{ID: "cancel-1", Seq: 1, Timestamp: start.Add(time.Second), Typ: events.CancelEventType, CorrelationID: &unknown, Actor: user, Payload: events.CancelPayload{}},
	}

	if _, err := Replay("session-1", log); err == nil {
		t.Fatal("a cancel against an arc that is not in the log was folded without complaint")
	}
}

// A result has to say how the task ended. The flag it replaced could be left
// unset and still mean something; an outcome cannot, so a result that carries
// none is refused rather than read as a success.
func TestAnAsyncTaskResultWithoutAnOutcomeIsRefused(t *testing.T) {
	start := time.Date(2026, time.September, 24, 9, 0, 0, 0, time.UTC)
	arc := "user-message-1"
	actor := users.Actor{UserID: "user-1", TenantID: "tenant-1"}

	log := []events.Event{
		{ID: arc, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: actor,
			Payload: events.UserMessagePayload{Content: "audit the cluster", Actor: actor}},
		{ID: "task-1", Seq: 1, Timestamp: start, Typ: events.AsyncTaskInvokeEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskInvokePayload{TaskName: "auditor"}},
		{ID: "task-result-1", Seq: 2, Timestamp: start, Typ: events.AsyncTaskResultEventType, CorrelationID: &arc,
			Payload: events.AsyncTaskResultPayload{CausationID: "task-1", Result: "healthy"}},
	}

	if _, err := Replay("session-1", log); err == nil {
		t.Fatal("a result carrying no outcome was folded")
	}
}

// TestFoldRefusesAnEventItCannotAttach covers the events that name their arc
// by correlation ID and nothing else. Each of them has to fail the fold rather
// than panic inside it: a correlation ID that names no arc is a malformed log,
// and the fold is the one place that can say which event broke it.
func TestFoldRefusesAnEventItCannotAttach(t *testing.T) {
	start := time.Date(2026, time.September, 27, 9, 0, 0, 0, time.UTC)
	arc := "arc-1"
	missing := "arc-nobody-opened"
	user := users.Actor{UserID: "user-1"}

	opening := events.Event{
		ID: arc, Seq: 0, Timestamp: start, Typ: events.UserMessageEventType, CorrelationID: &arc,
		Actor: user, Payload: events.UserMessagePayload{Content: "how many pods are pending?", Actor: user},
	}

	cases := []struct {
		name    string
		arcID   *string
		typ     events.EventType
		payload events.EventPayload
	}{
		{"assistant message for an arc that is not there", &missing, events.AssistantMessageEventType,
			events.AssistantMessagePayload{ModelID: "model-1", Content: "six"}},
		{"assistant message with no correlation ID", nil, events.AssistantMessageEventType,
			events.AssistantMessagePayload{ModelID: "model-1", Content: "six"}},
		{"task invocation for an arc that is not there", &missing, events.AsyncTaskInvokeEventType,
			events.AsyncTaskInvokePayload{TaskName: "auditor"}},
		{"task invocation with no correlation ID", nil, events.AsyncTaskInvokeEventType,
			events.AsyncTaskInvokePayload{TaskName: "auditor"}},
		{"error for an arc that is not there", &missing, events.ErrorEventType,
			events.ErrorPayload{Message: "recorded failure"}},
		{"error with no correlation ID", nil, events.ErrorEventType,
			events.ErrorPayload{Message: "recorded failure"}},
		{"user message with no correlation ID", nil, events.UserMessageEventType,
			events.UserMessagePayload{Content: "and the nodes?", Actor: user}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			history := foldEvents(t, "session-1", []events.Event{opening})
			state := FoldStateFromHistory(history)
			ev := events.Event{
				ID: "unattachable-1", Seq: 1, Timestamp: start.Add(time.Second),
				Typ: tc.typ, CorrelationID: tc.arcID, Actor: user, Payload: tc.payload,
			}

			if err := history.Update(&ev, state); err == nil {
				t.Fatalf("Update(%s) error = nil, want the fold to refuse an event it cannot attach", tc.typ)
			}
			// A refused event leaves the history where it was, so the next
			// event is still judged against the last one that was applied.
			if history.LastSeq != 0 {
				t.Errorf("LastSeq = %d, want 0 after a refused event", history.LastSeq)
			}
			if len(history.Arcs) != 1 {
				t.Errorf("arcs = %d, want the one arc that was opened", len(history.Arcs))
			}
			if len(history.Arcs[0].Responses) != 0 || len(history.Arcs[0].Errors) != 0 ||
				len(history.Arcs[0].AsyncTasks) != 0 {
				t.Errorf("arc = %#v, want nothing attached to it", history.Arcs[0])
			}
		})
	}
}
