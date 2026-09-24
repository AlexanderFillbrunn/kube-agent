package history

import (
	"errors"
	"fmt"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/users"
)

// ToolCallStatus is the lifecycle state of a ToolCall. Every state other than
// PENDING is terminal.
type ToolCallStatus string

const (
	// ToolCallStatusPending is a call that has been requested but has not
	// produced an outcome yet.
	ToolCallStatusPending ToolCallStatus = "PENDING"
	// ToolCallStatusFailed is a call that ran and reported an error.
	ToolCallStatusFailed ToolCallStatus = "FAILED"
	// ToolCallStatusSuccess is a call that ran and returned a result.
	ToolCallStatusSuccess ToolCallStatus = "SUCCESS"
	// ToolCallStatusBlocked is a call an automatic policy refused before it
	// ran. No human was asked.
	ToolCallStatusBlocked ToolCallStatus = "BLOCKED"
	// ToolCallStatusRefused is a call a human refused when asked to approve
	// it. A policy would have blocked it without asking.
	ToolCallStatusRefused ToolCallStatus = "REFUSED"
	// ToolCallStatusCancelled is a call still pending when its arc was cancelled.
	ToolCallStatusCancelled ToolCallStatus = "CANCELLED"
)

// MessageRole identifies who authored a piece of the conversation.
type MessageRole string

const (
	MessageRoleUser      MessageRole = "USER"
	MessageRoleAssistant MessageRole = "ASSISTANT"
	// MessageRoleInstruction marks a system or developer prompt rather than
	// anything a participant said.
	MessageRoleInstruction MessageRole = "INSTRUCTION"
)

// ToolEvent is a progress notification emitted by a tool while it runs. Events
// are informational; they never change the call's status.
type ToolEvent struct {
	ID string

	// Seq is the sequence number of the event that emitted this notification.
	Seq int

	Payload any
}

// ToolCall is one tool invocation requested by an assistant message.
type ToolCall struct {
	ID string

	// Seq is the sequence number of the event that requested the call. A
	// projection orders messages by the log, so every record keeps the
	// position it was written at.
	Seq int

	// VendorID is the model provider's own identifier for the call, used to
	// match a result back to the request when replying to the provider.
	VendorID string

	ToolName string
	Args     any
	Result   any
	Status   ToolCallStatus

	// DispatchedAt and DispatchedSeq record the first time the call was handed
	// to its tool, and are nil for a call that provably never ran. They are
	// past tense on purpose: a worker that died mid-call leaves them set with
	// no result behind them, which is the case they exist for.
	DispatchedAt  *time.Time
	DispatchedSeq *int

	// Dispatches counts how often the call was handed to its tool. More than
	// once means a worker picked the call up again after another had already
	// started it, so the tool may have run twice.
	Dispatches int

	// FinishedAt is set when the call reaches a terminal status, and is nil
	// while it is still pending.
	FinishedAt *time.Time

	// FinishedSeq is the sequence number of the event that settled the call,
	// and is nil while it is still pending. A projection cut off before this
	// number has to treat the call as unanswered even though the folded
	// history holds its outcome.
	FinishedSeq *int

	// Interrupt is the human decision this call is waiting on, or waited on,
	// and is nil for a call nobody was asked about. A call can only be put to
	// a human once.
	Interrupt *Interrupt

	Events []*ToolEvent
}

// ToolCallPhase is where a call stands, derived from the records the fold
// wrote rather than stored. Status alone cannot say it: a requested call, one
// held for a human and one already handed to its tool are all PENDING, and the
// difference decides whether a recovering worker may run it again.
type ToolCallPhase string

const (
	// ToolCallPhaseRequested is a call the model asked for that has not been
	// dispatched and is not waiting on anyone. Nothing has run.
	ToolCallPhaseRequested ToolCallPhase = "REQUESTED"
	// ToolCallPhaseAwaitingApproval is a call held until a human decides on
	// it.
	ToolCallPhaseAwaitingApproval ToolCallPhase = "AWAITING_APPROVAL"
	// ToolCallPhaseDispatched is a call that was handed to its tool and has no
	// outcome. Whether it is still running or was abandoned by a dead worker
	// cannot be told from the log, and neither can whether it took effect.
	ToolCallPhaseDispatched ToolCallPhase = "DISPATCHED"
	// ToolCallPhaseSettled is a call that reached a terminal status.
	ToolCallPhaseSettled ToolCallPhase = "SETTLED"
)

