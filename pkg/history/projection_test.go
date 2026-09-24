package history

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/users"
)

// The scenarios in projection-scenarios/ are the worked examples these tests
// check against. Each test that reproduces one names it.

var testActor = users.Actor{UserID: "user-1", TenantID: "tenant-1"}

// logger builds an event log in sequence order, so a test reads like the
// Events line of a scenario file.
type logger struct {
	seq    int
	start  time.Time
	events []events.Event
}

func newLogger() *logger {
	return &logger{start: time.Date(2026, time.September, 19, 9, 0, 0, 0, time.UTC)}
}

func (l *logger) add(id, arc string, typ events.EventType, payload events.EventPayload) {
	correlation := arc
	l.events = append(l.events, events.Event{
		SessionID:     "session-1",
		ID:            id,
		Seq:           l.seq,
		Timestamp:     l.start.Add(time.Duration(l.seq) * time.Second),
		Typ:           typ,
		CorrelationID: &correlation,
		Actor:         testActor,
		Payload:       payload,
	})
	l.seq++
}

func (l *logger) user(arc, content string) {
	l.add(arc, arc, events.UserMessageEventType, events.UserMessagePayload{Content: content, Actor: testActor})
}

func (l *logger) assistant(arc, id, content string) {
	l.add(id, arc, events.AssistantMessageEventType, events.AssistantMessagePayload{ModelID: "model-1", Content: content})
}

func (l *logger) assistantWithReasoning(arc, id, content string, reasoning []byte) {
	l.add(id, arc, events.AssistantMessageEventType, events.AssistantMessagePayload{ModelID: "model-1", Content: content, Reasoning: reasoning})
}

func (l *logger) toolCall(arc, message, id, name string, args any) {
	l.add(id, arc, events.ToolCallEventType, events.ToolCallPayload{MessageID: message, VendorID: "vendor-" + id, ToolName: name, Args: args})
}

func (l *logger) toolResult(arc, call string, result any, isError bool) {
	l.add("result-"+call, arc, events.ToolResultEventType, events.ToolResultPayload{CausationID: call, Result: result, IsError: isError})
}

func (l *logger) toolEvent(arc, call, id string, payload any) {
	l.add(id, arc, events.ToolEventEventType, events.ToolEventPayload{CausationID: call, Payload: payload})
}

func (l *logger) toolDispatched(arc, call string) {
	l.add("dispatched-"+call, arc, events.ToolDispatchedEventType, events.ToolDispatchedPayload{CausationID: call})
}

func (l *logger) guardrail(arc, call, message string) {
	l.add("block-"+call, arc, events.GuardrailBlockedEventType, events.GuardrailBlockedPayload{CausationID: call, Message: message})
}

func (l *logger) failure(arc, id, message string, fatal bool) {
	l.add(id, arc, events.ErrorEventType, events.ErrorPayload{Message: message, IsFatal: fatal})
}

func (l *logger) closeArc(arc, message string) {
	l.add("closed-"+arc, arc, events.ArcClosedEventType, events.ArcClosedPayload{CausationID: message})
}

func (l *logger) cancel(arc, id string, reason *string) {
	l.add(id, arc, events.CancelEventType, events.CancelPayload{Reason: reason})
}

func (l *logger) interrupt(arc, id, call, reason string) {
	l.add(id, arc, events.InterruptEventType, events.InterruptPayload{ToolCallID: call, Reason: reason})
}

func (l *logger) interruptWithContext(arc, id, call, reason string, context any) {
	l.add(id, arc, events.InterruptEventType, events.InterruptPayload{ToolCallID: call, Reason: reason, Context: context})
}

func (l *logger) interruptResponse(arc, interrupt string, rejected bool) {
	l.add("response-"+interrupt, arc, events.InterruptResponseEventType, events.InterruptResponsePayload{CausationID: interrupt, IsRejected: rejected, Actor: testActor})
}

func (l *logger) task(arc, id, name string, args any) {
	l.add(id, arc, events.AsyncTaskInvokeEventType, events.AsyncTaskInvokePayload{TaskName: name, Args: args})
}

func (l *logger) taskEvent(arc, task, id string, payload any) {
	l.add(id, arc, events.AsyncTaskEventEventType, events.AsyncTaskEventPayload{CausationID: task, Payload: payload})
}

func (l *logger) taskAsk(arc, task, id, message string) {
	l.add(id, arc, events.AsyncTaskInputRequestedEventType, events.AsyncTaskInputRequestedPayload{CausationID: task, Message: message})
}

func (l *logger) taskAnswer(arc, task, id, message string) {
	l.add(id, arc, events.AsyncTaskInputProvidedEventType, events.AsyncTaskInputProvidedPayload{CausationID: task, Message: message})
}

func (l *logger) taskResult(arc, task string, result any, isError bool) {
	l.add("result-"+task, arc, events.AsyncTaskResultEventType, events.AsyncTaskResultPayload{CausationID: task, Result: result, IsError: isError})
}

func (l *logger) history(t *testing.T) *History {
	t.Helper()
	return foldEvents(t, "session-1", l.events)
}

// eventsFor returns the events a read model hands the projection for one arc:
// everything up to that arc's user message, plus everything correlated to it
// however late it arrived. This query is the projection's cutoff, so a test
// projects from the slice rather than from the whole log.
func (l *logger) eventsFor(arc string) []events.Event {
	opened := -1
	for _, event := range l.events {
		if event.ID == arc {
			opened = event.Seq
			break
		}
	}
	var out []events.Event
	for _, event := range l.events {
		if event.Seq <= opened || (event.CorrelationID != nil && *event.CorrelationID == arc) {
			out = append(out, event)
		}
	}
	return out
}

// project folds the slice for one arc and projects it, failing the test if
// either step errors.
func project(t *testing.T, log *logger, arc string, modes Modes) *ModelTranscript {
	t.Helper()
	transcript, err := foldEvents(t, "session-1", log.eventsFor(arc)).Project(arc, modes)
	if err != nil {
		t.Fatalf("Project(%s) error = %v", arc, err)
	}
	return transcript
}

// shape renders a transcript in the compact form the scenario files use, with
// note contents left out so a sequence assertion does not depend on wording.
func shape(transcript *ModelTranscript) []string {
	out := make([]string, 0, len(transcript.Messages))
	for _, message := range transcript.Messages {
		switch typed := message.(type) {
		case ModelTextMessage:
			if typed.MessageRole == ModelRoleInstruction {
				out = append(out, "s{}")
				continue
			}
			out = append(out, fmt.Sprintf("u(%s)", typed.Content))
		case ModelAssistantMessage:
			calls := make([]string, 0, len(typed.ToolCalls))
			for _, call := range typed.ToolCalls {
				calls = append(calls, fmt.Sprintf("t(%s)", call.ID))
			}
			rendered := fmt.Sprintf("a(%s)", typed.Content)
			if len(calls) > 0 {
				rendered += "[" + strings.Join(calls, " ") + "]"
			}
			out = append(out, rendered)
		case ModelToolMessage:
			outcome := map[ModelToolOutcome]string{
				ModelToolOutcomeSuccess:   "ok",
				ModelToolOutcomeFailed:    "err",
				ModelToolOutcomeBlocked:   "blocked",
				ModelToolOutcomeRefused:   "refused",
				ModelToolOutcomeCancelled: "cancel",
				ModelToolOutcomePending:   "pending",
			}[typed.Outcome]
			out = append(out, fmt.Sprintf("r(%s:%s)", typed.ToolCallID, outcome))
		case ModelNoteMessage:
			out = append(out, "n{}")
		}
	}
	return out
}

func wantShape(t *testing.T, transcript *ModelTranscript, want ...string) {
	t.Helper()
	got := shape(transcript)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("transcript =\n  %s\nwant\n  %s", strings.Join(got, " "), strings.Join(want, " "))
	}
}

// noteTexts returns every note's content, for the assertions that are about
// what a note says rather than where it sits.
func noteTexts(transcript *ModelTranscript) []string {
	var out []string
	for _, message := range transcript.Messages {
		if note, ok := message.(ModelNoteMessage); ok {
			out = append(out, note.Content)
		}
	}
	return out
}

// dispatchedCall returns one call as the projection for arc saw it, so a test
// can check its own setup landed inside the cutoff.
func dispatchedCall(t *testing.T, log *logger, arc, call string) *ToolCall {
	t.Helper()
	history := foldEvents(t, "session-1", log.eventsFor(arc))
	for _, folded := range history.Arcs {
		for _, response := range folded.Responses {
			for _, candidate := range response.ToolCalls {
				if candidate.ID == call {
					return candidate
				}
			}
		}
	}
	t.Fatalf("no tool call %s in the history projected for %s", call, arc)
	return nil
}

