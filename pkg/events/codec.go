package events

import (
	"encoding/json"
	"fmt"
)

// PayloadCodec encodes and decodes event payloads. Implementations pick the
// wire format (JSON, protobuf, msgpack, ...); Event itself stays agnostic.
type PayloadCodec interface {
	Encode(payload EventPayload) ([]byte, error)
	Decode(t EventType, data []byte) (EventPayload, error)
}

type JSONCodec struct{}

func (JSONCodec) Encode(payload EventPayload) ([]byte, error) {
	return json.Marshal(payload)
}

func (JSONCodec) Decode(t EventType, data []byte) (EventPayload, error) {
	switch t {
	case UserMessageEventType:
		var p UserMessagePayload
		return p, json.Unmarshal(data, &p)
	case AssistantMessageEventType:
		var p AssistantMessagePayload
		return p, json.Unmarshal(data, &p)
	case ToolCallEventType:
		var p ToolCallPayload
		return p, json.Unmarshal(data, &p)
	case ToolDispatchedEventType:
		var p ToolDispatchedPayload
		return p, json.Unmarshal(data, &p)
	case ToolResultEventType:
		var p ToolResultPayload
		return p, json.Unmarshal(data, &p)
	case ToolEventEventType:
		var p ToolEventPayload
		return p, json.Unmarshal(data, &p)
	case InterruptEventType:
		var p InterruptPayload
		return p, json.Unmarshal(data, &p)
	case InterruptResponseEventType:
		var p InterruptResponsePayload
		return p, json.Unmarshal(data, &p)
	case AsyncTaskInvokeEventType:
		var p AsyncTaskInvokePayload
		return p, json.Unmarshal(data, &p)
	case AsyncTaskResultEventType:
		var p AsyncTaskResultPayload
		return p, json.Unmarshal(data, &p)
	case AsyncTaskEventEventType:
		var p AsyncTaskEventPayload
		return p, json.Unmarshal(data, &p)
	case AsyncTaskInputRequestedEventType:
		var p AsyncTaskInputRequestedPayload
		return p, json.Unmarshal(data, &p)
	case AsyncTaskInputProvidedEventType:
		var p AsyncTaskInputProvidedPayload
		return p, json.Unmarshal(data, &p)
	case ErrorEventType:
		var p ErrorPayload
		return p, json.Unmarshal(data, &p)
	case GuardrailBlockedEventType:
		var p GuardrailBlockedPayload
		return p, json.Unmarshal(data, &p)
	case CancelEventType:
		var p CancelPayload
		return p, json.Unmarshal(data, &p)
	case ArcClosedEventType:
		var p ArcClosedPayload
		return p, json.Unmarshal(data, &p)
	default:
		return nil, fmt.Errorf("unrecognized event type %q", t)
	}
}

func DecodeAs[T any](c PayloadCodec, t EventType, data []byte) (T, error) {
	v, err := c.Decode(t, data)
	if err != nil {
		var zero T
		return zero, err
	}
	typed, ok := v.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("payload for %q is %T, not %T", t, v, zero)
	}
	return typed, nil
}