// Phase reports where the call stands. It is computed, so it cannot drift from
// the fold.
func (c *ToolCall) Phase() ToolCallPhase {
	switch {
	case c.Status != ToolCallStatusPending:
		return ToolCallPhaseSettled
	case c.DispatchedSeq != nil:
		return ToolCallPhaseDispatched
	case c.Interrupt != nil && c.Interrupt.Status == InterruptPending:
		return ToolCallPhaseAwaitingApproval
	default:
		return ToolCallPhaseRequested
	}
}

// AsyncTaskStatus is the lifecycle state of an asynchronous task.
type AsyncTaskStatus string

const (
	// AsyncTaskPending is a task that is running.
	AsyncTaskPending AsyncTaskStatus = "PENDING"
	// AsyncTaskSuccess is a task that finished and returned a result.
	AsyncTaskSuccess AsyncTaskStatus = "SUCCESS"
	// AsyncTaskFailed is a task that finished and reported an error.
	AsyncTaskFailed AsyncTaskStatus = "FAILED"
	// AsyncTaskWaiting is a task that has stopped to ask for input and resumes
	// once the input arrives.
	AsyncTaskWaiting AsyncTaskStatus = "WAITING"
	// AsyncTaskCancelled is a task that was still running or waiting when its
	// arc was cancelled.
	AsyncTaskCancelled AsyncTaskStatus = "CANCELLED"
)

// AsyncTaskEventType distinguishes task notifications from blocking input
// interactions. Every one is retained in AsyncTask.Events.
type AsyncTaskEventType string

const (
	AsyncTaskEventNotification   AsyncTaskEventType = "NOTIFICATION"
	AsyncTaskEventInputRequested AsyncTaskEventType = "INPUT_REQUESTED"
	AsyncTaskEventInputProvided  AsyncTaskEventType = "INPUT_PROVIDED"
)

// AsyncTaskEvent is one entry in a task's transcript, in the order it occurred.
type AsyncTaskEvent struct {
	ID        string             `json:"id"`
	Seq       int                `json:"seq"`
	Timestamp time.Time          `json:"timestamp"`
	Type      AsyncTaskEventType `json:"type"`
	Payload   any                `json:"payload"`
}

// AsyncTask is a unit of work that runs outside the agent's turn and reports
// back through events.
type AsyncTask struct {
	ID string `json:"id"`

	// Seq is the sequence number of the event that invoked the task.
	Seq int `json:"seq"`

	// CorrelationID is the arc that owns this task. Cancelling that arc stops
	// the task.
	CorrelationID string `json:"correlation_id"`

	TaskName string          `json:"task_name"`
	Status   AsyncTaskStatus `json:"status"`
	Args     any             `json:"args,omitempty"`
	Result   any             `json:"result,omitempty"`

	// Events is the full transcript of the task: notifications it emitted and
	// every input interaction it went through.
	Events []AsyncTaskEvent `json:"events"`

	// FinishedAt is set when the task reaches a terminal status, and is nil
	// while it is still running or waiting.
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// FinishedSeq is the sequence number of the event that settled the task,
	// and is nil while it is still running or waiting.
	FinishedSeq *int `json:"finished_seq,omitempty"`
}

type ArcStatus string

const (
	// ArcStatusPending represents an arc that is currently being worked on.
	ArcStatusPending     ArcStatus = "PENDING"
	// ArcStatusInterrupted represents an arc whose tool call is waiting for a human to approve it.
	ArcStatusInterrupted ArcStatus = "INTERRUPTED"
	// ArcStatusDone represents an arc that has completed through an agent message with no tool calls.
	ArcStatusDone        ArcStatus = "DONE"
	// ArcStatusCancelled represents an arc that is closed due to user cancellation.
	ArcStatusCancelled   ArcStatus = "CANCELLED"
	// ArcStatusError represents an arc that is closed due to an error.
	ArcStatusError       ArcStatus = "ERROR"
)

// Arc represents one user message in the conversation, including its responses and other events.
//
// An arc is always opened by a user message and owns everything that follows
// from it. Every other record carries the arc's ID as its correlation ID, which
// is what nests it here.
type Arc struct {
	// ID is the correlation ID every record in this arc is tagged with.
	ID string `json:"id"`

	Seq int `json:"seq"`

	// Content is what the user said to open the arc.
	Content string `json:"content"`

	Timestamp time.Time `json:"timestamp"`

	// Actor is who sent the user message.
	Actor *users.Actor `json:"actor,omitempty"`

	// Cancel is set once the arc has been cancelled, and is nil otherwise. An
	// arc can only be cancelled once.
	Cancel *Cancel `json:"cancel"`

	// Closed is set once the agent closed the arc, and is nil while it is
	// open. An arc can only be closed once.
	Closed *ArcClosed `json:"closed,omitempty"`

	// Responses are the agent's replies within this arc, in order. One arc can
	// produce several, since each round of tool calls yields another message.
	Responses []*AssistantMessage `json:"responses"`

	Errors     []*Error     `json:"errors"`
	AsyncTasks []*AsyncTask `json:"async_tasks"`
}

