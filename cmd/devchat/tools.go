package main

import (
	"context"
	"encoding/json"
	"fmt"

	"fillbrunn.ai/kube-agent/pkg/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tools here answer from canned data and touch no cluster. They exist to
// give the agent loop something to call, and every one of them is a lie a real
// tool would have to go and find out.
//
// They also stand as the worked example of the contract a tool call carries. A
// call is synchronous, so a handler that returns has said what happened; work
// that cannot be accounted for by the time it returns belongs in an async task
// instead. A handler that gives up part way says so in its result, because a
// returned error is recorded as a definite failure — see restartDeployment.

// registerFakeTools fills a registry with the canned tools the harness offers.
func registerFakeTools(registry *agent.ToolRegistry) error {
	tools := []struct {
		tool    *mcp.Tool
		handler mcp.ToolHandler
	}{
		{
			tool: &mcp.Tool{
				Name:        "list_pods",
				Description: "List the pods in a namespace and their phase.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"namespace": map[string]any{
							"type":        "string",
							"description": "The namespace to list. Defaults to the whole cluster.",
						},
					},
				},
			},
			handler: listPods,
		},
		{
			tool: &mcp.Tool{
				Name:        "restart_deployment",
				Description: "Roll out a restart of one deployment.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":      map[string]any{"type": "string"},
						"namespace": map[string]any{"type": "string"},
					},
					"required": []any{"name", "namespace"},
				},
			},
			handler: restartDeployment,
		},
	}

	for _, entry := range tools {
		if err := registry.RegisterTool(entry.tool, entry.handler); err != nil {
			return err
		}
	}
	return nil
}

// fakePods is the cluster this harness pretends to have.
var fakePods = []map[string]any{
	{"name": "api-7d9f", "namespace": "default", "phase": "Running", "restarts": 0},
	{"name": "api-4c2a", "namespace": "default", "phase": "Running", "restarts": 2},
	{"name": "ingress-nginx-1b8e", "namespace": "kube-system", "phase": "Pending", "restarts": 0},
	{"name": "coredns-9a11", "namespace": "kube-system", "phase": "Running", "restarts": 0},
	{"name": "metrics-server-5f0c", "namespace": "kube-system", "phase": "CrashLoopBackOff", "restarts": 14},
}

func listPods(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Namespace string `json:"namespace"`
	}
	if err := decodeArgs(req, &args); err != nil {
		return toolError(err.Error()), nil
	}

	matched := []map[string]any{}
	for _, pod := range fakePods {
		if args.Namespace == "" || pod["namespace"] == args.Namespace {
			matched = append(matched, pod)
		}
	}
	if len(matched) == 0 {
		return toolError(fmt.Sprintf("no namespace %q", args.Namespace)), nil
	}
	return structured(map[string]any{"pods": matched})
}

func restartDeployment(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	if err := decodeArgs(req, &args); err != nil {
		return toolError(err.Error()), nil
	}
	if args.Name == "" || args.Namespace == "" {
		return toolError("both name and namespace are required"), nil
	}

	// What a mutating tool owes the log when it is interrupted. Returning the
	// context error here would be recorded as a definite failure, and a model
	// told the restart failed is a model that restarts it again. A real tool
	// would say this whenever it had already issued the change.
	if err := ctx.Err(); err != nil {
		return toolError(fmt.Sprintf(
			"stopped before this call reported (%v). Whether the restart took effect is not known.", err)), nil
	}

	return structured(map[string]any{
		"restarted": fmt.Sprintf("%s/%s", args.Namespace, args.Name),
		"note":      "nothing was restarted: this harness has no cluster",
	})
}

// decodeArgs unmarshals a call's arguments. Absent arguments are an empty
// object rather than an error, since a tool with no required fields can be
// called with none.
func decodeArgs(req *mcp.CallToolRequest, into any) error {
	if len(req.Params.Arguments) == 0 {
		return nil
	}
	if err := json.Unmarshal(req.Params.Arguments, into); err != nil {
		return fmt.Errorf("could not read the arguments: %v", err)
	}
	return nil
}

// toolError reports a failure the model can correct, which is a result with
// IsError set rather than a Go error. See ToolRegistry.Invoke.
func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: message}},
	}
}

func structured(value map[string]any) (*mcp.CallToolResult, error) {
	text, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{
		StructuredContent: value,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, nil
}
