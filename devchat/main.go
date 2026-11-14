// Command devchat is a terminal harness for driving the agent by hand.
//
// It is a development tool, not the product. It talks to one model over one
// file-based log and offers tools that answer from canned data.
//
//	export OPENAI_API_KEY=...
//	cd devchat && go run .
//
// Type a message and the agent runs until it is your turn again. Enter sends
// and esc leaves. A run in progress is stopped by ctrl+c or by /cancel, which
// withdraws the arc in the log rather than dropping it on the screen.
//
// Sessions are kept under .devchat/. Pointing -store at a log again resumes
// that conversation, and /clear moves the current one aside.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"

	"fillbrunn.ai/kube-agent/devchat/filestore"
	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/users"
)

// defaultStore keeps sessions in a directory of their own. They are working
// files of a development tool, and they hold whole conversations, so they do
// not belong next to the source.
const defaultStore = ".devchat/session.jsonl"

// defaultSettings is the agent settings file the resolver reads. It is a
// working file of a development tool, so it lives with the sessions.
const defaultSettings = ".devchat/agents.json"

// localAgentID names the one agent this harness runs. The resolver ignores
// it, but every work item and every event carries it, so the plumbing is
// exercised exactly as a deployed tier would exercise it.
const localAgentID = "local"

const defaultSystemPrompt = `You are a Kubernetes operations assistant with read and restart tools.
Use them rather than guessing, and say plainly when a tool tells you something you did not expect.`

func main() {
	store := flag.String("store", defaultStore, "event log to append to, resuming it if it exists")
	settings := flag.String("settings", defaultSettings, "agent settings the resolver reads, created if absent")
	user := flag.String("user", "dev", "who the messages are from")
	flag.Parse()

	if err := run(*store, *settings, *user); err != nil {
		fmt.Fprintf(os.Stderr, "devchat: %v\n", err)
		os.Exit(1)
	}
}