// Status computes the status of an arc. This is not a simple field lookup and should be used sparingly.
//
// Three records can close an arc and each says so itself: the close the agent
// recorded, a fatal error, and a cancel. A reply that requested no calls does
// not close anything on its own, because work it started can outlive it.
func (a Arc) Status() ArcStatus {
	status := ArcStatusPending
	closedAt := 0
	closedBy := func(seq int, closed ArcStatus) {
		if status == ArcStatusPending || seq < closedAt {
			status, closedAt = closed, seq
		}
	}

	if a.Closed != nil {
		closedBy(a.Closed.Seq, ArcStatusDone)
	}
	for _, e := range a.Errors {
		if e.IsFatal {
			closedBy(e.Seq, ArcStatusError)
		}
	}
	if a.Cancel != nil {
		closedBy(a.Cancel.Seq, ArcStatusCancelled)
	}
	if status != ArcStatusPending {
		return status
	}

	// An arc waiting on a human is waiting on one of its calls, since that is
	// the only thing an interrupt can hold up.
	for _, response := range a.Responses {
		for _, call := range response.ToolCalls {
			if call.Interrupt != nil && call.Interrupt.Status == InterruptPending {
				return ArcStatusInterrupted
			}
		}
	}
	return ArcStatusPending
}

// AssistantMessage represents the response of the agent to a user message.
type AssistantMessage struct {
	ID  string `json:"id"`
	Seq int    `json:"seq"`

	// CorrelationID is the arc this response belongs to.
	CorrelationID string `json:"correlation_id"`

	// VendorID is the model provider's own identifier for this message.
	VendorID *string `json:"vendor_id,omitempty"`

	Timestamp time.Time `json:"timestamp"`
	Content   string    `json:"content"`

	// Reasoning is the provider's own record of how the model arrived at this
	// message, kept as the opaque bytes it arrived as. Providers that accept
	// it back require it verbatim and alongside the message it belongs to, so
	// nothing here reads into it or rewrites it.
	Reasoning []byte `json:"reasoning,omitempty"`

	// Usage is what this message cost, and is nil where the provider reported
	// nothing. Nil is not zero, so a total over an arc is only as complete as
	// the messages that carry one.
	Usage *events.TokenUsage `json:"usage,omitempty"`

	// ToolCalls are the calls this message requested. A message with none is
	// the agent's answer rather than a step towards one.
	ToolCalls []*ToolCall `json:"tool_calls"`
}

// InterruptStatus is the lifecycle state of an Interrupt.
type InterruptStatus string

const (
	InterruptPending   InterruptStatus = "pending"
	InterruptApproved  InterruptStatus = "approved"
	InterruptRejected  InterruptStatus = "rejected"
	InterruptCancelled InterruptStatus = "cancelled"
)

// Interrupt is a tool call held up until a human decides on it: the
// human-in-the-loop gate. It is not a general pause. A policy that refuses a
// call on its own blocks it instead, without asking anyone.
type Interrupt struct {
	ID string `json:"id"`

	// Seq is the sequence number of the event that raised the interrupt.
	Seq int `json:"seq"`

	// ResolvedSeq is the sequence number of the event that answered or
	// cancelled it, and is nil while it is still pending.
	ResolvedSeq *int `json:"resolved_seq,omitempty"`

	// CorrelationID is the arc the held-up call belongs to. A resumed run picks
	// up the arc from here, and cancelling that arc cancels the interrupt.
	CorrelationID string `json:"correlation_id"`

	Timestamp time.Time `json:"timestamp"`

	// ToolCallID is the call this decision holds up. The call is nested under
	// the assistant message that requested it and carries this interrupt.
	ToolCallID string `json:"tool_call_id"`

	// Reason is what the human was asked to decide about, in words.
	Reason string `json:"reason"`

	// Context is what the decision is about beyond the call's own arguments.
	// It is written when the interrupt is raised and never changes, so
	// anything derived from it is stable for the life of the interrupt.
	Context any `json:"context,omitempty"`

	// Response and Responder are set once a human answers, and are nil while
	// the interrupt is pending or if it was cancelled unanswered.
	Response  any          `json:"response,omitempty"`
	Responder *users.Actor `json:"responder,omitempty"`

	Status InterruptStatus `json:"status"`
}

