package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolRegistry struct {
	tools map[string]registeredTool
}

type registeredTool struct {
	tool    *mcp.Tool
	handler mcp.ToolHandler
}

// NewToolRegistry returns an empty registry ready to take tools.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: map[string]registeredTool{}}
}

func (r *ToolRegistry) RegisterTool(tool *mcp.Tool, handler mcp.ToolHandler) error {
	_, nok := r.tools[tool.Name]
	if nok {
		return fmt.Errorf("a tool with name %s has already been registered", tool.Name)
	}

	r.tools[tool.Name] = registeredTool{
		tool:    tool,
		handler: handler,
	}

	return nil
}

func (r *ToolRegistry) Get(tool string) (*mcp.Tool, bool) {
	t, ok := r.tools[tool]
	return t.tool, ok
}

// All returns every registered tool by name. The order is fixed so that two
// requests built from the same registry carry the same prefix, which is what
// keeps a provider's prompt cache usable across a session.
func (r *ToolRegistry) All() []*mcp.Tool {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	slices.Sort(names)

	tools := make([]*mcp.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, r.tools[name].tool)
	}
	return tools
}

// ToolOutcome is what a tool produced, in the terms the log records it in.
type ToolOutcome struct {
	Result  any
	IsError bool
}

// Invoke runs a registered tool and returns what it produced.
//
// Dispatch lives here rather than behind a handler callers fetch, so that the
// MCP calling convention stays in the one place that knows it and every call
// goes through the same point. Argument validation, a guardrail or a timeout
// then has somewhere to sit that a caller cannot route around.
//
// The returned error means the call could not be made or the tool broke. A
// tool reporting its own failure returns an outcome with IsError set, which is
// the distinction MCP draws and the one the model needs: the second is
// something it can correct, the first is not its doing.
func (r *ToolRegistry) Invoke(ctx context.Context, name string, args any) (ToolOutcome, error) {
	registered, ok := r.tools[name]
	if !ok {
		return ToolOutcome{}, fmt.Errorf("tool not registered: %s", name)
	}

	// The log holds arguments as decoded JSON and a handler takes them raw, so
	// they go back the way they came.
	raw, err := json.Marshal(args)
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("marshal arguments for %s: %w", name, err)
	}

	// The request carries no session: these tools run in this process rather
	// than over an MCP connection.
	result, err := registered.handler(ctx, &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: name, Arguments: raw},
	})
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("call %s: %w", name, err)
	}
	if result == nil {
		return ToolOutcome{}, fmt.Errorf("tool %s returned no result", name)
	}

	return ToolOutcome{Result: resultValue(result), IsError: result.IsError}, nil
}

// resultValue reduces a tool result to the one value the log keeps. Structured
// output is preferred where a tool produced it, since it survives the round
// trip as data rather than as prose about data.
func resultValue(result *mcp.CallToolResult) any {
	if result.StructuredContent != nil {
		return result.StructuredContent
	}

	var text []string
	for _, content := range result.Content {
		if textual, ok := content.(*mcp.TextContent); ok {
			text = append(text, textual.Text)
		}
	}
	if len(text) > 0 {
		return strings.Join(text, "\n")
	}
	if len(result.Content) > 0 {
		// Whatever a tool returned that is not text is kept as it came, rather
		// than described.
		return result.Content
	}
	return nil
}