func run(storePath, settingsPath, userID string) error {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENAI_API_KEY is not set")
	}

	store, err := filestore.Open(storePath)
	if err != nil {
		return err
	}
	defer store.Close()

	registry := agent.NewToolRegistry()
	if err := registerFakeTools(registry); err != nil {
		return err
	}

	tasks := agent.NewTaskRegistry()
	runner := newLocalTasks()
	if err := runner.register(tasks); err != nil {
		return err
	}

	// The model sources hold what the settings file cannot: which endpoints
	// exist and how to reach them. "openai" is the vendor's own, and "local"
	// is any OpenAI-compatible endpoint this process is pointed at, so a
	// definition asks a source and never says where the source lives.
	openai := func(ref agent.ModelRef) (agent.LLM, error) {
		return agent.NewOpenAI(apiKey, ref.Name), nil
	}
	modelSources := map[string]agent.ModelFactory{"openai": openai}
	if baseURL := os.Getenv("OPENAI_BASE_URL"); baseURL != "" {
		modelSources["local"] = func(ref agent.ModelRef) (agent.LLM, error) {
			return agent.NewOpenAI(apiKey, ref.Name, agent.WithBaseURL(baseURL)), nil
		}
	}

	// The settings file is what the resolver reads, so the prompt, the model
	// and the tools are edited between arcs rather than passed once at
	// startup. A missing file is the first run, and it gets the defaults.
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		if err := writeDefaultSettings(settingsPath); err != nil {
			return err
		}
	}
	settings, err := readSettings(settingsPath)
	if err != nil {
		return err
	}

	// "local" is the one tool source this harness holds: the fake tools
	// compiled into it. A settings file may name no other.
	sources := map[string]agent.ToolProvider{"local": registry}
	resolver := agent.NewFileResolver(settingsPath, sources, modelSources, agent.WithTasks(tasks, runner))

	// The sinks report into the interface, and the interface cannot exist
	// until the model is built, so they go through a sender that is pointed at
	// the program once there is one.
	relay := &sender{}

	// Notification hangs off the persistor rather than the agent, so
	// everything written reaches the screen: the user's own message and a
	// cancel as much as what a run records. The guard turns a cancel recorded
	// in the log into a run that stops, wherever the cancel was recorded.
	persistor := agent.PublishEvents(
		agent.MintIdentity(agent.GuardCancellation(store, store)),
		screenEvents{relay: relay},
	)

	// One worker for the process, so that a person's turn, a task's report
	// and anything else that enters an arc contend for the same lease. The
	// session arrives with the arc rather than being built in.
	//
	// The cancels go through the same halves a deployed one would: the session
	// announces, the worker watches. In one process they could have been the
	// run's own CancelFunc, but then the harness would exercise a path nothing
	// else has.
	cancels := agent.NewLocalCancels()
	worker, err := agent.NewArcWorker(persistor, store, resolver, agent.NewLocalLease(),
		agent.WithTokenSink(screenTokens{relay: relay}),
		agent.WithCancelWatcher(cancels))
	if err != nil {
		return err
	}

	session := &session{
		id:        storePath,
		store:     store,
		persistor: persistor,
		worker:    worker,
		cancels:   cancels,
		actor:     users.Actor{UserID: userID, TenantID: "dev"},
	}

	// The inbound half. A task calls this rather than writing to the log, which
	// is the path a job somewhere else would have to take, so the harness
	// exercises it rather than going around it.
	reports, err := agent.NewTaskReports(worker)
	if err != nil {
		return err
	}
	runner.attach(reports, localAgentID,
		func(ref agent.TaskRef, err error) { relay.send(taskUnfiledMsg{ref: ref, err: err}) },
		func(arcID string) { relay.send(taskOwedMsg{arcID: arcID}) },
	)

	// The conversation already on disk is shown before anything is asked, so
	// a resumed session opens where it left off.
	recorded, err := session.fullHistory(context.Background())
	if err != nil {
		return err
	}

	// The only runner here is this process, so a task in that log is one an
	// earlier process died on. Left alone it would hold its arc open for
	// ever.
	settled, err := agent.SettleAbandonedTasks(context.Background(), agent.AsAgent(localAgentID, persistor), recorded)
	if err != nil {
		return err
	}
	if settled > 0 {
		if recorded, err = session.fullHistory(context.Background()); err != nil {
			return err
		}
	}

	existing := newTranscript()
	existing.fromHistory(recorded)

	program := tea.NewProgram(newModel(session, relay, settings, settingsPath, existing, worker))
	relay.attach(program)

	_, err = program.Run()
	return err
}

// writeDefaultSettings leaves a settings file for the resolver to read, so a
// first run has an agent and the prompt, the model and the tools are there to
// be edited. The tools are the fake ones this harness registers, since a
// definition can only pick what its process holds.
func writeDefaultSettings(path string) error {
	settings := agent.AgentSettings{
		SystemPrompt: defaultSystemPrompt,
		Model:        agent.ModelRef{Source: "openai", Name: "gpt-5"},
		Tools: []agent.ToolRef{
			{Source: "local", Name: "list_pods"},
			{Source: "local", Name: "wait"},
			{Source: "local", Name: "restart_deployment"},
		},
	}
	return writeSettings(path, settings)
}

// readSettings parses the settings file, for the interface as much as the
// resolver: the header shows the model the definition names, not one the
// process holds a pointer to.
func readSettings(path string) (agent.AgentSettings, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return agent.AgentSettings{}, err
	}
	var settings agent.AgentSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return agent.AgentSettings{}, fmt.Errorf("parse agent settings %s: %w", path, err)
	}
	return settings, nil
}

// writeSettings puts the settings back, which is how /model changes the model:
// the file is the definition, the resolver rereads it on the next arc, and one
// run never changes the model another arc is already asking.
func writeSettings(path string, settings agent.AgentSettings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}