// ArcClosed records the close of one arc. The agent closes an arc once a
// response has requested no tool calls and no async task under it is still
// running, so an arc holding a runaway task stays open instead of looking
// finished because its last message was an answer.
type ArcClosed struct {
	ID        string    `json:"id"`
	Seq       int       `json:"seq"`
	Timestamp time.Time `json:"timestamp"`

	// CausationID is the assistant message that concluded the arc. Nothing
	// reads it to project; it is here so a closed arc can be traced back to
	// the message that closed it.
	CausationID string `json:"causation_id"`
}

// Cancel records cancellation of one arc. Cancelling stops the arc's pending
// tool calls, along with any decision they were waiting on, and its unfinished
// async tasks.
type Cancel struct {
	ID        string    `json:"id"`
	Seq       int       `json:"seq"`
	Arc       string    `json:"arc"`
	Timestamp time.Time `json:"timestamp"`

	// Actor is who cancelled, when a human did. Nil means the arc was
	// abandoned rather than withdrawn, such as a crashed run swept up later.
	Actor  *users.Actor `json:"actor,omitempty"`
	Reason *string      `json:"reason,omitempty"`

	// TerminatedWork says whether this cancel stopped anything at all. A
	// cancel that arrives after the arc has already finished is still
	// recorded, but leaves the completed exchange rendered as completed.
	TerminatedWork bool `json:"terminated_work"`
}

// Error is a failure recorded against the log. Not a message: nobody said it.
//
// A fatal error ends its arc; a non-fatal one is recorded and the arc carries
// on. Ordering against messages is recoverable either way, since Seq orders the
// whole log.
type Error struct {
	ID            string    `json:"id"`
	Seq           int       `json:"seq"`
	CorrelationID string    `json:"correlation_id"`
	Timestamp     time.Time `json:"timestamp"`
	Message       string    `json:"message"`

	// IsFatal marks a failure that ended the arc rather than one it survived.
	IsFatal bool `json:"is_fatal"`

	// CausationID is what the failure was about, when it was about one thing.
	CausationID *string `json:"causation_id,omitempty"`
}

// History is the folded state of one session's event log.
type History struct {
	SessionID string

	// Arcs holds one arc per user message, in the order the user sent them.
	Arcs []*Arc

	// LastSeq is the sequence number of the last event applied. Events arrive
	// in increasing sequence order, skipping numbers wherever another arc's
	// events fall inside this arc's lifetime: a history is built for one arc,
	// from a query that takes the events before that arc plus the ones
	// correlated to it.
	LastSeq int
}

// NewHistory returns an empty history for a session. LastSeq starts at -1 so
// that the log's first event, numbered 0, is accepted.
func NewHistory(id string) *History {
	return &History{
		SessionID: id,
		LastSeq:   -1,
	}
}

// FoldStateFromHistory rebuilds the lookup index for an already-folded
// history, so a history loaded from storage can go on accepting events without
// replaying the log that produced it.
func FoldStateFromHistory(h *History) *FoldState {
	state := NewFoldState()
	for _, arc := range h.Arcs {
		state.arcs[arc.ID] = arc

		for _, msg := range arc.Responses {
			state.responses[msg.ID] = msg
			for _, tc := range msg.ToolCalls {
				state.toolCalls[tc.ID] = tc
				if tc.Interrupt != nil {
					state.interrupts[tc.Interrupt.ID] = tc.Interrupt
				}
			}
		}
		for _, task := range arc.AsyncTasks {
			state.asyncTasks[task.ID] = task
		}
	}
	return state
}

// Replay left-folds the event log into a history. The events must be in
// increasing sequence order, and may skip numbers: the log is the slice
// queried for one arc.
//
// An event the fold rejects stops the replay. A history that silently skipped
// one would disagree with the log it was built from, and every later event
// naming the record that never folded would be rejected in turn.
func Replay(sessionID string, events []events.Event) (*History, error) {
	h := NewHistory(sessionID)
	state := NewFoldState()
	for i := range events {
		ev := &events[i]
		if err := h.Update(ev, state); err != nil {
			return nil, fmt.Errorf("replay %s event %q at seq %d: %w", ev.Typ, ev.ID, ev.Seq, err)
		}
	}
	return h, nil
}

// Update folds one event into the history, using state to find the records the
// event refers to. The event must come after the last one applied, and its type
// must match its payload; neither the history nor LastSeq changes if it is not.
//
// Order matters because an event names the record it acts on by ID, so that
// record has to be folded already. Sequence numbers may skip: the events of
// one arc are interleaved with other arcs' in the log, and a history holds one
// arc's slice of it.
func (h *History) Update(ev *events.Event, state *FoldState) error {
	if ev.Seq <= h.LastSeq {
		return fmt.Errorf("expected event sequence number above %d, got %d", h.LastSeq, ev.Seq)
	}
	if ev.Typ != ev.Payload.EventType() {
		return fmt.Errorf("event type and payload disagree: %s != %s", ev.Typ, ev.Payload.EventType())
	}
	err := h.applyEvent(ev, state)
	if err != nil {
		return err
	}
	h.LastSeq = ev.Seq
	return nil
}

