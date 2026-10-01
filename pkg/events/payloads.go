package events

import (
	"fillbrunn.ai/kube-agent/pkg/users"
)

type UserMessagePayload struct {
	Content string      `json:"content"`
	Actor   users.Actor `json:"actor"`
}

func (UserMessagePayload) EventType() EventType {
    return UserMessageEventType
}
func (UserMessagePayload) eventPayload() {}

type AssistantMessagePayload struct {
	VendorID     *string `json:"vendor_id,omitempty"`
	ModelID      string  `json:"model_id"`
	Content      string  `json:"content"`
	Policy       *string `json:"policy,omitempty"`
	CancelPolicy *string `json:"cancel_policy,omitempty"`
	Reasoning    []byte  `json:"reasoning,omitempty"`

	// Usage is what the message cost, as the provider reported it. It is nil
	// where the provider reported nothing, which is not the same as zero.
	Usage *TokenUsage `json:"usage,omitempty"`
}

// TokenUsage is what one message cost, in tokens.
type TokenUsage struct {
	// Input counts every prompt token, CachedInput included.
	Input int `json:"input"`

	// CachedInput is the part of the prompt served from the provider's cache.
	CachedInput int `json:"cached_input"`

	// Output counts the tokens the model generated, reasoning included.
	Output int `json:"output"`
}

func (AssistantMessagePayload) EventType() EventType {
    return AssistantMessageEventType
}
func (AssistantMessagePayload) eventPayload() {}

type ToolCallPayload struct {
	MessageID string `json:"message_id"`
	VendorID  string `json:"vendor_id"`
	ToolName  string `json:"tool_name"`
	Args      any    `json:"args"`
}

func (ToolCallPayload) EventType() EventType {
    return ToolCallEventType
}
func (ToolCallPayload) eventPayload() {}

// ToolDispatchedPayload records that a tool call was handed to its tool. It is
// execution bookkeeping and is never disclosed to the model.
//
// It is written before the call is dispatched and never amended, so it says
// the call was handed over and not that it is still running. A retry writes
// another one.
type ToolDispatchedPayload struct {
	CausationID string `json:"causation_id"`
}

func (ToolDispatchedPayload) EventType() EventType {
    return ToolDispatchedEventType
}
func (ToolDispatchedPayload) eventPayload() {}

type ToolResultPayload struct {
	CausationID string `json:"causation_id"`
	Result      any    `json:"result"`
	IsError     bool   `json:"is_error"`
}

func (ToolResultPayload) EventType() EventType {
    return ToolResultEventType
}
func (ToolResultPayload) eventPayload() {}

type ToolEventPayload struct {
	CausationID string `json:"causation_id"`
	Payload     any    `json:"payload"`
}

func (ToolEventPayload) EventType() EventType {
    return ToolEventEventType
}
func (ToolEventPayload) eventPayload() {}

// InterruptPayload pauses a tool call until a human decides on it. It always
// names the call it holds up. A policy that refuses a call without asking
// anyone writes a GuardrailBlockedPayload instead.
type InterruptPayload struct {
	// ToolCallID is the call waiting on the decision.
	ToolCallID string `json:"tool_call_id"`

	// Reason is what the human is being asked to decide about, in words.
	Reason string `json:"reason"`

	// Context is what the decision is about beyond the call's own arguments.
	// It is set when the interrupt is raised and never changes.
	Context any `json:"context,omitempty"`
}

func (InterruptPayload) EventType() EventType {
    return InterruptEventType
}
func (InterruptPayload) eventPayload() {}

type InterruptResponsePayload struct {
	CausationID string      `json:"causation_id"`
	Response    any         `json:"response"`
	IsRejected  bool        `json:"is_rejected"`
	Actor       users.Actor `json:"actor"`
}

func (InterruptResponsePayload) EventType() EventType {
    return InterruptResponseEventType
}
func (InterruptResponsePayload) eventPayload() {}

type AsyncTaskInvokePayload struct {
	TaskName string `json:"task_name"`
	Args     any    `json:"args"`
}

