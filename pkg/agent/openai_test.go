package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testArcID = "arc-1"

// newTestServer serves one canned response and captures the request that asked
// for it.
func newTestServer(t *testing.T, body string, captured *openAIRequest) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if captured != nil {
			if err := json.Unmarshal(raw, captured); err != nil {
				t.Errorf("unmarshal request: %v", err)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func newTestLLM(t *testing.T, server *httptest.Server) *OpenAI {
	t.Helper()
	return NewOpenAI("test-key", "gpt-5", WithBaseURL(server.URL))
}

// userArcHistory returns a history holding one arc and nothing else.
func userArcHistory(t *testing.T) *history.History {
	t.Helper()
	arc := testArcID
	actor := users.Actor{UserID: "alice", TenantID: "acme"}
	h, err := history.Replay("session", []events.Event{{
		SessionID: "session", ID: arc, Seq: 0, Timestamp: time.Unix(0, 0),
		Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: actor,
		Payload: events.UserMessagePayload{Content: "how many pods are pending?", Actor: actor},
	}})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return h
}

func collectTokens() (*[]string, Sink[string]) {
	var tokens []string
	return &tokens, SinkFromFunc(func(tok string) error {
		tokens = append(tokens, tok)
		return nil
	})
}

func TestStreamResponseYieldsTokensAndReply(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-9","model":"gpt-5","choices":[{"delta":{"role":"assistant","content":"Checking"}}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"delta":{"content":" pods"}}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"list_pods","arguments":"{\"namespace\":"}}]}}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"kube-system\"}"}}]}}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chatcmpl-9","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":18,"prompt_tokens_details":{"cached_tokens":64}}}`,
		"data: [DONE]",
		"",
	}, "\n\n")

	llm := newTestLLM(t, newTestServer(t, stream, nil))
	tokens, tokSink := collectTokens()

	response, err := llm.StreamResponse(context.Background(), userArcHistory(t), &LLMOptions{}, tokSink)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}

	if got := strings.Join(*tokens, "|"); got != "Checking| pods" {
		t.Errorf("tokens = %q, want %q", got, "Checking| pods")
	}
	if response.Content != "Checking pods" {
		t.Errorf("content = %q, want %q", response.Content, "Checking pods")
	}
	if response.VendorID == nil || *response.VendorID != "chatcmpl-9" {
		t.Errorf("vendor ID = %v, want chatcmpl-9", response.VendorID)
	}
	if response.ModelID != "gpt-5" {
		t.Errorf("model = %q, want gpt-5", response.ModelID)
	}

	if len(response.ToolCalls) != 1 {
		t.Fatalf("requested %d calls, want 1", len(response.ToolCalls))
	}
	call := response.ToolCalls[0]
	if call.VendorID != "call_abc" || call.ToolName != "list_pods" {
		t.Errorf("call = %+v, want the list_pods call issued as call_abc", call)
	}
	args, ok := call.Args.(map[string]any)
	if !ok || args["namespace"] != "kube-system" {
		t.Errorf("args = %#v, want namespace kube-system", call.Args)
	}

	want := TokenUsage{Input: 120, CachedInput: 64, Output: 18}
	if response.Usage == nil || *response.Usage != want {
		t.Errorf("usage = %+v, want %+v", response.Usage, want)
	}
}

// A stream reports nothing about its cost unless the request asks for it.
func TestStreamAsksForUsage(t *testing.T) {
	var captured openAIRequest
	stream := "data: [DONE]\n\n"
	llm := newTestLLM(t, newTestServer(t, stream, &captured))
	_, tokSink := collectTokens()

	if _, err := llm.StreamResponse(context.Background(), userArcHistory(t), &LLMOptions{}, tokSink); err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	if captured.StreamOptions == nil || !captured.StreamOptions.IncludeUsage {
		t.Errorf("stream options = %+v, want usage included", captured.StreamOptions)
	}
}

// An endpoint that reports no usage leaves it unrecorded, because a zero in
// the log has to mean a measured zero.
func TestUnreportedUsageStaysNil(t *testing.T) {
	body := `{"id":"chatcmpl-3","model":"gpt-5","choices":[{"message":{"content":"two"},"finish_reason":"stop"}]}`
	llm := newTestLLM(t, newTestServer(t, body, nil))

	response, err := llm.Response(context.Background(), userArcHistory(t), &LLMOptions{})
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if response.Usage != nil {
		t.Errorf("usage = %+v, want nil", response.Usage)
	}
	if recordedUsage(response.Usage) != nil {
		t.Error("an unreported cost reached the payload")
	}
}