// applyFunc folds a single event of one type into the history.
type applyFunc func(h *History, ev *events.Event, state *FoldState) error

// applyFuncs dispatches each event type to the function that folds it. An
// event type missing from this table is rejected rather than ignored.
var applyFuncs = map[events.EventType]applyFunc{
	events.UserMessageEventType:             (*History).applyUserMessage,
	events.AssistantMessageEventType:        (*History).applyAssistantMessage,
	events.ToolCallEventType:                (*History).applyToolCall,
	events.ToolDispatchedEventType:          (*History).applyToolDispatched,
	events.ToolResultEventType:              (*History).applyToolResult,
	events.ToolEventEventType:               (*History).applyToolEvent,
	events.InterruptEventType:               (*History).applyInterrupt,
	events.InterruptResponseEventType:       (*History).applyInterruptResponse,
	events.AsyncTaskInvokeEventType:         (*History).applyAsyncTaskInvoke,
	events.AsyncTaskResultEventType:         (*History).applyAsyncTaskResult,
	events.AsyncTaskEventEventType:          (*History).applyAsyncTaskEvent,
	events.AsyncTaskInputRequestedEventType: (*History).applyAsyncTaskInputRequested,
	events.AsyncTaskInputProvidedEventType:  (*History).applyAsyncTaskInputProvided,
	events.ErrorEventType:                   (*History).applyError,
	events.GuardrailBlockedEventType:        (*History).applyGuardrailBlocked,
	events.CancelEventType:                  (*History).applyCancel,
	events.ArcClosedEventType:               (*History).applyArcClosed,
}

// applyEvent routes an event to its fold function.
func (h *History) applyEvent(ev *events.Event, state *FoldState) error {
	fn, ok := applyFuncs[ev.Typ]
	if !ok {
		return fmt.Errorf("unhandled event type %q", ev.Typ)
	}
	if err := fn(h, ev, state); err != nil {
		return fmt.Errorf("apply %s: %w", ev.Typ, err)
	}
	return nil
}

// applyUserMessage opens a new arc. This is the only event that creates one;
// every other event attaches to an arc that already exists.
func (h *History) applyUserMessage(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.UserMessagePayload)
	if !ok {
		return errors.New("expected payload type to be UserMessagePayload")
	}

	arc := &Arc{
		ID:        *ev.CorrelationID,
		Seq:       ev.Seq,
		Timestamp: ev.Timestamp,
		Content:   pl.Content,
		Actor:     &pl.Actor,
	}
	h.Arcs = append(h.Arcs, arc)
	state.arcs[arc.ID] = arc

	return nil
}

// applyAssistantMessage appends a reply to the arc it answers.
func (h *History) applyAssistantMessage(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.AssistantMessagePayload)
	if !ok {
		return errors.New("expected payload type to be AssistantMessagePayload")
	}
	msg := state.arcs[*ev.CorrelationID]
	amsg := &AssistantMessage{
		ID:            ev.ID,
		Seq:           ev.Seq,
		CorrelationID: *ev.CorrelationID,
		Timestamp:     ev.Timestamp,
		VendorID:      pl.VendorID,
		Content:       pl.Content,
		Reasoning:     pl.Reasoning,
		Usage:         pl.Usage,
	}
	msg.Responses = append(msg.Responses, amsg)
	state.responses[amsg.ID] = amsg

	return nil
}

// applyToolCall attaches a pending call to the assistant message that
// requested it.
func (h *History) applyToolCall(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.ToolCallPayload)
	if !ok {
		return errors.New("expected payload type to be ToolCallPayload")
	}
	tc := &ToolCall{
		ID:       ev.ID,
		Seq:      ev.Seq,
		VendorID: pl.VendorID,
		ToolName: pl.ToolName,
		Args:     pl.Args,
		Status:   ToolCallStatusPending,
	}
	amsg, ok := state.responses[pl.MessageID]
	if !ok {
		return fmt.Errorf("no assistant message with ID=%s found for tool call", pl.MessageID)
	}
	amsg.ToolCalls = append(amsg.ToolCalls, tc)
	state.toolCalls[tc.ID] = tc
	return nil
}

