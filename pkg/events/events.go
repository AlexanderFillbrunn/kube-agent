package events

import (
	"encoding/json"
	"fmt"
	"time"

	"fillbrunn.ai/kube-agent/pkg/users"
)

type EventType string

const (
	UserMessageEventType             EventType = "USER_MESSAGE"
	AssistantMessageEventType        EventType = "ASSISTANT_MESSAGE"
	ToolCallEventType                EventType = "TOOL_CALL"
	ToolDispatchedEventType          EventType = "TOOL_DISPATCHED"
	ToolResultEventType              EventType = "TOOL_RESULT"
	ToolEventEventType               EventType = "TOOL_EVENT"
	InterruptEventType               EventType = "INTERRUPT"
	InterruptResponseEventType       EventType = "INTERRUPT_RESPONSE"
	AsyncTaskInvokeEventType         EventType = "ASYNC_TASK_INVOKE"
	AsyncTaskResultEventType         EventType = "ASYNC_TASK_RESULT"
	AsyncTaskEventEventType          EventType = "ASYNC_TASK_EVENT"
	AsyncTaskInputRequestedEventType EventType = "ASYNC_TASK_INPUT_REQUESTED"
	AsyncTaskInputProvidedEventType  EventType = "ASYNC_TASK_INPUT_PROVIDED"
	ErrorEventType                   EventType = "ERROR"
	GuardrailBlockedEventType        EventType = "GUARDRAIL_BLOCKED"
	CancelEventType                  EventType = "CANCEL"
	ArcClosedEventType               EventType = "ARC_CLOSED"
)

type EventPayload interface {
    EventType() EventType
    eventPayload()
}

// Event represents an event in the chat history. Payload holds the
// concrete, typed payload for ev.Typ (e.g. UserMessagePayload)
type Event struct {
	SessionID     string      	`json:"session_id"`
	AgentID       string      	`json:"agent_id"`
	ID            string      	`json:"id"`
	Seq           int         	`json:"seq"`
	Timestamp     time.Time   	`json:"timestamp"`
	Typ           EventType   	`json:"type"`
	CorrelationID *string     	`json:"correlation_id"`
	Actor         users.Actor 	`json:"actor"`
	Payload       EventPayload	`json:"payload"`
}

// eventJSON mirrors Event but types Payload as json.RawMessage so decoding
// can be deferred until Typ is known.
type eventJSON struct {
	SessionID     string          `json:"session_id"`
	AgentID       string          `json:"agent_id"`
	ID            string          `json:"id"`
	Seq           int             `json:"seq"`
	Timestamp     time.Time       `json:"timestamp"`
	Typ           EventType       `json:"type"`
	CorrelationID *string         `json:"correlation_id"`
	Actor         users.Actor     `json:"actor"`
	Payload       json.RawMessage `json:"payload"`
}

func (e *Event) UnmarshalJSON(data []byte) error {
	var aux eventJSON
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	payload, err := JSONCodec{}.Decode(aux.Typ, aux.Payload)
	if err != nil {
		return fmt.Errorf("decode payload for %s: %w", aux.Typ, err)
	}
	e.SessionID = aux.SessionID
	e.AgentID = aux.AgentID
	e.ID = aux.ID
	e.Seq = aux.Seq
	e.Timestamp = aux.Timestamp
	e.Typ = aux.Typ
	e.CorrelationID = aux.CorrelationID
	e.Actor = aux.Actor
	e.Payload = payload
	return nil
}
