package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolSet is the list of tools a request offers the model, over however many
// sources those tools come from.
type ToolSet interface {
	// All returns every tool, in a fixed order, so that two requests built
	// from the same sources carry the same prefix.
	All() []*mcp.Tool
}

// CombineTools presents several sources as one. A nil source contributes
// nothing.
func CombineTools(sets ...ToolSet) ToolSet { return combinedTools(sets) }

type combinedTools []ToolSet

func (c combinedTools) All() []*mcp.Tool {
	var tools []*mcp.Tool
	for _, set := range c {
		if set == nil {
			continue
		}
		tools = append(tools, set.All()...)
	}
	return tools
}

type ToolRegistry struct {
	tools map[string]registeredTool
}

// ToolProvider is one source of tools: what the model is offered from it, and
// how a call to one of those tools is run. A provider answers from its own
// tools only, so several can be combined without one seeing another's.
//
// Which tools are safe to repeat is not here: it is written in the tool's own
// annotations, so it is read by SafeToRepeat rather than asked of a source.
type ToolProvider interface {
	ToolSet

	// Get returns one tool by name, for a caller that has to know how a call
	// by that name would be made.
	Get(tool string) (*mcp.Tool, bool)

	// Invoke runs a tool and returns what it produced. See ToolRegistry.Invoke
	// for what the returned error means.
	Invoke(ctx context.Context, name string, args any) (ToolOutcome, error)
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

// All returns every registered tool, ordered by name so that two requests
// built from the same registry carry the same prefix.
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
// is as good as running it once. It answers from the tool's own annotations,
// so a tool that declares neither hint is not safe to repeat.
func SafeToRepeat(tool *mcp.Tool) bool {
	if tool == nil || tool.Annotations == nil {
		return false
	}
	return tool.Annotations.ReadOnlyHint || tool.Annotations.IdempotentHint
}

// ToolOutcome is what a tool produced, in the terms the log records it in.
type ToolOutcome struct {
	Result  any
	IsError bool
}

// Invoke runs a registered tool and returns what it produced.
//
// The returned error means the call could not be made or the tool broke. A
// tool reporting its own failure returns an outcome with IsError set instead,
// which is the distinction MCP draws.
//
// A panic is not recovered. It takes the worker with it and leaves the call
// dispatched with no result, which is what the log says about work nobody can
// account for.
func (r *ToolRegistry) Invoke(ctx context.Context, name string, args any) (ToolOutcome, error) {
	registered, ok := r.tools[name]
	if !ok {
		return ToolOutcome{}, fmt.Errorf("tool not registered: %s", name)
	}

	// The log holds arguments as decoded JSON and a handler takes them raw.
	raw, err := json.Marshal(args)
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("marshal arguments for %s: %w", name, err)
	}

	// These tools run in this process, so the request carries no session.
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

// resultValue reduces a tool result to the one value the log keeps, preferring
// structured output where a tool produced it.
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
		// Non-text content is kept as it came.
		return result.Content
	}
	return nil
}