// assistantMessages returns the model's own messages, for assertions about
// what a message carries rather than where it sits.
func assistantMessages(transcript *ModelTranscript) []ModelAssistantMessage {
	var out []ModelAssistantMessage
	for _, message := range transcript.Messages {
		if assistant, ok := message.(ModelAssistantMessage); ok {
			out = append(out, assistant)
		}
	}
	return out
}

// toolTexts returns what every tool message says, for the assertions that are
// about wording rather than shape.
func toolTexts(transcript *ModelTranscript) []string {
	var out []string
	for _, message := range transcript.Messages {
		if tool, ok := message.(ModelToolMessage); ok {
			out = append(out, tool.Content)
		}
	}
	return out
}

func wantNoteContaining(t *testing.T, transcript *ModelTranscript, substring string) {
	t.Helper()
	for _, text := range noteTexts(transcript) {
		if strings.Contains(text, substring) {
			return
		}
	}
	t.Errorf("notes = %q, want one containing %q", noteTexts(transcript), substring)
}

// openArcLog is the log of scenarios 01 through 03: one open arc, one new one.
func openArcLog() *logger {
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.user("u2", "also, describe node n1")
	return log
}

// pendingCallLog is the log of scenario 04.
func pendingCallLog() *logger {
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.assistant("u1", "r1.1", "let me check the cluster")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", map[string]any{"kind": "pod"})
	log.user("u2", "also, describe node n1")
	return log
}

func TestForeignArcModeGovernsAnOpenArc(t *testing.T) {
	log := openArcLog()

	for _, testCase := range []struct {
		name string
		mode Treatment
		want []string
	}{
		// 01-open-arc-kept.md
		{"keep", TreatmentKeep, []string{"u(how many pods are pending?)", "n{}", "u(also, describe node n1)"}},
		// 02-open-arc-noted.md
		{"note", TreatmentNote, []string{"n{}", "u(also, describe node n1)"}},
		// 03-open-arc-elided.md
		{"elide", TreatmentElide, []string{"u(also, describe node n1)"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			modes := DefaultModes()
			modes.ForeignArc = testCase.mode
			transcript := project(t, log, "u2", modes)
			wantShape(t, transcript, testCase.want...)
			// Both disclosing modes name the request the note is about. Under
			// keep that is what separates the foreign user turn from the one
			// this turn has to answer.
			if testCase.mode != TreatmentElide {
				wantNoteContaining(t, transcript, "how many pods are pending?")
			}
		})
	}
}

func TestClosedForeignArcIsOrdinaryHistoryWhateverTheModeSays(t *testing.T) {
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.assistant("u1", "r1.1", "six")
	log.closeArc("u1", "r1.1")
	log.user("u2", "describe node n1")

	modes := DefaultModes()
	modes.ForeignArc = TreatmentElide
	transcript := project(t, log, "u2", modes)
	wantShape(t, transcript,
		"u(how many pods are pending?)",
		"a(six)",
		"u(describe node n1)")
}

func TestPendingModeGovernsACallWithNoResult(t *testing.T) {
	log := pendingCallLog()

	for _, testCase := range []struct {
		name string
		mode Treatment
		want []string
	}{
		// 04-pending-call.md
		{"stand_in", TreatmentStandIn, []string{
			"u(how many pods are pending?)",
			"a(let me check the cluster)[t(t1.1.1)]",
			"r(t1.1.1:pending)",
			"n{}",
			"u(also, describe node n1)",
		}},
		{"note", TreatmentNote, []string{
			"u(how many pods are pending?)",
			"a(let me check the cluster)",
			"n{}",
			"n{}",
			"u(also, describe node n1)",
		}},
		{"elide", TreatmentElide, []string{
			"u(how many pods are pending?)",
			"a(let me check the cluster)",
			"n{}",
			"u(also, describe node n1)",
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			modes := DefaultModes()
			modes.Pending = testCase.mode
			wantShape(t, project(t, log, "u2", modes), testCase.want...)
		})
	}
}

func TestPendingStandInIsSynthesizedAndAnswersEveryCall(t *testing.T) {
	log := newLogger()
	log.user("u1", "clean up")
	log.assistant("u1", "r1.1", "on it")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", nil)
	log.toolCall("u1", "r1.1", "t1.1.2", "kubectl_delete", nil)
	log.user("u2", "never mind")
	transcript := project(t, log, "u2", DefaultModes())

	// The stand-in is per call, the note is per arc.
	wantShape(t, transcript,
		"u(clean up)",
		"a(on it)[t(t1.1.1) t(t1.1.2)]",
		"r(t1.1.1:pending)",
		"r(t1.1.2:pending)",
		"n{}",
		"u(never mind)")
	if notes := noteTexts(transcript); len(notes) != 1 {
		t.Errorf("notes = %q, want one note for the arc", notes)
	}
}

func TestReasoningTravelsWithAMessageThatKeptEveryCall(t *testing.T) {
	reasoning := []byte("opaque-provider-bytes")
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.assistantWithReasoning("u1", "r1.1", "let me check the cluster", reasoning)
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", nil)
	log.toolResult("u1", "t1.1.1", "3 pending", false)
	log.closeArc("u1", "r1.1")
	log.user("u2", "also, describe node n1")

	got := assistantMessages(project(t, log, "u2", DefaultModes()))
	if len(got) != 1 {
		t.Fatalf("assistant messages = %d, want one", len(got))
	}
	if string(got[0].Reasoning) != string(reasoning) {
		t.Errorf("reasoning = %q, want the bytes the provider sent", got[0].Reasoning)
	}
}

// A mode that drops a call rewrites the message, and the reasoning no longer
// describes what the provider is sent. See the Reasoning section of
// PROJECTION.md.
func TestReasoningIsDroppedWhenAModeTookACallOutOfTheMessage(t *testing.T) {
	for _, mode := range []Treatment{TreatmentStandIn, TreatmentNote, TreatmentElide} {
		t.Run(mode.String(), func(t *testing.T) {
			reasoning := []byte("opaque-provider-bytes")
			log := newLogger()
			log.user("u1", "how many pods are pending?")
			log.assistantWithReasoning("u1", "r1.1", "let me check the cluster", reasoning)
			log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", nil)
			log.user("u2", "also, describe node n1")

			modes := DefaultModes()
			modes.Pending = mode
			got := assistantMessages(project(t, log, "u2", modes))
			if len(got) != 1 {
				t.Fatalf("assistant messages = %d, want one", len(got))
			}
			// stand_in answers the call in place, so the message keeps it.
			want := mode == TreatmentStandIn
			if kept := got[0].Reasoning != nil; kept != want {
				t.Errorf("reasoning kept = %v under %s, want %v", kept, mode, want)
			}
		})
	}
}

// A dispatch is execution bookkeeping and has no projected form at any mode.
// See the Execution records section of PROJECTION.md: at a cutoff the model can
// act on, a dispatched call and an undispatched one are both unanswered, and
// what the model needs to know arrives through the outcome of the call instead.
func TestDispatchIsNeverDisclosedUnderAnyMode(t *testing.T) {
	for _, mode := range []Treatment{TreatmentStandIn, TreatmentNote, TreatmentElide} {
		t.Run(mode.String(), func(t *testing.T) {
			modes := DefaultModes()
			modes.Pending = mode

			// The dispatch has to fall before u2 opens, or the cutoff
			// keeps it out of the projection and the comparison is vacuous.
			dispatched := newLogger()
			dispatched.user("u1", "how many pods are pending?")
			dispatched.assistant("u1", "r1.1", "let me check the cluster")
			dispatched.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", map[string]any{"kind": "pod"})
			dispatched.toolDispatched("u1", "t1.1.1")
			dispatched.user("u2", "also, describe node n1")

			want := project(t, pendingCallLog(), "u2", modes)
			got := project(t, dispatched, "u2", modes)
			if call := dispatchedCall(t, dispatched, "u2", "t1.1.1"); call.DispatchedSeq == nil {
				t.Fatalf("call = %#v, want the dispatch inside the projection's cutoff", call)
			}

			wantShape(t, got, shape(want)...)
			if !slices.Equal(noteTexts(got), noteTexts(want)) {
				t.Errorf("notes = %q, want the same text as an undispatched call: %q", noteTexts(got), noteTexts(want))
			}
			if !slices.Equal(toolTexts(got), toolTexts(want)) {
				t.Errorf("tool messages = %q, want the same text as an undispatched call: %q", toolTexts(got), toolTexts(want))
			}
		})
	}
}

