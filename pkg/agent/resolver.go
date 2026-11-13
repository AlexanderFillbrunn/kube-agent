package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AgentResolver answers which agent works an arc. It is asked inside the lease
// and after the work item has named the agent, so a failure means this worker
// owned the arc and could not build the agent for it — never that somebody
// else holds the arc.
//
// What a resolver does between calls is its own business. One that reads a
// definition from somewhere changing decides for itself when a resolved agent
// is stale; the contract is only that one arc sees one consistent agent, since
// a run that changed model or tools halfway through would be two agents
// wearing one log.
type AgentResolver interface {
	// ResolveAgent returns the agent that works an arc. The id is the one the
	// work item named; an implementation that cannot tell its agents apart is
	// free to ignore it, and one that can must refuse an id it does not know
	// rather than guess.
	ResolveAgent(ctx context.Context, agentID string) (*Agent, error)
}

// FixedResolver answers every id with the same agent. It is the resolver a
// process with one agent already built has, and the one tests run under.
type FixedResolver struct {
	agent *Agent
}

// NewFixedResolver answers every id with the agent given.
func NewFixedResolver(agent *Agent) *FixedResolver {
	return &FixedResolver{agent: agent}
}

// ResolveAgent returns the agent, whatever the id.
func (f *FixedResolver) ResolveAgent(_ context.Context, _ string) (*Agent, error) {
	if f.agent == nil {
		return nil, errors.New("no agent was given to resolve")
	}
	return f.agent, nil
}

// ModelRef names the model an agent asks: which source serves it, and what
// the model is called there. A source groups the models one endpoint serves,
// so a local OpenAI-compatible API is a source of its own — "local", say —
// configured with its base URL by the process that can reach it. A definition
// says no more than source and name, since everything else about reaching a
// source belongs to the process doing the asking.
type ModelRef struct {
	Source string `json:"source"`
	Name   string `json:"name"`
}

// ToolRef names one tool a definition wants: the source that runs it, and the
// tool's name there. A source is a provider the process already holds, so a
// definition can pick its tools but never invent them.
type ToolRef struct {
	Source string `json:"source"`
	Name   string `json:"name"`
}

// AgentSettings is what a definition says an agent is: what the model is
// told, which model to ask, and which tools to offer. It holds only what a
// definition can say, which is why the tools are names and the model is a
// name: turning those into the code behind them is the resolver's half.
type AgentSettings struct {
	SystemPrompt string    `json:"system_prompt"`
	Model        ModelRef  `json:"model"`
	Tools        []ToolRef `json:"tools"`
}

// ModelFactory builds the client that asks one source's models. A process
// supplies one per source with the credentials and endpoints it holds; a
// definition supplies the ref.
type ModelFactory func(ref ModelRef) (LLM, error)

// FileResolver builds an agent from a settings file. It ignores the agent id:
// every id names the one agent the file describes, which is what local
// testing wants and nothing a deployed resolver would promise.
//
// The file is read again when it changes, and not before: the mod time is
// what the resolver remembers, so editing the prompt, the model or the tools
// between one arc and the next takes effect on the next arc without anything
// watching the file.
type FileResolver struct {
	path         string
	toolSources  map[string]ToolProvider
	modelSources map[string]ModelFactory
	options      []AgentOption

	mu      sync.Mutex
	modTime time.Time
	agent   *Agent
}

// NewFileResolver reads agent settings from the file at path, building its
// agent from the tool sources, the model sources and the options given, which
// are the parts a file cannot supply.
func NewFileResolver(path string, toolSources map[string]ToolProvider, modelSources map[string]ModelFactory, options ...AgentOption) *FileResolver {
	return &FileResolver{path: path, toolSources: toolSources, modelSources: modelSources, options: options}
}

// ResolveAgent returns the agent the settings file describes, reading it
// again only if it changed since the last read.
func (f *FileResolver) ResolveAgent(_ context.Context, _ string) (*Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	info, err := os.Stat(f.path)
	if err != nil {
		return nil, fmt.Errorf("read agent settings %s: %w", f.path, err)
	}
	if f.agent != nil && !info.ModTime().After(f.modTime) {
		return f.agent, nil
	}

	raw, err := os.ReadFile(f.path)
	if err != nil {
		return nil, fmt.Errorf("read agent settings %s: %w", f.path, err)
	}
	var settings AgentSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, fmt.Errorf("parse agent settings %s: %w", f.path, err)
	}

	agent, err := f.build(settings)
	if err != nil {
		return nil, fmt.Errorf("the agent in %s does not build: %w", f.path, err)
	}
	f.modTime = info.ModTime()
	f.agent = agent
	return agent, nil
}

// build turns what the file says into the agent that runs it.
func (f *FileResolver) build(settings AgentSettings) (*Agent, error) {
	if settings.Model.Name == "" || settings.Model.Source == "" {
		return nil, errors.New("the settings name no model to ask")
	}
	tools, err := SelectTools(settings.Tools, f.toolSources)
	if err != nil {
		return nil, err
	}
	factory, ok := f.modelSources[settings.Model.Source]
	if !ok {
		return nil, fmt.Errorf("no model source %s is held by this process", settings.Model.Source)
	}
	llm, err := factory(settings.Model)
	if err != nil {
		return nil, err
	}
	return NewAgent(&AgentConfig{SystemPrompt: settings.SystemPrompt}, tools, llm, f.options...)
}

// SelectTools turns the tools a definition names into the provider that
// offers exactly those, in the order they are named, each run by the source
// that holds it.
//
// A ref naming a source the process does not hold, or a tool its source does
// not, is refused rather than skipped: a definition that silently offered
// fewer tools than it asked for would have the model plan around tools that
// do not exist. The same goes for a tool named twice, since the model picks
// by name and the agent routes by name, so one name must mean one tool.
func SelectTools(refs []ToolRef, sources map[string]ToolProvider) (ToolProvider, error) {
	selected := selectedTools{order: []string{}, byName: map[string]ToolProvider{}}
	for _, ref := range refs {
		source, ok := sources[ref.Source]
		if !ok {
			return nil, fmt.Errorf("no tool source %s is held by this process", ref.Source)
		}
		if _, ok := source.Get(ref.Name); !ok {
			return nil, fmt.Errorf("tool source %s holds no tool %s", ref.Source, ref.Name)
		}
		if _, taken := selected.byName[ref.Name]; taken {
			return nil, fmt.Errorf("tool %s is named twice", ref.Name)
		}
		selected.byName[ref.Name] = source
		selected.order = append(selected.order, ref.Name)
	}
	return selected, nil
}

// selectedTools offers the tools a definition named, each answered by the
// source that holds it.
type selectedTools struct {
	order  []string
	byName map[string]ToolProvider
}

func (s selectedTools) All() []*mcp.Tool {
	tools := make([]*mcp.Tool, 0, len(s.order))
	for _, name := range s.order {
		if tool, ok := s.byName[name].Get(name); ok {
			tools = append(tools, tool)
		}
	}
	return tools
}

func (s selectedTools) Get(tool string) (*mcp.Tool, bool) {
	source, ok := s.byName[tool]
	if !ok {
		return nil, false
	}
	return source.Get(tool)
}

// Invoke runs a selected tool on the source that holds it. A tool the
// definition did not name is not offered to the model, so a call for one is
// refused here rather than reaching a source the definition said nothing
// about.
func (s selectedTools) Invoke(ctx context.Context, name string, args any) (ToolOutcome, error) {
	source, ok := s.byName[name]
	if !ok {
		return ToolOutcome{}, fmt.Errorf("tool not selected: %s", name)
	}
	return source.Invoke(ctx, name, args)
}