// applyToolDispatched records that a call was handed to its tool. Unlike its
// neighbours it validates nothing beyond the call existing, because it is an
// observation of something that already happened rather than a transition the
// fold may permit or refuse.
//
// So a dispatch is recorded even for a call that is already terminal. A cancel
// sweeping an arc races the worker that is dispatching one of its calls, and
// that is the case where knowing the tool may have run matters most. Refusing
// the event would fail the whole fold over a bookkeeping record, and dropping
// it would discard the one fact it carries.
//
// It leaves Status alone, so every status but PENDING stays terminal.
func (h *History) applyToolDispatched(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.ToolDispatchedPayload)
	if !ok {
		return errors.New("expected payload type to be ToolDispatchedPayload")
	}
	tc, ok := state.toolCalls[pl.CausationID]
	if !ok {
		return fmt.Errorf("no tool call with ID=%s found for tool dispatch", pl.CausationID)
	}
	tc.Dispatches++
	if tc.DispatchedSeq == nil {
		tc.DispatchedAt = &ev.Timestamp
		tc.DispatchedSeq = &ev.Seq
	}
	return nil
}

// applyToolResult settles a pending call as successful or failed. A result for
// a call that already reached a terminal status is dropped, so a call blocked
// or cancelled mid-flight keeps that outcome.
func (h *History) applyToolResult(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.ToolResultPayload)
	if !ok {
		return errors.New("expected payload type to be ToolResultPayload")
	}
	tc, ok := state.toolCalls[pl.CausationID]
	if !ok {
		return fmt.Errorf("no tool call with ID=%s found for tool call", pl.CausationID)
	}
	if tc.Status != ToolCallStatusPending {
		return nil
	}
	tc.Result = pl.Result
	tc.FinishedAt = &ev.Timestamp
	tc.FinishedSeq = &ev.Seq
	if pl.IsError {
		tc.Status = ToolCallStatusFailed
	} else {
		tc.Status = ToolCallStatusSuccess
	}

	return nil
}

// applyToolEvent appends a progress notification to the call that emitted it.
func (h *History) applyToolEvent(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.ToolEventPayload)
	if !ok {
		return errors.New("expected payload type to be ToolEventPayload")
	}
	tc, ok := state.toolCalls[pl.CausationID]
	if !ok {
		return fmt.Errorf("no tool call with ID=%s found for tool call", pl.CausationID)
	}
	event := &ToolEvent{ID: ev.ID, Seq: ev.Seq, Payload: pl}
	tc.Events = append(tc.Events, event)
	return nil
}

// applyInterrupt holds a tool call until a human decides on it. A call that
// already reached a terminal status cannot be put to a human, and a call can
// only be put to one once.
func (h *History) applyInterrupt(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.InterruptPayload)
	if !ok {
		return errors.New("expected payload type to be InterruptPayload")
	}
	if ev.CorrelationID == nil {
		return fmt.Errorf("event %q of type %q has no correlation ID", ev.ID, ev.Typ)
	}
	call, ok := state.toolCalls[pl.ToolCallID]
	if !ok {
		return fmt.Errorf("no tool call with ID=%s found for interrupt", pl.ToolCallID)
	}
	if call.Status != ToolCallStatusPending {
		return fmt.Errorf("tool call %s is %s and cannot be put to a human", call.ID, call.Status)
	}
	if call.Interrupt != nil {
		return fmt.Errorf("tool call %s already has interrupt %s", call.ID, call.Interrupt.ID)
	}
	interrupt := &Interrupt{
		ID:            ev.ID,
		Seq:           ev.Seq,
		CorrelationID: *ev.CorrelationID,
		ToolCallID:    call.ID,
		Timestamp:     ev.Timestamp,
		Reason:        pl.Reason,
		Context:       pl.Context,
		Status:        InterruptPending,
	}
	call.Interrupt = interrupt
	state.interrupts[interrupt.ID] = interrupt
	return nil
}

// applyInterruptResponse records the human decision, as an approval or a
// refusal. A refusal settles the call, because the call is never going to run.
// An approval leaves it pending, because now it will.
//
// A decision on an interrupt that is no longer pending is dropped, so a call
// cancelled while it waited keeps that outcome.
func (h *History) applyInterruptResponse(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.InterruptResponsePayload)
	if !ok {
		return errors.New("expected payload type to be InterruptResponsePayload")
	}
	interrupt, ok := state.interrupts[pl.CausationID]
	if !ok {
		return fmt.Errorf("no interrupt with ID=%s found for interrupt response", pl.CausationID)
	}
	if interrupt.Status != InterruptPending {
		return nil
	}
	call, ok := state.toolCalls[interrupt.ToolCallID]
	if !ok {
		return fmt.Errorf("no tool call with ID=%s found for interrupt %s", interrupt.ToolCallID, interrupt.ID)
	}

	interrupt.Responder = &pl.Actor
	interrupt.Response = pl.Response
	interrupt.ResolvedSeq = &ev.Seq
	if !pl.IsRejected {
		interrupt.Status = InterruptApproved
		return nil
	}

	interrupt.Status = InterruptRejected
	if call.Status == ToolCallStatusPending {
		call.Status = ToolCallStatusRefused
		call.FinishedAt = &ev.Timestamp
		call.FinishedSeq = &ev.Seq
		call.Result = pl.Response
	}
	return nil
}