func TestSettledCallsCarryTheirOutcome(t *testing.T) {
	log := newLogger()
	log.user("u1", "do four things")
	log.assistant("u1", "r1.1", "working")
	log.toolCall("u1", "r1.1", "ok", "kubectl_get", nil)
	log.toolCall("u1", "r1.1", "bad", "kubectl_get", nil)
	log.toolCall("u1", "r1.1", "blocked", "kubectl_delete", nil)
	log.toolResult("u1", "ok", "6 pods pending", false)
	log.toolResult("u1", "bad", "connection refused", true)
	log.guardrail("u1", "blocked", "deletion needs approval")
	log.assistant("u1", "r1.2", "done")
	log.closeArc("u1", "r1.2")
	log.user("u2", "thanks")
	transcript := project(t, log, "u2", DefaultModes())

	wantShape(t, transcript,
		"u(do four things)",
		"a(working)[t(ok) t(bad) t(blocked)]",
		"r(ok:ok)",
		"r(bad:err)",
		"r(blocked:blocked)",
		"a(done)",
		"u(thanks)")

	results := map[string]ModelToolMessage{}
	for _, message := range transcript.Messages {
		if result, isResult := message.(ModelToolMessage); isResult {
			results[result.ToolCallID] = result
		}
	}
	if results["ok"].Content != "6 pods pending" || results["ok"].IsError {
		t.Errorf("successful result = %#v", results["ok"])
	}
	if results["bad"].Content != "connection refused" || !results["bad"].IsError {
		t.Errorf("failed result = %#v", results["bad"])
	}
	if !strings.Contains(results["blocked"].Content, "deletion needs approval") || !results["blocked"].IsError {
		t.Errorf("blocked result = %#v, want the guardrail message", results["blocked"])
	}
	if !strings.Contains(results["blocked"].Content, "policy") {
		t.Errorf("blocked result = %q, want the policy named as what refused it", results["blocked"].Content)
	}
	if results["ok"].Outcome.Synthesized() {
		t.Error("a real result must not read as synthesized")
	}
}

func TestToolEventsModeGovernsProgressNotifications(t *testing.T) {
	log := newLogger()
	log.user("u1", "roll the deployment")
	log.assistant("u1", "r1.1", "rolling")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_rollout", nil)
	log.toolEvent("u1", "t1.1.1", "te1", "2 of 5 replicas updated")
	log.toolResult("u1", "t1.1.1", "rolled out", false)
	log.assistant("u1", "r1.2", "done")
	log.closeArc("u1", "r1.2")
	log.user("u2", "thanks")

	modes := DefaultModes()
	modes.ToolEvents = TreatmentElide
	wantShape(t, project(t, log, "u2", modes),
		"u(roll the deployment)", "a(rolling)[t(t1.1.1)]", "r(t1.1.1:ok)", "a(done)", "u(thanks)")

	modes.ToolEvents = TreatmentNote
	noted := project(t, log, "u2", modes)
	wantShape(t, noted,
		"u(roll the deployment)", "a(rolling)[t(t1.1.1)]", "r(t1.1.1:ok)", "n{}", "a(done)", "u(thanks)")
	wantNoteContaining(t, noted, "2 of 5 replicas updated")
}

func TestAsyncEventsModeGovernsTaskNotifications(t *testing.T) {
	log := newLogger()
	log.user("u1", "audit the cluster")
	log.assistant("u1", "r1.1", "started the audit")
	log.task("u1", "a1.1", "auditor", nil)
	log.taskEvent("u1", "a1.1", "ae1", "scanned 40 namespaces")
	log.taskResult("u1", "a1.1", "no findings", false)
	log.assistant("u1", "r1.2", "audit clean")
	log.closeArc("u1", "r1.2")
	log.user("u2", "thanks")

	modes := DefaultModes()
	modes.AsyncEvents = TreatmentElide
	elided := project(t, log, "u2", modes)
	// A task is always two notes, whatever async_events says.
	wantShape(t, elided,
		"u(audit the cluster)", "a(started the audit)", "n{}", "n{}", "a(audit clean)", "u(thanks)")

	modes.AsyncEvents = TreatmentNote
	noted := project(t, log, "u2", modes)
	wantShape(t, noted,
		"u(audit the cluster)", "a(started the audit)", "n{}", "n{}", "n{}", "a(audit clean)", "u(thanks)")
	wantNoteContaining(t, noted, "scanned 40 namespaces")
	wantNoteContaining(t, noted, "started")
	wantNoteContaining(t, noted, "no findings")
}

func TestAsyncInputModeGovernsATaskAskingAHuman(t *testing.T) {
	log := newLogger()
	log.user("u1", "upgrade the cluster")
	log.assistant("u1", "r1.1", "starting")
	log.task("u1", "a1.1", "upgrader", nil)
	log.taskAsk("u1", "a1.1", "aq1", "which maintenance window?")
	log.assistant("u1", "r1.2", "the upgrader needs a window from you")
	log.taskAnswer("u1", "a1.1", "ap1", "tonight")
	log.user("u2", "status?")

	modes := DefaultModes()
	modes.AsyncInput = TreatmentNote
	noted := project(t, log, "u2", modes)
	// The ask and the answer keep their own positions, so the response the
	// model wrote in between stays between them.
	wantShape(t, noted,
		"u(upgrade the cluster)",
		"a(starting)",
		"n{}",
		"n{}",
		"a(the upgrader needs a window from you)",
		"n{}",
		"n{}",
		"u(status?)")
	wantNoteContaining(t, noted, "which maintenance window?")
	wantNoteContaining(t, noted, "tonight")

	modes.AsyncInput = TreatmentElide
	wantShape(t, project(t, log, "u2", modes),
		"u(upgrade the cluster)",
		"a(starting)",
		"n{}",
		"a(the upgrader needs a window from you)",
		"n{}",
		"u(status?)")
}

func TestForeignArcModeGovernsADeadArc(t *testing.T) {
	// 05-dead-arc-elided.md
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.assistant("u1", "r1.1", "let me check")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", nil)
	log.failure("u1", "fe1.1", "the cluster connection dropped", true)
	log.user("u2", "describe node n1")

	modes := DefaultModes()
	modes.ForeignArc = TreatmentElide
	wantShape(t, project(t, log, "u2", modes), "u(describe node n1)")

	modes.ForeignArc = TreatmentNote
	noted := project(t, log, "u2", modes)
	wantShape(t, noted, "n{}", "u(describe node n1)")
	wantNoteContaining(t, noted, "how many pods are pending?")
}

func TestErrorModeGovernsANonFatalFailure(t *testing.T) {
	log := newLogger()
	log.user("u1", "check the nodes")
	log.assistant("u1", "r1.1", "checking")
	log.failure("u1", "e1.1", "retrying after a timeout", false)
	log.assistant("u1", "r1.2", "all nodes ready")
	log.closeArc("u1", "r1.2")
	log.user("u2", "thanks")

	modes := DefaultModes()
	modes.Error = TreatmentElide
	wantShape(t, project(t, log, "u2", modes),
		"u(check the nodes)", "a(checking)", "a(all nodes ready)", "u(thanks)")

	modes.Error = TreatmentNote
	noted := project(t, log, "u2", modes)
	wantShape(t, noted,
		"u(check the nodes)", "a(checking)", "n{}", "a(all nodes ready)", "u(thanks)")
	wantNoteContaining(t, noted, "retrying after a timeout")
}

