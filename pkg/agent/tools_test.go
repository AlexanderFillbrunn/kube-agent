package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registryWith returns a registry holding one tool under the given handler.
func registryWith(t *testing.T, name string, handler mcp.ToolHandler) *ToolRegistry {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.RegisterTool(&mcp.Tool{Name: name}, handler); err != nil {
		t.Fatalf("register: %v", err)
	}
	return registry
}

func TestInvokePassesArgumentsAndReturnsText(t *testing.T) {
	var got json.RawMessage
	registry := registryWith(t, "list_pods", func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		got = req.Params.Arguments
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "2 pending"}}}, nil
	})

	outcome, err := registry.Invoke(context.Background(), "list_pods", map[string]any{"namespace": "kube-system"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(got) != `{"namespace":"kube-system"}` {
		t.Errorf("handler saw arguments %s", got)
	}
	if outcome.Result != "2 pending" || outcome.IsError {
		t.Errorf("outcome = %+v, want the text result and no error", outcome)
	}
}

// Structured output survives as data rather than being flattened into prose.
func TestInvokePrefersStructuredContent(t *testing.T) {
	registry := registryWith(t, "count", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "2"}},
			StructuredContent: map[string]any{"pending": 2},
		}, nil
	})

	outcome, err := registry.Invoke(context.Background(), "count", nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	structured, ok := outcome.Result.(map[string]any)
	if !ok || structured["pending"] != 2 {
		t.Errorf("result = %#v, want the structured content", outcome.Result)
	}
}

// A tool reporting its own failure is an outcome, not an error: the model can
// correct it.
func TestInvokeReportsToolFailureAsOutcome(t *testing.T) {
	registry := registryWith(t, "restart", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "no such deployment"}},
		}, nil
	})

	outcome, err := registry.Invoke(context.Background(), "restart", nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !outcome.IsError || outcome.Result != "no such deployment" {
		t.Errorf("outcome = %+v, want a failure the model is told about", outcome)
	}
}

func TestInvokeErrorsOnBrokenAndUnknownTools(t *testing.T) {
	registry := registryWith(t, "broken", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("connection refused")
	})

	if _, err := registry.Invoke(context.Background(), "broken", nil); err == nil {
		t.Error("a handler that failed returned no error")
	}
	if _, err := registry.Invoke(context.Background(), "absent", nil); err == nil {
		t.Error("an unregistered tool returned no error")
	}
}