func TestResponseEncodesPromptAndTools(t *testing.T) {
	registry := ToolRegistry{tools: map[string]registeredTool{}}
	if err := registry.RegisterTool(&mcp.Tool{
		Name:        "list_pods",
		Description: "list pods in a namespace",
		InputSchema: map[string]any{"type": "object"},
	}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}

	var captured openAIRequest
	body := `{"id":"chatcmpl-1","model":"gpt-5","choices":[{"message":{"content":"two"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":80,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":0}}}`
	llm := newTestLLM(t, newTestServer(t, body, &captured))

	opts := &LLMOptions{SystemPrompt: "you are a cluster operator", Tools: &registry}
	response, err := llm.Response(context.Background(), userArcHistory(t), opts)
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if captured.Stream {
		t.Error("Response asked for a stream")
	}
	if len(captured.Messages) != 2 {
		t.Fatalf("sent %d messages, want 2", len(captured.Messages))
	}
	if captured.Messages[0].Role != openAIRoleSystem || *captured.Messages[0].Content != opts.SystemPrompt {
		t.Errorf("first message = %+v, want the system prompt", captured.Messages[0])
	}
	if captured.Messages[1].Role != openAIRoleUser {
		t.Errorf("second message role = %q, want user", captured.Messages[1].Role)
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Function.Name != "list_pods" {
		t.Errorf("tools = %+v, want the list_pods function", captured.Tools)
	}
	if response.Content != "two" || len(response.ToolCalls) != 0 {
		t.Errorf("response = %+v, want the answer two and no calls", response)
	}
	if want := (TokenUsage{Input: 80, Output: 3}); response.Usage == nil || *response.Usage != want {
		t.Errorf("usage = %+v, want %+v", response.Usage, want)
	}
}

// A call the model already issued has to come back to it under the identifier
// it issued, and with its result attached to that identifier.
func TestEncodePairsCallsUnderVendorIDs(t *testing.T) {
	arc := testArcID
	at := time.Unix(0, 0)
	actor := users.Actor{UserID: "alice", TenantID: "acme"}
	h, err := history.Replay("session", []events.Event{
		{
			SessionID: "session", ID: arc, Seq: 0, Timestamp: at,
			Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: actor,
			Payload: events.UserMessagePayload{Content: "how many pods are pending?", Actor: actor},
		},
		{
			SessionID: "session", ID: "msg-1", Seq: 1, Timestamp: at,
			Typ: events.AssistantMessageEventType, CorrelationID: &arc,
			Payload: events.AssistantMessagePayload{ModelID: "gpt-5", Content: "let me look"},
		},
		{
			SessionID: "session", ID: "tc-1", Seq: 2, Timestamp: at,
			Typ: events.ToolCallEventType, CorrelationID: &arc,
			Payload: events.ToolCallPayload{
				MessageID: "msg-1", VendorID: "call_abc", ToolName: "list_pods",
				Args: map[string]any{"namespace": "kube-system"},
			},
		},
		{
			SessionID: "session", ID: "tr-1", Seq: 3, Timestamp: at,
			Typ: events.ToolResultEventType, CorrelationID: &arc,
			Payload: events.ToolResultPayload{CausationID: "tc-1", Result: "2 pending"},
		},
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	var captured openAIRequest
	body := `{"id":"chatcmpl-2","model":"gpt-5","choices":[{"message":{"content":"two"},"finish_reason":"stop"}]}`
	llm := newTestLLM(t, newTestServer(t, body, &captured))

	if _, err := llm.Response(context.Background(), h, &LLMOptions{}); err != nil {
		t.Fatalf("Response: %v", err)
	}

	if len(captured.Messages) != 3 {
		t.Fatalf("sent %d messages, want user, assistant and tool", len(captured.Messages))
	}
	assistant := captured.Messages[1]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_abc" {
		t.Fatalf("assistant tool calls = %+v, want the call under call_abc", assistant.ToolCalls)
	}
	if assistant.ToolCalls[0].Function.Arguments != `{"namespace":"kube-system"}` {
		t.Errorf("arguments = %q", assistant.ToolCalls[0].Function.Arguments)
	}
	result := captured.Messages[2]
	if result.Role != openAIRoleTool || result.ToolCallID != "call_abc" {
		t.Errorf("result message = %+v, want a tool message keyed by call_abc", result)
	}
}