func TestInterruptModeGovernsAnApprovalThatWasGranted(t *testing.T) {
	log := newLogger()
	log.user("u1", "delete the failed jobs")
	log.assistant("u1", "r1.1", "this needs approval")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_delete", nil)
	log.interrupt("u1", "i1.1", "t1.1.1", "deletion of 12 jobs")
	log.interruptResponse("u1", "i1.1", false)
	log.toolResult("u1", "t1.1.1", "12 jobs deleted", false)
	log.assistant("u1", "r1.2", "deleted")
	log.closeArc("u1", "r1.2")
	log.user("u2", "thanks")

	// An approval leaves no mark on the call: it ran and returned like any
	// other. Under elide the transcript cannot tell anyone was asked.
	modes := DefaultModes()
	modes.Interrupt = TreatmentElide
	wantShape(t, project(t, log, "u2", modes),
		"u(delete the failed jobs)",
		"a(this needs approval)[t(t1.1.1)]",
		"r(t1.1.1:ok)",
		"a(deleted)",
		"u(thanks)")

	modes.Interrupt = TreatmentNote
	noted := project(t, log, "u2", modes)
	// Two notes: the ask where it happened, the decision where it came back.
	wantShape(t, noted,
		"u(delete the failed jobs)",
		"a(this needs approval)[t(t1.1.1)]",
		"r(t1.1.1:ok)",
		"n{}",
		"n{}",
		"a(deleted)",
		"u(thanks)")
	wantNoteContaining(t, noted, "deletion of 12 jobs")
	wantNoteContaining(t, noted, "interrupt i1.1: a human approved it")

	// Both notes name the interrupt, so the decision can be matched to the ask
	// across whatever sits between them.
	for _, text := range noteTexts(noted) {
		if !strings.Contains(text, "i1.1") {
			t.Errorf("note %q does not name the interrupt it is about", text)
		}
	}
}

func TestInterruptAskIsTheSameTextBeforeAndAfterTheAnswer(t *testing.T) {
	log := newLogger()
	log.user("u1", "delete the failed jobs")
	log.assistant("u1", "r1.1", "this needs approval")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_delete", nil)
	log.interrupt("u1", "i1.1", "t1.1.1", "deletion of 12 jobs")
	log.user("u2", "meanwhile, describe node n1")
	log.interruptResponse("u1", "i1.1", false)
	log.user("u3", "and the ingress?")

	modes := DefaultModes()
	modes.Interrupt = TreatmentNote
	waiting := project(t, log, "u2", modes)
	answered := project(t, log, "u3", modes)

	// The ask says nothing about the outcome, so the answer arriving later
	// adds a message rather than rewriting one. A prefix that does not change
	// is a prefix a provider can still serve from cache.
	ask := askNote(log.history(t).Arcs[0].Responses[0].ToolCalls[0].Interrupt)
	if !strings.Contains(strings.Join(noteTexts(waiting), "\n"), ask) {
		t.Fatalf("notes = %q, want the ask while the decision is outstanding", noteTexts(waiting))
	}
	if !strings.Contains(strings.Join(noteTexts(answered), "\n"), ask) {
		t.Fatalf("notes = %q, want the same ask once the decision arrived", noteTexts(answered))
	}
	for i, message := range shape(waiting) {
		if got := shape(answered)[i]; got != message {
			t.Errorf("message %d changed from %s to %s once the decision arrived", i, message, got)
		}
	}
}

func TestRejectedInterruptIsAlwaysDisclosed(t *testing.T) {
	log := newLogger()
	log.user("u1", "delete the failed jobs")
	log.assistant("u1", "r1.1", "this needs approval")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_delete", nil)
	log.interrupt("u1", "i1.1", "t1.1.1", "deletion of 12 jobs")
	log.interruptResponse("u1", "i1.1", true)
	log.assistant("u1", "r1.2", "left them in place")
	log.closeArc("u1", "r1.2")
	log.user("u2", "thanks")

	// A refusal settles the call, so it reaches the model as that call's
	// result. It needs no note and no mode allows it to be dropped: the call
	// has to be answered and the refusal is the answer.
	modes := DefaultModes()
	modes.Interrupt = TreatmentElide
	transcript := project(t, log, "u2", modes)
	wantShape(t, transcript,
		"u(delete the failed jobs)",
		"a(this needs approval)[t(t1.1.1)]",
		"r(t1.1.1:refused)",
		"a(left them in place)",
		"u(thanks)")
	if notes := noteTexts(transcript); len(notes) != 0 {
		t.Errorf("notes = %q, want none: the call's own result carries the refusal", notes)
	}

	refusal := transcript.Messages[2].(ModelToolMessage)
	if !strings.Contains(refusal.Content, "human") || !refusal.IsError {
		t.Errorf("refusal = %#v, want an error result naming the human who refused", refusal)
	}
	if refusal.Outcome != ModelToolOutcomeRefused {
		t.Errorf("refusal outcome = %s, want %s", refusal.Outcome, ModelToolOutcomeRefused)
	}
	if refusal.Outcome.Synthesized() {
		t.Error("the refusal is what the log says, so it must not read as synthesized")
	}
}

func TestCancelThatStoppedNothingIsAlwaysElided(t *testing.T) {
	// 08-cancel-too-late.md
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.assistant("u1", "r1.1", "let me check")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", nil)
	log.toolResult("u1", "t1.1.1", "6 pending", false)
	log.assistant("u1", "r1.2", "6 pods are pending")
	log.closeArc("u1", "r1.2")
	log.cancel("u1", "c1.1", nil)
	log.user("u2", "thanks")

	for _, mode := range []Treatment{TreatmentNote, TreatmentElide} {
		modes := DefaultModes()
		modes.ForeignArc = mode
		transcript := project(t, log, "u2", modes)
		wantShape(t, transcript,
			"u(how many pods are pending?)",
			"a(let me check)[t(t1.1.1)]",
			"r(t1.1.1:ok)",
			"a(6 pods are pending)",
			"u(thanks)")
		if notes := noteTexts(transcript); len(notes) != 0 {
			t.Errorf("cancel=%s notes = %q, want none for a cancel that stopped nothing", mode, notes)
		}
	}
}

func TestCancelThatStoppedWorkIsAlwaysDisclosed(t *testing.T) {
	// 09-cancel-stopped-work.md
	log := newLogger()
	log.user("u1", "clean up the failed jobs in batch")
	log.assistant("u1", "r1.1", "deleting every failed job")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_delete", nil)
	log.cancel("u1", "c1.1", nil)
	log.user("u2", "delete only the ones older than a week")

	modes := DefaultModes()
	modes.ForeignArc = TreatmentElide
	transcript := project(t, log, "u2", modes)
	wantShape(t, transcript,
		"u(clean up the failed jobs in batch)",
		"a(deleting every failed job)[t(t1.1.1)]",
		"r(t1.1.1:cancel)",
		"n{}",
		"u(delete only the ones older than a week)")
	wantNoteContaining(t, transcript, "a human stopped this request")
}

func TestStoppedCancelQuotesTheReasonWhenTheLogHasOne(t *testing.T) {
	log := newLogger()
	log.user("u1", "clean up the failed jobs")
	log.assistant("u1", "r1.1", "deleting")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_delete", nil)
	log.cancel("u1", "c1.1", stringPtr("superseded by a later request"))
	log.user("u2", "narrower this time")

	transcript := project(t, log, "u2", DefaultModes())
	wantNoteContaining(t, transcript, "superseded by a later request")
}

func TestCancelModeGovernsAWithdrawnRequest(t *testing.T) {
	// 10-cancel-withdrawn.md
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.cancel("u1", "c1.1", nil)
	log.user("u2", "describe node n1")

	modes := DefaultModes()
	modes.ForeignArc = TreatmentElide
	wantShape(t, project(t, log, "u2", modes), "u(describe node n1)")

	modes.ForeignArc = TreatmentNote
	noted := project(t, log, "u2", modes)
	wantShape(t, noted, "n{}", "u(describe node n1)")
	wantNoteContaining(t, noted, "withdrawn")
}

func TestDeadArcIsTreatedTheSameWheneverItDied(t *testing.T) {
	// One mode governs a foreign arc at every cutoff, so when the arc died
	// relative to an earlier turn cannot change what this turn is shown.
	// These two logs differ only in where the fatal error sits.
	early := newLogger()
	early.user("u1", "how many pods are pending?")
	early.failure("u1", "fe1.1", "the cluster connection dropped", true)
	early.user("u2", "describe node n1")
	early.assistant("u2", "r2.1", "n1 is Ready")
	early.closeArc("u2", "r2.1")
	early.user("u3", "and the ingress?")

	late := newLogger()
	late.user("u1", "how many pods are pending?")
	late.user("u2", "describe node n1")
	late.assistant("u2", "r2.1", "n1 is Ready")
	late.closeArc("u2", "r2.1")
	late.failure("u1", "fe1.1", "the cluster connection dropped", true)
	late.user("u3", "and the ingress?")

	for _, mode := range []Treatment{TreatmentKeep, TreatmentNote, TreatmentElide} {
		modes := DefaultModes()
		modes.ForeignArc = mode
		// Compared as sets. The mode decides which messages there are; log
		// order decides where they sit, and the death note sits at the fatal
		// error, which is in a different place in each log.
		before, after := slices.Sorted(slices.Values(shape(project(t, early, "u3", modes)))),
			slices.Sorted(slices.Values(shape(project(t, late, "u3", modes))))
		if strings.Join(before, " ") != strings.Join(after, " ") {
			t.Errorf("foreign_arc=%s discloses different material depending on when U1 died:\n  %s\n  %s",
				mode, strings.Join(before, " "), strings.Join(after, " "))
		}
	}
}

