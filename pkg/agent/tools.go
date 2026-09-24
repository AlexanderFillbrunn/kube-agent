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

// SafeToRepeat reports whether running a tool twice with the same arguments
// is as good as running it once.
//
// It answers from the tool's own annotations, and an undeclared tool is not
// safe: both hints default to false, which is the right way round. A tool that
// forgot to say is treated as one that changes something.
//
// This is a declaration, not an observation. It says what the tool's author
// promised, which is all anyone can know without running it.
func (r *ToolRegistry) SafeToRepeat(name string) bool {
	tool, ok := r.tools[name]
	if !ok || tool.tool.Annotations == nil {
		return false
	}
	return tool.tool.Annotations.ReadOnlyHint || tool.tool.Annotations.IdempotentHint
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
//
// Either way the handler returned, and a handler that returns has said what
// happened: a tool call is synchronous, so there is no outcome it can still be
// waiting on. Work that cannot be accounted for by the time it returns belongs
// in an async task instead. A tool that may have changed something before
// giving up says so in its result rather than returning an error, because an
// error is recorded as a definite failure.
//
// Whether a call may be made twice is a separate question, answered by
// SafeToRepeat from the tool's annotations, and it only arises for a call an
// earlier run had already dispatched.
//
// A panic is not recovered here on purpose. It is the one way a handler does
// not return, and turning it into a failed result would claim knowledge of
// something that stopped halfway. Left alone it takes the worker with it and
// the call stays dispatched with no result, which is what the log says about
// work nobody can account for.
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
