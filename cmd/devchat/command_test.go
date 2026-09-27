package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fillbrunn.ai/kube-agent/internal/filestore"
	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/users"
)

func modelOver(t *testing.T, store *filestore.Store) model {
	t.Helper()
	s := &session{id: "session", store: store, actor: users.Actor{UserID: "dev"}}
	llm := agent.NewOpenAI("test-key", "gpt-5")
	return newModel(s, &sender{}, llm, newTranscript(), nil).resize(80, 24)
}

// Clearing has to reach the log, or the next turn replays a conversation the
// screen no longer shows and the model answers questions nobody can see.
func TestClearStartsANewLogAndKeepsTheOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	store, err := filestore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	arc := "arc-1"
	actor := users.Actor{UserID: "dev", TenantID: "dev"}
	if _, err := store.Persist(context.Background(), &events.Event{
		SessionID: "session", Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: actor,
		Payload: events.UserMessagePayload{Content: "what is pending?", Actor: actor},
	}); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	cleared, _ := clearSession(modelOver(t, store), "")

	stored, err := store.Events(context.Background())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("the new log holds %d events, want none", len(stored))
	}

	// The next event starts the numbering over, which is what the fold of a
	// fresh history expects.
	next, err := store.Persist(context.Background(), &events.Event{
		SessionID: "session", Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: actor,
		Payload: events.UserMessagePayload{Content: "again", Actor: actor},
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if next.Seq != 0 {
		t.Errorf("the new log starts at seq %d, want 0", next.Seq)
	}

	archived, err := filepath.Glob(path + ".*")
	if err != nil || len(archived) != 1 {
		t.Fatalf("archived logs = %v (%v), want exactly one", archived, err)
	}
	kept, err := os.ReadFile(archived[0])
	if err != nil || !strings.Contains(string(kept), "what is pending?") {
		t.Errorf("the archived log does not hold the conversation: %v", err)
	}

	if len(cleared.transcript.blocks) != 1 || cleared.transcript.blocks[0].kind != blockNote {
		t.Errorf("after clearing the screen shows %+v, want one note", cleared.transcript.blocks)
	}
}

// Clearing an untouched session leaves no empty file behind for every attempt.
func TestClearingAnEmptySessionArchivesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	store, err := filestore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	clearSession(modelOver(t, store), "")

	archived, err := filepath.Glob(path + ".*")
	if err != nil || len(archived) != 0 {
		t.Errorf("archived %v, want nothing", archived)
	}
}

func TestQuitAsksTheProgramToStop(t *testing.T) {
	store, err := filestore.Open(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	_, command := runCommand(modelOver(t, store), "/quit")
	if command == nil {
		t.Fatal("/quit returned no command")
	}
}

// A mistyped command is answered rather than sent to the model.
func TestAnUnknownCommandIsAnswered(t *testing.T) {
	store, err := filestore.Open(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	answered, command := runCommand(modelOver(t, store), "/histroy")
	if command != nil {
		t.Error("an unknown command did something")
	}
	if len(answered.transcript.blocks) != 1 || answered.transcript.blocks[0].kind != blockNote {
		t.Fatalf("screen shows %+v, want one note", answered.transcript.blocks)
	}
	if !strings.Contains(answered.transcript.blocks[0].text, "/clear") {
		t.Errorf("the note does not say what is available: %q", answered.transcript.blocks[0].text)
	}
}

func TestOnlySlashLinesAreCommands(t *testing.T) {
	if !isCommand("/clear") {
		t.Error("/clear is not recognised")
	}
	if isCommand("what is pending?") {
		t.Error("a question was taken for a command")
	}
}

// Switching points the rest of the session at another model. The header reads
// the client, so what is shown cannot drift from what is asked.
func TestModelSwitches(t *testing.T) {
	store, err := filestore.Open(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	switched, command := runCommand(modelOver(t, store), "/model gpt-4.1")
	if command != nil {
		t.Error("switching a model did something else as well")
	}
	if switched.llm.Model != "gpt-4.1" {
		t.Errorf("client asks for %q, want gpt-4.1", switched.llm.Model)
	}
	if note := switched.transcript.blocks[0]; note.kind != blockNote || !strings.Contains(note.text, "gpt-5") {
		t.Errorf("note = %+v, want it to say what was switched from", note)
	}
	if !strings.Contains(switched.View().Content, "gpt-4.1") {
		t.Error("the header still shows the old model")
	}
}

// With no name it reports rather than changing anything, since there is no
// sane model to fall back to.
func TestModelWithNoNameReports(t *testing.T) {
	store, err := filestore.Open(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	reported, _ := runCommand(modelOver(t, store), "/model")
	if reported.llm.Model != "gpt-5" {
		t.Errorf("client asks for %q, want it unchanged", reported.llm.Model)
	}
	if note := reported.transcript.blocks[0]; !strings.Contains(note.text, "gpt-5") {
		t.Errorf("note = %q, want the model it is using", note.text)
	}
}

// The conversation carries over a switch, because the log is what the next
// turn is built from and nothing about it belongs to a model.
func TestSwitchingKeepsTheConversation(t *testing.T) {
	store, err := filestore.Open(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	before := modelOver(t, store)
	before.transcript.blocks = append(before.transcript.blocks,
		&block{kind: blockUser, author: "dev", text: "what is pending?"})

	after, _ := runCommand(before, "/model gpt-4.1")
	if len(after.transcript.blocks) != 2 || after.transcript.blocks[0].text != "what is pending?" {
		t.Errorf("transcript = %+v, want the question kept and a note added", after.transcript.blocks)
	}
}

// The help line is built from the table, so a command cannot go unlisted.
func TestHelpListsEveryCommand(t *testing.T) {
	help := commandHelp()
	for _, name := range commandNames() {
		if !strings.Contains(help, name) {
			t.Errorf("help %q does not mention %s", help, name)
		}
	}
	if !strings.Contains(help, "/model <name>") {
		t.Errorf("help %q does not say that /model takes a name", help)
	}
}
