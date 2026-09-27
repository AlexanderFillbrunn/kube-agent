// Command devchat is a terminal harness for driving the agent by hand.
//
// It is a development tool, not the product: it talks to one model over one
// file-based log, offers tools that answer from canned data and touch no
// cluster, and keeps every session in a file you can read. Nothing here is
// built to be deployed, and the storage it leans on lives under internal/ so
// it cannot be mistaken for the real thing.
//
//	export OPENAI_API_KEY=...
//	go run ./cmd/devchat
//
// Type a message and the agent runs until it is your turn again, with the
// reply arriving as it is written and each tool call showing what it is doing.
// Enter sends and esc leaves. A run in progress is stopped by ctrl+c or by
// /cancel, which takes a reason the model is told about; either way the arc is
// withdrawn in the log rather than merely dropped on the screen.
//
// Sessions are kept under .devchat/ rather than beside the source. Pointing
// -store at a log again resumes that conversation, and /clear moves the
// current one aside and starts another.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"fillbrunn.ai/kube-agent/internal/filestore"
	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/users"
)

// defaultStore keeps sessions in a directory of their own. They are working
// files of a development tool, and they hold whole conversations, so they do
// not belong next to the source.
const defaultStore = ".devchat/session.jsonl"

const defaultSystemPrompt = `You are a Kubernetes operations assistant with read and restart tools.
Use them rather than guessing, and say plainly when a tool tells you something you did not expect.`

func main() {
	store := flag.String("store", defaultStore, "event log to append to, resuming it if it exists")
	model := flag.String("model", "gpt-5", "model to ask")
	baseURL := flag.String("base-url", "", "an OpenAI-compatible endpoint to use instead of OpenAI's")
	system := flag.String("system", defaultSystemPrompt, "system prompt")
	user := flag.String("user", "dev", "who the messages are from")
	flag.Parse()

	if err := run(*store, *model, *baseURL, *system, *user); err != nil {
		fmt.Fprintf(os.Stderr, "devchat: %v\n", err)
		os.Exit(1)
	}
}

func run(storePath, modelName, baseURL, systemPrompt, userID string) error {
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

	opts := []agent.OpenAIOption{}
	if baseURL != "" {
		opts = append(opts, agent.WithBaseURL(baseURL))
	}
	llm := agent.NewOpenAI(apiKey, modelName, opts...)

	// The sinks report into the interface, and the interface cannot exist
	// until the model is built, so they go through a sender that is pointed at
	// the program once there is one.
	relay := &sender{}
	chat, err := agent.NewAgent(
		&agent.AgentConfig{
			SystemPrompt: systemPrompt,
			Tokens: agent.SinkFromFunc(func(token string) error {
				relay.send(tokenMsg(token))
				return nil
			}),
			Observer: agent.SinkFromFunc(func(ev *events.Event) error {
				relay.send(eventMsg{event: ev})
				return nil
			}),
		},
		registry,
		// The guard turns a cancel recorded in the log into a run that stops,
		// which is what lets a cancel from anywhere but this process take
		// effect. Here that is only ever this process, so it earns its keep as
		// the wiring rather than the feature.
		llm,
		agent.WithTasks(tasks, runner),
	)
	if err != nil {
		return err
	}

	persistor := agent.StampIdentity(agent.GuardCancellation(store, store), storePath)
	session := &session{
		id:         storePath,
		store:      store,
		persistor:  persistor,
		agent:      chat,
		actor:      users.Actor{UserID: userID, TenantID: "dev"},
		controller: runner,
	}

	// The inbound half. A task calls this rather than writing to the log, which
	// is the path a job somewhere else would have to take, so the harness
	// exercises it rather than going around it.
	reports, err := agent.NewTaskReports(storePath, persistor, store, chat, agent.NewLocalLease())
	if err != nil {
		return err
	}
	runner.attach(reports,
		func(ref agent.TaskRef, text string, state toolState) {
			relay.send(taskMsg{ref: ref, text: text, state: state})
		},
		func(arcID string) { relay.send(taskOwedMsg{arcID: arcID}) },
	)

	// The conversation already on disk is shown before anything is asked, so
	// a resumed session opens where it left off.
	recorded, err := session.fullHistory(context.Background())
	if err != nil {
		return err
	}

	// A task in that log is one this process started and then died on, because
	// the only runner here is this one. Left alone it would hold its arc open
	// for ever and put a note about work in flight in front of the model on
	// every turn from now on.
	settled, err := agent.SettleAbandonedTasks(context.Background(), persistor, recorded)
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

	program := tea.NewProgram(newModel(session, relay, llm, existing, reports))
	relay.attach(program)

	_, err = program.Run()
	return err
}