func TestDeadArcKeptWithAPendingCallReportsOnlyItsDeath(t *testing.T) {
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.assistant("u1", "r1.1", "let me check")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", nil)
	log.user("u2", "also, describe node n1")
	log.assistant("u2", "r2.1", "n1 is Ready")
	log.closeArc("u2", "r2.1")
	log.failure("u1", "fe1.1", "the cluster connection dropped", true)
	log.user("u3", "and the ingress?")

	// One note, not two. The arc is dead, so its status is that it failed. An
	// in-flight note would contradict the stand-in it explains.
	transcript := project(t, log, "u3", DefaultModes())
	wantShape(t, transcript,
		"u(how many pods are pending?)",
		"a(let me check)[t(t1.1.1)]",
		"r(t1.1.1:pending)",
		"u(also, describe node n1)",
		"a(n1 is Ready)",
		"n{}",
		"u(and the ingress?)")
	wantNoteContaining(t, transcript, "failed and was never answered")
}

func TestArcOrderingIsLogOrder(t *testing.T) {
	// 11-surrounding-arc.md
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.assistant("u1", "r1.1", "let me check the cluster")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", nil)
	log.user("u2", "also, describe node n1")
	log.assistant("u2", "r2.1", "n1 is Ready")
	log.closeArc("u2", "r2.1")
	log.toolResult("u1", "t1.1.1", "6 pods pending", false)
	log.assistant("u1", "r1.2", "6 pods are pending")
	log.closeArc("u1", "r1.2")
	log.user("u3", "and the ingress?")

	wantShape(t, project(t, log, "u3", DefaultModes()),
		"u(how many pods are pending?)",
		"a(let me check the cluster)[t(t1.1.1)]",
		"r(t1.1.1:ok)",
		"u(also, describe node n1)",
		"a(n1 is Ready)",
		"a(6 pods are pending)",
		"u(and the ingress?)")

	// The same call under the earlier cutoff, where its result had not
	// arrived. A transcript is a view from one point in the log.
	wantShape(t, project(t, log, "u2", DefaultModes()),
		"u(how many pods are pending?)",
		"a(let me check the cluster)[t(t1.1.1)]",
		"r(t1.1.1:pending)",
		"n{}",
		"u(also, describe node n1)",
		"a(n1 is Ready)")
}

func TestProjectedArcKeepsItsOwnMaterialWhateverTheModeSays(t *testing.T) {
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.assistant("u1", "r1.1", "let me check")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", nil)
	log.toolResult("u1", "t1.1.1", "6 pending", false)

	modes := DefaultModes()
	modes.ForeignArc = TreatmentElide
	wantShape(t, project(t, log, "u1", modes),
		"u(how many pods are pending?)",
		"a(let me check)[t(t1.1.1)]",
		"r(t1.1.1:ok)")
}

func TestAssistantMessageWithNothingLeftIsDropped(t *testing.T) {
	log := newLogger()
	log.user("u1", "clean up")
	log.assistant("u1", "r1.1", "")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_delete", nil)
	log.user("u2", "never mind")

	modes := DefaultModes()
	modes.Pending = TreatmentElide
	// The elided call takes the text-free message it belonged to with it, so
	// U1 reaches the transcript as a bare user turn and the inflight note is
	// the only thing saying it is not this turn's to answer.
	wantShape(t, project(t, log, "u2", modes),
		"u(clean up)", "n{}", "u(never mind)")
}

func TestNoNoteLandsBetweenACallAndItsResult(t *testing.T) {
	log := newLogger()
	log.user("u1", "roll the deployment")
	log.assistant("u1", "r1.1", "rolling")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_rollout", nil)
	log.toolEvent("u1", "t1.1.1", "te1", "2 of 5 replicas updated")
	log.failure("u1", "e1.1", "a retry was needed", false)
	log.toolResult("u1", "t1.1.1", "rolled out", false)
	log.assistant("u1", "r1.2", "done")
	log.closeArc("u1", "r1.2")
	log.user("u2", "thanks")

	modes := DefaultModes()
	modes.ToolEvents = TreatmentNote
	modes.Error = TreatmentNote
	transcript := project(t, log, "u2", modes)

	for i, message := range transcript.Messages {
		assistant, isAssistant := message.(ModelAssistantMessage)
		if !isAssistant || len(assistant.ToolCalls) == 0 {
			continue
		}
		for offset := 1; offset <= len(assistant.ToolCalls); offset++ {
			if _, isResult := transcript.Messages[i+offset].(ModelToolMessage); !isResult {
				t.Fatalf("message %d after a call is %T, want the call's result:\n  %s",
					i+offset, transcript.Messages[i+offset], strings.Join(shape(transcript), " "))
			}
		}
	}
}

func TestProjectRejectsUnknownArcsAndIllegalModes(t *testing.T) {
	history := openArcLog().history(t)

	if _, err := history.Project("nope", DefaultModes()); err == nil {
		t.Error("Project(unknown arc) error = nil, want an error")
	}

	var nilHistory *History
	if _, err := nilHistory.Project("u1", DefaultModes()); err == nil {
		t.Error("Project on a nil history error = nil, want an error")
	}

	for name, modes := range map[string]Modes{
		"foreign arc cannot stand in": {ForeignArc: TreatmentStandIn, Pending: TreatmentStandIn},
		"pending cannot keep":         {ForeignArc: TreatmentKeep, Pending: TreatmentKeep},
		"error cannot keep":           {ForeignArc: TreatmentKeep, Pending: TreatmentStandIn, Error: TreatmentKeep},
		"tool events cannot stand in": {ForeignArc: TreatmentKeep, Pending: TreatmentStandIn, ToolEvents: TreatmentStandIn},
	} {
		if err := modes.Validate(); err == nil {
			t.Errorf("Validate(%s) error = nil, want an error", name)
		}
		if _, err := history.Project("u2", modes); err == nil {
			t.Errorf("Project(%s) error = nil, want an error", name)
		}
	}

	if err := DefaultModes().Validate(); err != nil {
		t.Errorf("Validate(defaults) error = %v", err)
	}
}

func TestProjectionIsAPureFunctionOfTheLogAndTheArc(t *testing.T) {
	log := pendingCallLog()
	first := shape(project(t, log, "u2", DefaultModes()))
	second := shape(project(t, log, "u2", DefaultModes()))
	if strings.Join(first, " ") != strings.Join(second, " ") {
		t.Errorf("two projections of one log disagree:\n  %s\n  %s", strings.Join(first, " "), strings.Join(second, " "))
	}
}

func TestArcWaitingOnAnUnansweredInterruptIsStillOpen(t *testing.T) {
	log := newLogger()
	log.user("u1", "delete the failed jobs")
	log.assistant("u1", "r1.1", "this needs approval")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_delete", nil)
	log.interrupt("u1", "i1.1", "t1.1.1", "deletion of 12 jobs")
	log.user("u2", "describe node n1")

	// Nobody has answered the interrupt, so the arc is neither closed nor
	// failed, so foreign_arc governs it.
	modes := DefaultModes()
	modes.ForeignArc = TreatmentNote
	noted := project(t, log, "u2", modes)
	wantShape(t, noted, "n{}", "u(describe node n1)")
	wantNoteContaining(t, noted, "still being worked on")

	modes.ForeignArc = TreatmentKeep
	wantShape(t, project(t, log, "u2", modes),
		"u(delete the failed jobs)",
		"a(this needs approval)[t(t1.1.1)]",
		"r(t1.1.1:pending)",
		"n{}",
		"u(describe node n1)")
}