// applyAsyncTaskInvoke starts a task under the arc that launched it.
func (h *History) applyAsyncTaskInvoke(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.AsyncTaskInvokePayload)
	if !ok {
		return errors.New("expected payload type to be AsyncTaskInvokePayload")
	}
	if ev.CorrelationID == nil {
		return fmt.Errorf("event %q of type %q has no correlation ID", ev.ID, ev.Typ)
	}
	arc := state.arcs[*ev.CorrelationID]

	task := &AsyncTask{
		ID:            ev.ID,
		Seq:           ev.Seq,
		CorrelationID: *ev.CorrelationID,
		TaskName:      pl.TaskName,
		Status:        AsyncTaskPending,
		Args:          pl.Args,
		Events:        make([]AsyncTaskEvent, 0),
	}

	arc.AsyncTasks = append(arc.AsyncTasks, task)
	state.asyncTasks[task.ID] = task
	return nil
}

// applyAsyncTaskResult settles a running task as successful or failed. A
// result for a task that already finished is dropped, so a cancelled task
// keeps that outcome.
func (h *History) applyAsyncTaskResult(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.AsyncTaskResultPayload)
	if !ok {
		return errors.New("expected payload type to be AsyncTaskResultPayload")
	}
	task, ok := state.asyncTasks[pl.CausationID]
	if !ok {
		return fmt.Errorf("no async task with ID=%s found for async task result", pl.CausationID)
	}
	if task.FinishedAt != nil {
		return nil
	}
	task.Result = pl.Result
	task.FinishedAt = &ev.Timestamp
	task.FinishedSeq = &ev.Seq
	if pl.IsError {
		task.Status = AsyncTaskFailed
	} else {
		task.Status = AsyncTaskSuccess
	}
	return nil
}

// applyAsyncTaskEvent appends a notification to the task's transcript without
// changing its status.
func (h *History) applyAsyncTaskEvent(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.AsyncTaskEventPayload)
	if !ok {
		return errors.New("expected payload type to be AsyncTaskEventPayload")
	}
	task, ok := state.asyncTasks[pl.CausationID]
	if !ok {
		return fmt.Errorf("no async task with ID=%s found for async task event", pl.CausationID)
	}
	appendAsyncTaskEvent(task, ev, AsyncTaskEventNotification, pl.Payload)
	return nil
}

// applyAsyncTaskInputRequested parks a task on the input it is asking for.
func (h *History) applyAsyncTaskInputRequested(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.AsyncTaskInputRequestedPayload)
	if !ok {
		return errors.New("expected payload type to be AsyncTaskInputRequestedPayload")
	}
	task, ok := state.asyncTasks[pl.CausationID]
	if !ok {
		return fmt.Errorf("no async task with ID=%s found for async task input request", pl.CausationID)
	}
	task.Status = AsyncTaskWaiting
	appendAsyncTaskEvent(task, ev, AsyncTaskEventInputRequested, pl.Message)
	return nil
}

// applyAsyncTaskInputProvided delivers the requested input and puts the task
// back to running.
func (h *History) applyAsyncTaskInputProvided(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.AsyncTaskInputProvidedPayload)
	if !ok {
		return errors.New("expected payload type to be AsyncTaskInputProvidedPayload")
	}
	task, ok := state.asyncTasks[pl.CausationID]
	if !ok {
		return fmt.Errorf("no async task with ID=%s found for async task input", pl.CausationID)
	}
	task.Status = AsyncTaskPending
	appendAsyncTaskEvent(task, ev, AsyncTaskEventInputProvided, pl.Message)
	return nil
}

// applyError records a failure against the arc it happened in.
func (h *History) applyError(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.ErrorPayload)
	if !ok {
		return errors.New("expected payload type to be ErrorPayload")
	}
	if ev.CorrelationID == nil {
		return fmt.Errorf("event %q of type %q has no correlation ID", ev.ID, ev.Typ)
	}
	msg := state.arcs[*ev.CorrelationID]
	msg.Errors = append(msg.Errors, &Error{
		ID:            ev.ID,
		Seq:           ev.Seq,
		IsFatal:       pl.IsFatal,
		CorrelationID: *ev.CorrelationID,
		Timestamp:     ev.Timestamp,
		Message:       pl.Message,
		CausationID:   pl.CausationID,
	})
	return nil
}

