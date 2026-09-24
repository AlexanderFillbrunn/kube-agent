package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"fillbrunn.ai/kube-agent/pkg/history"
)

// This file is the OpenAI half of the encoding step ENCODING.md specifies: it
// turns a ModelTranscript into the messages and tools of a Chat Completions
// request. It decides how the transcript is carried, never what the model is
// told; anything it would have to invent belongs to the projection.
//
// OpenAI accepts adjacent same-role messages and constrains neither the first
// nor the last role, so three of the four repairs the document lists are not
// reachable here. Merging is the one that would still be worth doing for the
// model's sake, and it needs the actor behind each user message to write the
// headers with. A ModelTextMessage does not carry one, so merging waits for a
// transcript that does rather than being faked from position.

const (
	openAIRoleSystem    = "system"
	openAIRoleDeveloper = "developer"
	openAIRoleUser      = "user"
	openAIRoleAssistant = "assistant"
	openAIRoleTool      = "tool"
)

// openAIMessage is one entry of the request's `messages` array. Content is a
// pointer because an assistant message that only requests calls has no content
// at all, which is a different thing to the empty string.
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    *string          `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

// openAIFunctionCall carries the arguments as a JSON string, which is the
// shape OpenAI both emits and expects back.
type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// encoder holds the per-request state of one encoding.
type encoder struct {
	// vendorIDs maps a tool call's log ID to the identifier the provider
	// issued for it. A call the model made under one ID and is shown back
	// under another is a call it cannot match to its own request.
	vendorIDs map[string]string

	// noteRole is where a note is carried. Every note in one request goes
	// through the same carrier, so role stays a stable signal of provenance.
	noteRole string
}

// encodeTranscript renders a transcript as the messages of one request. The
// system prompt is prepended as the real instruction it is; notes never go
// there, because an instruction sits in the cached prefix for the session and
// a note belongs at the position it was raised.
func (e encoder) encodeTranscript(transcript *history.ModelTranscript, systemPrompt string) ([]openAIMessage, error) {
	messages := make([]openAIMessage, 0, len(transcript.Messages)+1)
	if systemPrompt != "" {
		messages = append(messages, textMessage(openAIRoleSystem, systemPrompt))
	}

	for _, message := range transcript.Messages {
		encoded, err := e.encodeMessage(message)
		if err != nil {
			return nil, err
		}
		messages = append(messages, encoded...)
	}
	return messages, nil
}

// encodeMessage renders one transcript message. An assistant message becomes
// several, since OpenAI carries each of its results in a message of its own.
func (e encoder) encodeMessage(message history.ModelMessage) ([]openAIMessage, error) {
	switch m := message.(type) {
	case history.ModelTextMessage:
		role := openAIRoleUser
		if m.Role() == history.ModelRoleInstruction {
			role = openAIRoleSystem
		}
		return []openAIMessage{textMessage(role, m.Content)}, nil

	case history.ModelNoteMessage:
		return []openAIMessage{textMessage(e.noteRole, m.Content)}, nil

	case history.ModelAssistantMessage:
		return e.encodeAssistant(m)

	case history.ModelToolMessage:
		return []openAIMessage{{
			Role:       openAIRoleTool,
			Content:    &m.Content,
			ToolCallID: e.toolCallID(m.ToolCallID),
		}}, nil
	}
	return nil, fmt.Errorf("cannot encode model message of type %T", message)
}

func (e encoder) encodeAssistant(message history.ModelAssistantMessage) ([]openAIMessage, error) {
	encoded := openAIMessage{Role: openAIRoleAssistant}
	if message.Content != "" {
		encoded.Content = &message.Content
	}

	for _, call := range message.ToolCalls {
		args, err := json.Marshal(call.Args)
		if err != nil {
			return nil, fmt.Errorf("encode arguments of tool call %s: %w", call.ID, err)
		}
		encoded.ToolCalls = append(encoded.ToolCalls, openAIToolCall{
			ID:       e.toolCallID(call.ID),
			Type:     "function",
			Function: openAIFunctionCall{Name: call.Name, Arguments: string(args)},
		})
	}
	return []openAIMessage{encoded}, nil
}

// toolCallID returns the identifier a call travels under. A call the provider
// issued keeps the provider's own ID; a stand-in the projection synthesized
// never had one, so it gets an ID derived from its log ID, which is stable
// across the requests that replay it.
func (e encoder) toolCallID(logID string) string {
	if vendorID, ok := e.vendorIDs[logID]; ok && vendorID != "" {
		return vendorID
	}
	sum := sha256.Sum256([]byte(logID))
	return "call_" + hex.EncodeToString(sum[:])[:24]
}

func textMessage(role, content string) openAIMessage {
	return openAIMessage{Role: role, Content: &content}
}

// vendorToolCallIDs indexes the provider identifiers a history holds, so the
// encoder can look one up by the log ID the transcript carries.
func vendorToolCallIDs(h *history.History) map[string]string {
	ids := map[string]string{}
	for _, arc := range h.Arcs {
		for _, response := range arc.Responses {
			for _, call := range response.ToolCalls {
				if call.VendorID != "" {
					ids[call.ID] = call.VendorID
				}
			}
		}
	}
	return ids
}

// encodeTools renders the registry as the request's tool list.
func encodeTools(registry *ToolRegistry) []openAITool {
	if registry == nil {
		return nil
	}
	tools := registry.All()
	encoded := make([]openAITool, 0, len(tools))
	for _, tool := range tools {
		encoded = append(encoded, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}
	return encoded
}