func TestInterruptAnsweredAfterTheCutoffIsNotDisclosedYet(t *testing.T) {
	log := newLogger()
	log.user("u1", "delete the failed jobs")
	log.assistant("u1", "r1.1", "this needs approval")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_delete", nil)
	log.interrupt("u1", "i1.1", "t1.1.1", "deletion of 12 jobs")
	log.user("u2", "describe node n1")
	log.interruptResponse("u1", "i1.1", false)

	modes := DefaultModes()
	modes.Interrupt = TreatmentNote
	transcript := project(t, log, "u2", modes)
	// The ask is disclosed and the arc reports itself. What neither reports is
	// the approval, which happened after the cutoff.
	wantShape(t, transcript,
		"u(delete the failed jobs)",
		"a(this needs approval)[t(t1.1.1)]",
		"r(t1.1.1:pending)",
		"n{}",
		"n{}",
		"u(describe node n1)")
	wantNoteContaining(t, transcript, "interrupt i1.1: a human was asked to approve")
	for _, text := range noteTexts(transcript) {
		if strings.Contains(text, "approved") {
			t.Errorf("notes = %q, want nothing about a decision taken after the cutoff", noteTexts(transcript))
		}
	}
}

func TestArcHoldingARunningTaskStaysOpen(t *testing.T) {
	log := newLogger()
	log.user("u1", "audit the cluster")
	log.assistant("u1", "r1.1", "started the audit")
	log.task("u1", "a1.1", "auditor", nil)
	log.user("u2", "meanwhile, describe node n1")

	// The reply requested no calls, but the task it started is still running,
	// so the agent has not closed the arc. Nothing may report it as finished.
	transcript := project(t, log, "u2", DefaultModes())
	wantShape(t, transcript,
		"u(audit the cluster)",
		"a(started the audit)",
		"n{}",
		"n{}",
		"u(meanwhile, describe node n1)")
	wantNoteContaining(t, transcript, "task auditor started")
	wantNoteContaining(t, transcript, "still being worked on")

	if status := log.history(t).Arcs[0].Status(); status != ArcStatusPending {
		t.Errorf("arc status = %s, want %s while its task runs", status, ArcStatusPending)
	}
}

func TestCancelThatStoppedARunningTaskIsDisclosed(t *testing.T) {
	log := newLogger()
	log.user("u1", "audit the cluster")
	log.assistant("u1", "r1.1", "started the audit")
	log.task("u1", "a1.1", "auditor", nil)
	log.cancel("u1", "c1.1", nil)
	log.user("u2", "never mind")

	// The arc was open because its task was running, so the cancel stopped
	// work and is disclosed whatever the mode says.
	modes := DefaultModes()
	modes.ForeignArc = TreatmentElide
	transcript := project(t, log, "u2", modes)
	wantShape(t, transcript,
		"u(audit the cluster)",
		"a(started the audit)",
		"n{}",
		"n{}",
		"n{}",
		"u(never mind)")
	// The cancel ended the waiting, not the task. What became of the task is
	// not something the cancel knows, and the note does not pretend it does.
	wantNoteContaining(t, transcript, "still running when this request was stopped")
	wantNoteContaining(t, transcript, "a human stopped this request")

	if cancel := log.history(t).Arcs[0].Cancel; !cancel.TerminatedWork {
		t.Errorf("cancel = %#v, want TerminatedWork for a cancel that stopped a task", cancel)
	}
}

func TestFailedTaskIsReportedAsFailed(t *testing.T) {
	log := newLogger()
	log.user("u1", "audit the cluster")
	log.assistant("u1", "r1.1", "started the audit")
	log.task("u1", "a1.1", "auditor", nil)
	log.taskResult("u1", "a1.1", "the audit host was unreachable", true)
	log.assistant("u1", "r1.2", "the audit did not run")
	log.closeArc("u1", "r1.2")
	log.user("u2", "thanks")

	transcript := project(t, log, "u2", DefaultModes())
	wantNoteContaining(t, transcript, "task auditor failed: the audit host was unreachable")
}

func TestCancelledCallWithoutAReasonStillAnswersTheCall(t *testing.T) {
	log := newLogger()
	log.user("u1", "clean up")
	log.assistant("u1", "r1.1", "deleting")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_delete", nil)
	log.cancel("u1", "c1.1", nil)
	log.user("u2", "never mind")

	transcript := project(t, log, "u2", DefaultModes())
	for _, message := range transcript.Messages {
		result, isResult := message.(ModelToolMessage)
		if !isResult {
			continue
		}
		if result.Content == "" {
			t.Errorf("result for %s has no content, want a stated outcome", result.ToolCallID)
		}
		if result.Outcome != ModelToolOutcomeCancelled {
			t.Errorf("result for %s is %s, want the cancel the log recorded", result.ToolCallID, result.Outcome)
		}
	}
}

func TestMessageRolesAreTheClosedSet(t *testing.T) {
	for _, testCase := range []struct {
		message ModelMessage
		want    ModelMessageRole
	}{
		{ModelTextMessage{MessageRole: ModelRoleUser}, ModelRoleUser},
		{ModelTextMessage{MessageRole: ModelRoleInstruction}, ModelRoleInstruction},
		{ModelAssistantMessage{}, ModelRoleAssistant},
		{ModelToolMessage{}, ModelRoleTool},
		{ModelNoteMessage{}, ModelRoleNote},
	} {
		if got := testCase.message.Role(); got != testCase.want {
			t.Errorf("%T.Role() = %s, want %s", testCase.message, got, testCase.want)
		}
	}
}

func TestTreatmentNamesMatchTheSpec(t *testing.T) {
	for treatment, want := range map[Treatment]string{
		TreatmentElide:   "elide",
		TreatmentNote:    "note",
		TreatmentStandIn: "stand_in",
		TreatmentKeep:    "keep",
	} {
		if got := treatment.String(); got != want {
			t.Errorf("Treatment(%d).String() = %s, want %s", int(treatment), got, want)
		}
	}
}

func TestPolicyBlockAndHumanRefusalAreDifferentOutcomes(t *testing.T) {
	log := newLogger()
	log.user("u1", "clean up the cluster")
	log.assistant("u1", "r1.1", "two things to do")
	log.toolCall("u1", "r1.1", "blocked", "kubectl_delete", nil)
	log.toolCall("u1", "r1.1", "refused", "kubectl_drain", nil)
	log.guardrail("u1", "blocked", "deleting namespaces is not allowed")
	log.interrupt("u1", "i1.1", "refused", "draining node n1")
	log.interruptResponse("u1", "i1.1", true)
	log.assistant("u1", "r1.2", "neither went through")
	log.closeArc("u1", "r1.2")
	log.user("u2", "understood")

	transcript := project(t, log, "u2", DefaultModes())
	results := map[string]ModelToolMessage{}
	for _, message := range transcript.Messages {
		if result, isResult := message.(ModelToolMessage); isResult {
			results[result.ToolCallID] = result
		}
	}

	// Both calls were refused and neither ran, but by different deciders. The
	// model can go back to a human and cannot argue with a policy.
	if !strings.Contains(results["blocked"].Content, "policy") {
		t.Errorf("policy block = %q, want it named as a policy", results["blocked"].Content)
	}
	if !strings.Contains(results["refused"].Content, "human") {
		t.Errorf("human refusal = %q, want it named as a human", results["refused"].Content)
	}

	calls := log.history(t).Arcs[0].Responses[0].ToolCalls
	if calls[0].Status != ToolCallStatusBlocked || calls[1].Status != ToolCallStatusRefused {
		t.Errorf("statuses = %s / %s, want %s / %s",
			calls[0].Status, calls[1].Status, ToolCallStatusBlocked, ToolCallStatusRefused)
	}
	if calls[0].Interrupt != nil {
		t.Error("a policy block asked nobody, so its call must carry no interrupt")
	}
}

func TestCallWaitingOnAHumanSaysSoInItsStandIn(t *testing.T) {
	log := newLogger()
	log.user("u1", "drain node n1")
	log.assistant("u1", "r1.1", "this needs approval")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_drain", nil)
	log.interrupt("u1", "i1.1", "t1.1.1", "draining node n1")
	log.user("u2", "meanwhile, how many pods are pending?")
	log.assistant("u2", "r2.1", "checking")
	log.toolCall("u2", "r2.1", "t2.1.1", "kubectl_get", nil)
	log.user("u3", "and the ingress?")

	transcript := project(t, log, "u3", DefaultModes())
	standIns := map[string]string{}
	for _, message := range transcript.Messages {
		if result, isResult := message.(ModelToolMessage); isResult && result.Outcome.Synthesized() {
			standIns[result.ToolCallID] = result.Content
		}
	}

	// Both calls are unanswered. One waits on a machine, the other on a
	// person, and only the second is something anyone can do anything about.
	if !strings.Contains(standIns["t1.1.1"], "waiting for a human") {
		t.Errorf("held call stand-in = %q, want it to say it waits on a human", standIns["t1.1.1"])
	}
	if strings.Contains(standIns["t2.1.1"], "human") {
		t.Errorf("running call stand-in = %q, want no claim about a human", standIns["t2.1.1"])
	}
}