func (AsyncTaskInvokePayload) EventType() EventType {
    return AsyncTaskInvokeEventType
}
func (AsyncTaskInvokePayload) eventPayload() {}

// AsyncTaskOutcome is how a task ended, as the runner reported it. A result
// can only say that a task is over, so it carries none of the states a running
// task has.
type AsyncTaskOutcome string

const (
	// AsyncTaskOutcomeSuccess is a task that finished and returned a result.
	AsyncTaskOutcomeSuccess AsyncTaskOutcome = "SUCCESS"
	// AsyncTaskOutcomeFailed is a task that ran and reported a failure.
	AsyncTaskOutcomeFailed AsyncTaskOutcome = "FAILED"
	// AsyncTaskOutcomeCancelled is a runner reporting that it stopped. Only a
	// runner can say this, since cancelling an arc asks a task to stop without
	// settling it.
	AsyncTaskOutcomeCancelled AsyncTaskOutcome = "CANCELLED"
)

// Valid reports whether the outcome is one a runner may report. The fold
// refuses a result carrying anything else.
func (o AsyncTaskOutcome) Valid() bool {
	switch o {
	case AsyncTaskOutcomeSuccess, AsyncTaskOutcomeFailed, AsyncTaskOutcomeCancelled:
		return true
	}
	return false
}

// AsyncTaskResultPayload settles a task. Outcome is a value rather than an
// error flag, because work that was called off is not work that failed.
type AsyncTaskResultPayload struct {
	CausationID string           `json:"causation_id"`
	Result      any              `json:"result"`
	Outcome     AsyncTaskOutcome `json:"outcome"`
}

func (AsyncTaskResultPayload) EventType() EventType {
    return AsyncTaskResultEventType
}
func (AsyncTaskResultPayload) eventPayload() {}

// AsyncTaskEventPayload records a non-blocking notification emitted by a task.
type AsyncTaskEventPayload struct {
	CausationID string `json:"causation_id"`
	Payload     any    `json:"payload"`
}

func (AsyncTaskEventPayload) EventType() EventType {
    return AsyncTaskEventEventType
}
func (AsyncTaskEventPayload) eventPayload() {}

type AsyncTaskInputRequestedPayload struct {
	CausationID string `json:"causation_id"`
	Message     string `json:"message"`
}

func (AsyncTaskInputRequestedPayload) EventType() EventType {
    return AsyncTaskInputRequestedEventType
}
func (AsyncTaskInputRequestedPayload) eventPayload() {}

type AsyncTaskInputProvidedPayload struct {
	CausationID string `json:"causation_id"`
	Message     string `json:"message"`
}

func (AsyncTaskInputProvidedPayload) EventType() EventType {
    return AsyncTaskInputProvidedEventType
}
func (AsyncTaskInputProvidedPayload) eventPayload() {}

type ErrorPayload struct {
	Message     string  `json:"message"`
	IsFatal		bool    `json:"is_fatal"`
	CausationID *string `json:"causation_id,omitempty"`
}

func (ErrorPayload) EventType() EventType {
    return ErrorEventType
}
func (ErrorPayload) eventPayload() {}

type GuardrailBlockedPayload struct {
	Message     string `json:"message"`
	CausationID string `json:"causation_id"`
}

func (GuardrailBlockedPayload) EventType() EventType {
    return GuardrailBlockedEventType
}
func (GuardrailBlockedPayload) eventPayload() {}

// ArcClosedPayload closes an arc. The agent emits it once a response has
// requested no tool calls and no async task under the arc is still running.
type ArcClosedPayload struct {
	// CausationID is the assistant message that concluded the arc.
	CausationID string `json:"causation_id"`
}

func (ArcClosedPayload) EventType() EventType {
	return ArcClosedEventType
}
func (ArcClosedPayload) eventPayload() {}

type CancelPayload struct {
	Reason *string `json:"reason,omitempty"`
}

func (CancelPayload) EventType() EventType {
    return CancelEventType
}
func (CancelPayload) eventPayload() {}