// applyGuardrailBlocked settles a pending call that a guardrail refused,
// recording the refusal message as its result. A call that already reached a
// terminal status keeps that outcome.
func (h *History) applyGuardrailBlocked(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.GuardrailBlockedPayload)
	if !ok {
		return errors.New("expected payload type to be GuardrailBlockedPayload")
	}
	tc, ok := state.toolCalls[pl.CausationID]
	if !ok {
		return fmt.Errorf("no tool call with ID=%s found for tool call", pl.CausationID)
	}
	if tc.Status != ToolCallStatusPending {
		return nil
	}
	tc.FinishedAt = &ev.Timestamp
	tc.FinishedSeq = &ev.Seq
	tc.Status = ToolCallStatusBlocked
	tc.Result = pl.Message
	return nil
}

// applyCancel closes an arc, stopping its pending tool calls, unanswered
// interrupts and unfinished async tasks. The first cancel an arc receives is
// the one that sticks.
func (h *History) applyCancel(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.CancelPayload)
	if !ok {
		return errors.New("expected payload type to be CancelPayload")
	}
	if ev.CorrelationID == nil {
		return fmt.Errorf("event %q of type %q has no correlation ID", ev.ID, ev.Typ)
	}
	arc := state.arcs[*ev.CorrelationID]

	stoppedSomething := false

	for _, amsg := range arc.Responses {
		for _, call := range amsg.ToolCalls {
			if call.Status != ToolCallStatusPending {
				continue
			}
			call.Status = ToolCallStatusCancelled
			call.FinishedAt = &ev.Timestamp
			call.FinishedSeq = &ev.Seq
			call.Result = pl.Reason
			if call.Interrupt != nil && call.Interrupt.Status == InterruptPending {
				// Nobody will answer a decision about a call that is no
				// longer going to run.
				call.Interrupt.Status = InterruptCancelled
				call.Interrupt.ResolvedSeq = &ev.Seq
			}
			stoppedSomething = true
		}
	}

	for _, task := range arc.AsyncTasks {
		if task.Status != AsyncTaskPending && task.Status != AsyncTaskWaiting {
			continue
		}
		task.Status = AsyncTaskCancelled
		task.FinishedAt = &ev.Timestamp
		task.FinishedSeq = &ev.Seq
		stoppedSomething = true
	}

	if arc.Cancel == nil {
		// A cancel that reached an arc the agent had not closed terminated it,
		// even when there was nothing in flight to mark cancelled. Only a
		// cancel arriving after the close stopped nothing at all.
		arc.Cancel = &Cancel{
			ID:             ev.ID,
			Seq:            ev.Seq,
			Arc:            arc.ID,
			Timestamp:      ev.Timestamp,
			Actor:          &ev.Actor,
			Reason:         pl.Reason,
			TerminatedWork: stoppedSomething || arc.Closed == nil,
		}
	}
	return nil
}

// applyArcClosed records that the agent finished with an arc. A second close
// is dropped, so the first one sticks, as with a cancel.
func (h *History) applyArcClosed(ev *events.Event, state *FoldState) error {
	pl, ok := ev.Payload.(events.ArcClosedPayload)
	if !ok {
		return errors.New("expected payload type to be ArcClosedPayload")
	}
	if ev.CorrelationID == nil {
		return fmt.Errorf("event %q of type %q has no correlation ID", ev.ID, ev.Typ)
	}
	arc, ok := state.arcs[*ev.CorrelationID]
	if !ok {
		return fmt.Errorf("no arc with ID=%s found for arc close", *ev.CorrelationID)
	}
	if _, ok := state.responses[pl.CausationID]; !ok {
		return fmt.Errorf("no assistant message with ID=%s found for arc close", pl.CausationID)
	}
	if arc.Closed != nil {
		return nil
	}
	arc.Closed = &ArcClosed{
		ID:          ev.ID,
		Seq:         ev.Seq,
		Timestamp:   ev.Timestamp,
		CausationID: pl.CausationID,
	}
	return nil
}

// appendAsyncTaskEvent adds one entry to a task's transcript.
func appendAsyncTaskEvent(task *AsyncTask, ev *events.Event, typ AsyncTaskEventType, payload any) {
	task.Events = append(task.Events, AsyncTaskEvent{
		ID:        ev.ID,
		Seq:       ev.Seq,
		Timestamp: ev.Timestamp,
		Type:      typ,
		Payload:   payload,
	})
}