func TestInterruptCancelledWithItsCallReportsNoDecision(t *testing.T) {
	log := newLogger()
	log.user("u1", "drain node n1")
	log.assistant("u1", "r1.1", "this needs approval")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_drain", nil)
	log.interrupt("u1", "i1.1", "t1.1.1", "draining node n1")
	log.cancel("u1", "c1.1", nil)
	log.user("u2", "never mind")

	modes := DefaultModes()
	modes.Interrupt = TreatmentNote
	transcript := project(t, log, "u2", modes)
	// The ask stands, because it was asked. Nothing answers it, because the
	// cancel took the call away before a human got to it.
	wantShape(t, transcript,
		"u(drain node n1)",
		"a(this needs approval)[t(t1.1.1)]",
		"r(t1.1.1:cancel)",
		"n{}",
		"n{}",
		"u(never mind)")
	wantNoteContaining(t, transcript, "interrupt i1.1: a human was asked to approve")
	for _, text := range noteTexts(transcript) {
		if strings.Contains(text, "approved it") || strings.Contains(text, "refused it") {
			t.Errorf("notes = %q, want no decision for an ask nobody answered", noteTexts(transcript))
		}
	}

	interrupt := log.history(t).Arcs[0].Responses[0].ToolCalls[0].Interrupt
	if interrupt.Status != InterruptCancelled {
		t.Errorf("interrupt status = %s, want %s", interrupt.Status, InterruptCancelled)
	}
}

func TestInterruptContextReachesTheAskAndStaysPutThere(t *testing.T) {
	context := map[string]any{"node": "n1", "pods_evicted": 14}
	log := newLogger()
	log.user("u1", "drain node n1")
	log.assistant("u1", "r1.1", "this needs approval")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_drain", map[string]any{"node": "n1"})
	log.interruptWithContext("u1", "i1.1", "t1.1.1", "draining evicts running pods", context)
	log.user("u2", "meanwhile, how many pods are pending?")
	log.interruptResponse("u1", "i1.1", false)
	log.user("u3", "and the ingress?")

	modes := DefaultModes()
	modes.Interrupt = TreatmentNote

	// The context is what the human deciding was shown. The model is told the
	// same thing, so it can account for the decision without guessing.
	waiting := project(t, log, "u2", modes)
	wantNoteContaining(t, waiting, "draining evicts running pods")
	wantNoteContaining(t, waiting, "pods_evicted")

	// Context is written when the interrupt is raised and never changes, so
	// the ask is still the same message once the decision has arrived.
	answered := project(t, log, "u3", modes)
	for i, message := range shape(waiting) {
		if got := shape(answered)[i]; got != message {
			t.Errorf("message %d changed from %s to %s once the decision arrived", i, message, got)
		}
	}
	if stored := log.history(t).Arcs[0].Responses[0].ToolCalls[0].Interrupt.Context; stored == nil {
		t.Error("interrupt context = nil, want the context the event carried")
	}
}

func TestEveryProjectedResultCarriesItsOutcome(t *testing.T) {
	log := newLogger()
	log.user("u1", "do five things")
	log.assistant("u1", "r1.1", "working")
	log.toolCall("u1", "r1.1", "ok", "kubectl_get", nil)
	log.toolCall("u1", "r1.1", "bad", "kubectl_get", nil)
	log.toolCall("u1", "r1.1", "blocked", "kubectl_delete", nil)
	log.toolCall("u1", "r1.1", "refused", "kubectl_drain", nil)
	log.toolCall("u1", "r1.1", "running", "kubectl_logs", nil)
	log.toolResult("u1", "ok", "6 pods pending", false)
	log.toolResult("u1", "bad", "connection refused", true)
	log.guardrail("u1", "blocked", "not allowed")
	log.interrupt("u1", "i1.1", "refused", "draining node n1")
	log.interruptResponse("u1", "i1.1", true)
	log.user("u2", "understood")

	transcript := project(t, log, "u2", DefaultModes())
	want := map[string]ModelToolOutcome{
		"ok":      ModelToolOutcomeSuccess,
		"bad":     ModelToolOutcomeFailed,
		"blocked": ModelToolOutcomeBlocked,
		"refused": ModelToolOutcomeRefused,
		"running": ModelToolOutcomePending,
	}
	seen := 0
	for _, message := range transcript.Messages {
		result, isResult := message.(ModelToolMessage)
		if !isResult {
			continue
		}
		seen++
		if got := result.Outcome; got != want[result.ToolCallID] {
			t.Errorf("outcome for %s = %s, want %s", result.ToolCallID, got, want[result.ToolCallID])
		}
		// IsError is the flag a provider carries. Everything that is not a
		// returned result sets it, including the stand-in's absence of one.
		if wantError := result.Outcome != ModelToolOutcomeSuccess && result.Outcome != ModelToolOutcomePending; result.IsError != wantError {
			t.Errorf("IsError for %s = %t, want %t", result.ToolCallID, result.IsError, wantError)
		}
	}
	if seen != len(want) {
		t.Errorf("results = %d, want one per call: %s", seen, strings.Join(shape(transcript), " "))
	}
}

func TestEventsForIsTheProjectionsCutoff(t *testing.T) {
	// U1's call returns after U2 has opened and answered, so the two arcs
	// disagree about it and the slice is what settles the disagreement.
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.assistant("u1", "r1.1", "let me check")
	log.toolCall("u1", "r1.1", "t1.1.1", "kubectl_get", nil)
	log.user("u2", "also, describe node n1")
	log.assistant("u2", "r2.1", "n1 is Ready")
	log.toolResult("u1", "t1.1.1", "six", false)
	log.assistant("u1", "r1.2", "six pods are pending")

	// U1 owns everything correlated to it however late it arrived, and sees
	// nothing of the arc that opened after its user message.
	if got, all := len(log.eventsFor("u1")), len(log.events); got >= all {
		t.Errorf("eventsFor(u1) kept %d of %d events, want a strict subset", got, all)
	}
	wantShape(t, project(t, log, "u1", DefaultModes()),
		"u(how many pods are pending?)",
		"a(let me check)[t(t1.1.1)]",
		"r(t1.1.1:ok)",
		"a(six pods are pending)")

	// U2 sees U1 as it stood when U2 was asked: the call had not returned and
	// the arc had not spoken again.
	wantShape(t, project(t, log, "u2", DefaultModes()),
		"u(how many pods are pending?)",
		"a(let me check)[t(t1.1.1)]",
		"r(t1.1.1:pending)",
		"n{}",
		"u(also, describe node n1)",
		"a(n1 is Ready)")
}

// A note names the request it is about so the model can tell which one is
// meant. Naming is not repeating: the whole request is already in the
// transcript, or was deliberately left out of it.
func TestANoteQuotesOnlyEnoughOfARequestToRecogniseIt(t *testing.T) {
	long := "how many pods are pending across every namespace in the cluster?"

	log := newLogger()
	log.user("u1", long)
	log.user("u2", "describe node n1")

	noted := project(t, log, "u2", DefaultModes())
	wantNoteContaining(t, noted, `"how many pods are pending across…"`)

	for _, text := range noteTexts(noted) {
		if strings.Contains(text, "every namespace") {
			t.Errorf("note = %q, want the request cut at %d characters", text, DefaultQuoteLimit)
		}
	}
}

// A request that was cut says so, and one that was always short does not, or
// the two cannot be told apart.
func TestOnlyACutRequestIsMarkedAsCut(t *testing.T) {
	log := newLogger()
	log.user("u1", "how many pods are pending?")
	log.user("u2", "describe node n1")

	for _, text := range noteTexts(project(t, log, "u2", DefaultModes())) {
		if strings.Contains(text, "…") {
			t.Errorf("note = %q, want no ellipsis on a request that fits", text)
		}
	}
}

func TestQuoteLimitIsConfigurable(t *testing.T) {
	long := "how many pods are pending across every namespace in the cluster?"

	log := newLogger()
	log.user("u1", long)
	log.user("u2", "describe node n1")

	modes := DefaultModes()
	modes.QuoteLimit = 8
	wantNoteContaining(t, project(t, log, "u2", modes), `"how many…"`)

	// Zero quotes the request whole, which is what a Modes built by hand and
	// never told otherwise does.
	modes.QuoteLimit = 0
	wantNoteContaining(t, project(t, log, "u2", modes), `"`+long+`"`)
}

