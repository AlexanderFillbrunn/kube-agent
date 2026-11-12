package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"fillbrunn.ai/kube-agent/pkg/history"
)

// A work item that cannot say who works the arc is refused before anything is
// taken or read: there is no default agent to fall back on.
func TestWorkRefusesAnItemNamingNoAgent(t *testing.T) {
	agent, err := NewAgent(nil, nil, &scriptedLLM{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	lease := NewLocalLease()
	worker, err := NewArcWorker(&logStore{}, &logStore{}, NewFixedResolver(agent), lease)
	if err != nil {
		t.Fatalf("NewArcWorker: %v", err)
	}

	if _, err := worker.Work(context.Background(), testRef(), ""); err == nil {
		t.Error("a work item naming no agent was accepted")
	}
	// The refusal came before the lease, so the arc is free to whoever names
	// an agent.
	if release, ok, err := lease.Acquire(context.Background(), testRef()); err != nil || !ok {
		t.Errorf("the refused item left the arc held: ok=%v err=%v", ok, err)
	} else {
		release()
	}
}

// Everything a run writes carries the agent the work item named, which is the
// whole of how the log learns who added what: the agent itself never knows.
func TestARunStampsItsEventsWithTheWorkItemsAgent(t *testing.T) {
	store := runningTaskStore(t)
	agent, err := NewAgent(nil, nil, &scriptedLLM{replies: []*LLMResponse{{ModelID: "gpt-5", Content: "done"}}})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	worker, err := NewArcWorker(store, store, NewFixedResolver(agent), nil)
	if err != nil {
		t.Fatalf("NewArcWorker: %v", err)
	}

	if _, err := worker.Work(context.Background(), testRef(), testAgentID); err != nil {
		t.Fatalf("Work: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.events) == 0 {
		t.Fatal("the run wrote nothing")
	}
	for _, ev := range store.events {
		if ev.AgentID != testAgentID {
			t.Errorf("event %s carries agent %q, want %q", ev.ID, ev.AgentID, testAgentID)
		}
	}
}

// The resolver is asked only once the arc is held, so a resolver that cannot
// build the agent fails a worker that owns the arc, not one queued behind
// somebody else.
type refusingResolver struct{ err error }

func (r refusingResolver) ResolveAgent(context.Context, string) (*Agent, error) {
	return nil, r.err
}

func TestAnUnresolvableAgentFailsTheWork(t *testing.T) {
	worker, err := NewArcWorker(&logStore{}, &logStore{}, refusingResolver{err: errors.New("no such definition")}, nil)
	if err != nil {
		t.Fatalf("NewArcWorker: %v", err)
	}
	if _, err := worker.Work(context.Background(), testRef(), testAgentID); err == nil {
		t.Error("a work item naming an unresolvable agent was accepted")
	}
}

// The file resolver answers every id with the same agent, and reads the file
// again only once it changed.
func TestFileResolverIgnoresTheAgentIDAndRereadsOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, []byte(settingsJSON("first", "gpt-5")), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	resolver := NewFileResolver(path, nil, scriptedModels)

	first, err := resolver.ResolveAgent(context.Background(), "anybody")
	if err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if first.config.SystemPrompt != "first" {
		t.Errorf("prompt = %q, want the one in the file", first.config.SystemPrompt)
	}

	// The same file answers with the same agent, whatever the id.
	again, err := resolver.ResolveAgent(context.Background(), "somebody else")
	if err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if again != first {
		t.Error("an unchanged file built a second agent")
	}

	// A changed file builds a fresh agent on the next arc, asking the model it
	// names rather than one the process chose.
	later := time.Now().Add(time.Hour)
	if err := os.WriteFile(path, []byte(settingsJSON("second", "gpt-5-mini")), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("touch settings: %v", err)
	}
	fresh, err := resolver.ResolveAgent(context.Background(), "anybody")
	if err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if fresh == first || fresh.config.SystemPrompt != "second" {
		t.Errorf("a changed file resolved to %+v, want a fresh agent on the new prompt", fresh.config)
	}
	if fresh.llm == first.llm {
		t.Error("the model did not change with the file")
	}
	if name := fresh.llm.(*namedLLM).name; name != "gpt-5-mini" {
		t.Errorf("the factory built %q, want the model the file names", name)
	}
}

// namedLLM is the model a definition named, remembered so a test can check
// that the ref travelled: the factory is where a settings file says gpt-5 and
// a client says it back.
type namedLLM struct{ name string }

func (l *namedLLM) StreamResponse(context.Context, *history.History, *LLMOptions, Sink[RawToken]) (*LLMResponse, error) {
	return nil, fmt.Errorf("the named model %s was asked for a reply", l.name)
}

func (l *namedLLM) Response(context.Context, *history.History, *LLMOptions) (*LLMResponse, error) {
	return nil, fmt.Errorf("the named model %s was asked for a reply", l.name)
}

// The model a definition names is the model the agent asks, through the
// factory the process supplied: the ref travels, the credentials do not.
var scriptedModels = map[string]ModelFactory{
	"test": func(ref ModelRef) (LLM, error) {
		return &namedLLM{name: ref.Name}, nil
	},
}

func settingsJSON(prompt, model string) string {
	return fmt.Sprintf(`{"system_prompt":%q,"model":{"source":"test","name":%q}}`, prompt, model)
}

func TestFileResolverFailsOnAMissingFile(t *testing.T) {
	resolver := NewFileResolver(filepath.Join(t.TempDir(), "absent.json"), nil, scriptedModels)
	if _, err := resolver.ResolveAgent(context.Background(), testAgentID); err == nil {
		t.Error("a settings file that is not there resolved")
	}
}

// A settings file naming no model is not an agent, since there is nothing to
// ask.
func TestFileResolverRefusesSettingsNamingNoModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, []byte(`{"system_prompt":"a prompt"}`), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	resolver := NewFileResolver(path, nil, scriptedModels)
	if _, err := resolver.ResolveAgent(context.Background(), testAgentID); err == nil {
		t.Error("settings naming no model resolved")
	}
}

// The tools a definition names are the tools the agent offers, in the order
// they are named, each answered by the source that holds it.
func TestSelectToolsOffersTheNamedToolsFromTheirSources(t *testing.T) {
	first, second := NewToolRegistry(), NewToolRegistry()
	if err := first.RegisterTool(&mcp.Tool{Name: "list_pods"}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := second.RegisterTool(&mcp.Tool{Name: "restart_deployment"}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	sources := map[string]ToolProvider{"first": first, "second": second}

	selected, err := SelectTools([]ToolRef{
		{Source: "second", Name: "restart_deployment"},
		{Source: "first", Name: "list_pods"},
	}, sources)
	if err != nil {
		t.Fatalf("SelectTools: %v", err)
	}
	if got := toolNames(selected.All()); !slices.Equal(got, []string{"restart_deployment", "list_pods"}) {
		t.Errorf("offered %v, want the named tools in the named order", got)
	}

	// A tool the definition did not name is not offered and not callable.
	if _, ok := selected.Get("wait"); ok {
		t.Error("a tool the definition did not name is offered")
	}
	if _, err := selected.Invoke(context.Background(), "wait", nil); err == nil {
		t.Error("a call for an unselected tool was not refused")
	}
}

func TestSelectToolsRefusesWhatItCannotResolve(t *testing.T) {
	held := NewToolRegistry()
	if err := held.RegisterTool(&mcp.Tool{Name: "list_pods"}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	sources := map[string]ToolProvider{"local": held}

	for name, refs := range map[string][]ToolRef{
		"an unknown source":  {{Source: "nowhere", Name: "list_pods"}},
		"an unknown tool":    {{Source: "local", Name: "wait"}},
		"a tool named twice": {{Source: "local", Name: "list_pods"}, {Source: "local", Name: "list_pods"}},
	} {
		if _, err := SelectTools(refs, sources); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A settings file naming a model source the process does not hold is refused,
// like a tool it does not hold: a definition cannot know what the process can
// reach, and the model it names has to be askable.
func TestFileResolverRefusesAnUnknownModelSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	settings := `{"system_prompt":"a prompt","model":{"source":"nowhere","name":"gpt-5"}}`
	if err := os.WriteFile(path, []byte(settings), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	resolver := NewFileResolver(path, nil, scriptedModels)
	if _, err := resolver.ResolveAgent(context.Background(), testAgentID); err == nil {
		t.Error("settings naming a model source nothing holds resolved")
	}
}
