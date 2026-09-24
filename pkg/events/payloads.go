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

	// Usage is what the message cost, as the provider reported it, and is nil
	// where it reported nothing. Nil is not zero: a message whose cost went
	// unreported must not be summed in as a free one.
	Usage *TokenUsage `json:"usage,omitempty"`
}

// TokenUsage is what one message cost, in tokens.
type TokenUsage struct {
	// Input counts every prompt token, the cached ones included. CachedInput
	// is a part of this number rather than an addition to it, which is how
	// providers report it and how they bill it.
	Input int `json:"input"`

	// CachedInput is the part of the prompt served from the provider's cache,
	// billed at a lower rate than the rest.
	CachedInput int `json:"cached_input"`

	// Output counts the tokens the model generated, reasoning tokens included
	// where the provider charges for them.
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
// the one payload that says nothing about the conversation: it is execution
// bookkeeping, never disclosed to the model, and exists so a worker recovering
// an abandoned arc can tell a call that may have taken effect from one that
// provably never ran.
//
// It is written before the call is dispatched and never amended, so it states
// that the call left the building and not that it is still running. A retry
// writes another one rather than replacing this.
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

// InterruptPayload pauses a tool call until a human decides on it. An
// interrupt is the human-in-the-loop gate and nothing else: it always names
// the call it holds up. A policy that refuses a call without asking anyone is
// a GuardrailBlockedPayload.
type InterruptPayload struct {
	// ToolCallID is the call waiting on the decision.
	ToolCallID string `json:"tool_call_id"`

	// Reason is what the human is being asked to decide about, in words.
	Reason string `json:"reason"`

	// Context is what the decision is about beyond the call's own arguments:
	// what the call would touch, which policy asked for a human, whatever
	// else the person deciding needs. It is set when the interrupt is raised
	// and never changes.
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

type AsyncTaskResultPayload struct {
	CausationID string `json:"causation_id"`
	Result      any    `json:"result"`
	IsError     bool   `json:"is_error"`
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
// requested no tool calls and no async task under the arc is still running, so
// closing is a recorded fact rather than something a reader infers from the
// shape of the last message.
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