// A note is one line, so a request that was not folds into one.
func TestAQuotedRequestIsFoldedOntoOneLine(t *testing.T) {
	log := newLogger()
	log.user("u1", "how many pods\nare pending?")
	log.user("u2", "describe node n1")

	noted := project(t, log, "u2", DefaultModes())
	wantNoteContaining(t, noted, `"how many pods are pending?"`)
	for _, text := range noteTexts(noted) {
		if strings.Contains(text, "pods\nare") {
			t.Errorf("note = %q, want the request on one line", text)
		}
	}
}

func TestANegativeQuoteLimitIsRejected(t *testing.T) {
	modes := DefaultModes()
	modes.QuoteLimit = -1
	if err := modes.Validate(); err == nil {
		t.Error("a negative quote limit was accepted")
	}
}

// cancelledCallLog is one arc whose two calls were stopped by a cancel: one
// had reached its tool, the other had not.
func cancelledCallLog() *logger {
	reason := "the user stopped it"
	log := newLogger()
	log.user("u1", "restart the ingress controller")
	log.assistant("u1", "r1.1", "restarting it")
	log.toolCall("u1", "r1.1", "t1.1.1", "restart_deployment", map[string]any{"name": "ingress-nginx"})
	log.toolCall("u1", "r1.1", "t1.1.2", "list_pods", nil)
	log.toolDispatched("u1", "t1.1.1")
	log.cancel("u1", "c1", &reason)
	return log
}

// A cancel settles a call its tool never saw. A call already handed to its
// tool is not the cancel's to settle, because whether it took effect is not
// something the cancel knows.
func TestCancelSettlesOnlyTheCallsThatNeverRan(t *testing.T) {
	arc := cancelledCallLog().history(t).Arcs[0]
	calls := arc.Responses[0].ToolCalls

	dispatched, never := calls[0], calls[1]
	if dispatched.Status != ToolCallStatusPending || dispatched.Phase() != ToolCallPhaseDispatched {
		t.Errorf("the dispatched call is %s in phase %s, want it pending and dispatched",
			dispatched.Status, dispatched.Phase())
	}
	if never.Status != ToolCallStatusCancelled {
		t.Errorf("the call that never ran is %s, want CANCELLED", never.Status)
	}

	// Why the arc was cancelled belongs to the cancel. A call the tool never
	// saw returned nothing, and saying otherwise puts words in its mouth.
	if never.Result != nil {
		t.Errorf("the cancelled call carries a result: %#v", never.Result)
	}
}

// The result of a stopped call settles it the ordinary way, because the cancel
// left it open for exactly this.
func TestAResultAfterACancelSettlesTheCall(t *testing.T) {
	log := cancelledCallLog()
	log.toolResult("u1", "t1.1.1", map[string]any{"restarted": "ingress-nginx"}, false)

	call := log.history(t).Arcs[0].Responses[0].ToolCalls[0]
	if call.Status != ToolCallStatusSuccess {
		t.Fatalf("the call is %s, want it settled by its own result", call.Status)
	}
	if call.FinishedSeq == nil || *call.FinishedSeq <= *call.DispatchedSeq {
		t.Errorf("the call settled at %v, want it after its dispatch", call.FinishedSeq)
	}
}

// What the model is told about a stopped arc. The two calls ended differently
// and the difference is the one that decides whether it tries again.
func TestAStoppedArcSaysWhichCallsMayHaveTakenEffect(t *testing.T) {
	log := cancelledCallLog()
	log.user("u2", "what happened?")

	noted := project(t, log, "u2", DefaultModes())
	var reported []string
	for _, message := range noted.Messages {
		if result, ok := message.(ModelToolMessage); ok {
			reported = append(reported, result.Content)
		}
	}
	if len(reported) != 2 {
		t.Fatalf("the transcript answers %d calls, want both", len(reported))
	}
	if !strings.Contains(reported[0], "not known") {
		t.Errorf("the dispatched call reads %q, want it to say the effect is unknown", reported[0])
	}
	if !strings.Contains(reported[1], "cancelled before it ran") {
		t.Errorf("the call that never ran reads %q", reported[1])
	}
}

// A call still pending under a live arc has not returned yet, which is a
// different thing to one nothing is coming back for.
func TestALiveArcStillReadsAsWaiting(t *testing.T) {
	log := newLogger()
	log.user("u1", "restart the ingress controller")
	log.assistant("u1", "r1.1", "restarting it")
	log.toolCall("u1", "r1.1", "t1.1.1", "restart_deployment", nil)
	log.toolDispatched("u1", "t1.1.1")
	log.user("u2", "any news?")

	noted := project(t, log, "u2", DefaultModes())
	for _, message := range noted.Messages {
		if result, ok := message.(ModelToolMessage); ok {
			if !strings.Contains(result.Content, "not returned yet") {
				t.Errorf("a live call reads %q, want it still waiting", result.Content)
			}
		}
	}
}

// An arc that died of a fatal error is over too, so a call it never made is
// not one that is still coming.
func TestADeadArcSaysItsCallsWereNeverMade(t *testing.T) {
	log := newLogger()
	log.user("u1", "restart the ingress controller")
	log.assistant("u1", "r1.1", "restarting it")
	log.toolCall("u1", "r1.1", "t1.1.1", "restart_deployment", nil)
	log.failure("u1", "e1", "the model is not available", true)
	log.user("u2", "what happened?")

	noted := project(t, log, "u2", DefaultModes())
	for _, message := range noted.Messages {
		if result, ok := message.(ModelToolMessage); ok {
			if !strings.Contains(result.Content, "never made") {
				t.Errorf("a call under a dead arc reads %q", result.Content)
			}
		}
	}
}

// A task outlives the run that started it, so a cancel cannot say what became
// of it. It ends the waiting and leaves the task as it found it.
func TestCancelLeavesARunningTaskUnsettled(t *testing.T) {
	log := newLogger()
	log.user("u1", "audit the cluster")
	log.assistant("u1", "r1.1", "started the audit")
	log.task("u1", "k1.1", "auditor", nil)
	log.cancel("u1", "c1.1", nil)

	task := log.history(t).Arcs[0].AsyncTasks[0]
	if task.Status != AsyncTaskPending {
		t.Errorf("task is %s, want it left running", task.Status)
	}
	if task.FinishedAt != nil || task.FinishedSeq != nil {
		t.Errorf("task = %#v, want it unfinished", task)
	}
}

// A result arriving after the cancel settles the task, the same way a tool
// result settles a call the cancel left open.
func TestATaskResultAfterACancelStillSettlesIt(t *testing.T) {
	log := newLogger()
	log.user("u1", "audit the cluster")
	log.assistant("u1", "r1.1", "started the audit")
	log.task("u1", "k1.1", "auditor", nil)
	log.cancel("u1", "c1.1", nil)
	log.taskResult("u1", "k1.1", "14 findings", false)
	log.user("u2", "anything?")

	task := log.history(t).Arcs[0].AsyncTasks[0]
	if task.Status != AsyncTaskSuccess || task.Result != "14 findings" {
		t.Fatalf("task = %#v, want it settled by its own result", task)
	}

	// And the model is told what it found, rather than that it was stopped.
	noted := project(t, log, "u2", DefaultModes())
	wantNoteContaining(t, noted, "task auditor finished: 14 findings")
	for _, text := range noteTexts(noted) {
		if strings.Contains(text, "not known") {
			t.Errorf("note = %q, want no uncertainty about a task that reported", text)
		}
	}
}

// Fixed rule: a task is always two notes. An arc that stopped will never
// produce a completion event, so the second note has to come from the stop.
func TestAStoppedArcStillGivesATaskItsSecondNote(t *testing.T) {
	log := newLogger()
	log.user("u1", "audit the cluster")
	log.assistant("u1", "r1.1", "started the audit")
	log.task("u1", "k1.1", "auditor", nil)
	log.failure("u1", "e1", "the worker died", true)
	log.user("u2", "what happened?")

	noted := project(t, log, "u2", DefaultModes())
	notes := noteTexts(noted)
	if len(notes) < 2 {
		t.Fatalf("notes = %q, want the task's start and its stop", notes)
	}
	if !strings.Contains(notes[0], "task auditor started") {
		t.Errorf("first note = %q, want the task starting", notes[0])
	}
	wantNoteContaining(t, noted, "still running when this request was stopped")
}
