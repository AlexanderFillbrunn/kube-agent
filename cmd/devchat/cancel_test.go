package main

import (
	"context"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"

	"fillbrunn.ai/kube-agent/internal/filestore"
	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
)

// runningModel returns a model that believes a turn is in flight, over a
// session that writes the way the real one does. Nothing is actually running:
// what a cancel has to get right is the record it leaves and the context it
// stops, and both are here.
func runningModel(t *testing.T, arc string) (model, context.Context, *filestore.Store) {
	t.Helper()

	store, err := filestore.Open(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	actor := users.Actor{UserID: "dev", TenantID: "dev"}
	s := &session{
		id:        "session",
		store:     store,
		persistor: agent.StampIdentity(agent.GuardCancellation(store, store), "session"),
		actor:     actor,
	}

	// The arc has to be opened before it can be withdrawn: a cancel naming an
	// arc the fold never saw is an error, not a cancel.
	if _, err := s.persistor.Persist(context.Background(), &events.Event{
		Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: actor,
		Payload: events.UserMessagePayload{Content: "what is pending?", Actor: actor},
	}); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	m := newModel(s, &sender{}, agent.NewOpenAI("test-key", "gpt-5"), newTranscript(), nil).resize(80, 24)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.cancel = cancel
	m.arc = arc
	return m, ctx, store
}

// A cancel has to do both halves. The record is what the next turn reads, and
// the context is what stops a tool that is already running; either one alone
// leaves the log and the run disagreeing about whether the arc is over.
func TestCancelRecordsTheWithdrawalAndStopsTheRun(t *testing.T) {
	m, ctx, store := runningModel(t, "arc-1")

	m, _ = cancelTurn(m, "took too long")

	if ctx.Err() == nil {
		t.Error("the run was left going")
	}

	stored, err := store.Events(context.Background())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	h, err := history.Replay("session", values(stored))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(h.Arcs) != 1 {
		t.Fatalf("the log folds to %d arcs, want one", len(h.Arcs))
	}

	arc := h.Arcs[0]
	if got := arc.Status(); got != history.ArcStatusCancelled {
		t.Errorf("the arc folds as %s, want %s", got, history.ArcStatusCancelled)
	}
	if arc.Cancel == nil {
		t.Fatal("the fold recorded no cancel")
	}
	if arc.Cancel.Reason == nil || *arc.Cancel.Reason != "took too long" {
		t.Errorf("the cancel carries reason %v, want the one that was typed", arc.Cancel.Reason)
	}
	// Who withdrew the arc is the point of recording it here rather than
	// letting the agent write its own.
	if arc.Cancel.Actor == nil || arc.Cancel.Actor.UserID != "dev" {
		t.Errorf("the cancel names actor %v, want dev", arc.Cancel.Actor)
	}
}

// A reason is optional, and ctrl+c gives none.
func TestCancelWithoutAReasonRecordsNone(t *testing.T) {
	m, _, store := runningModel(t, "arc-1")

	cancelTurn(m, "")

	stored, err := store.Events(context.Background())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	h, err := history.Replay("session", values(stored))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if cancel := h.Arcs[0].Cancel; cancel == nil || cancel.Reason != nil {
		t.Errorf("the cancel carries %v, want no reason", cancel)
	}
}

// Cancelling nothing must not write a cancel. An arc withdrawn after it
// finished still folds, and would rewrite a completed exchange as a stopped
// one.
func TestCancelWithNothingRunningWritesNothing(t *testing.T) {
	m, _, store := runningModel(t, "arc-1")
	m.cancel = nil
	m.arc = ""

	m, _ = cancelTurn(m, "")

	stored, err := store.Events(context.Background())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, ev := range stored {
		if ev.Typ == events.CancelEventType {
			t.Fatal("a cancel was written with nothing to cancel")
		}
	}

	blocks := m.transcript.blocks
	if len(blocks) != 1 || blocks[0].kind != blockNote {
		t.Errorf("the screen shows %+v, want one note", blocks)
	}
}

func pressEnter(m model) (model, tea.Cmd) {
	next, command := m.key(tea.KeyPressMsg{Code: tea.KeyEnter})
	return next.(model), command
}

// The whole point of /cancel is that enter reaches it mid-run, which it did
// not before.
func TestCancelCanBeSentWhileTheAgentIsWorking(t *testing.T) {
	m, ctx, _ := runningModel(t, "arc-1")
	m.input.SetValue("/cancel enough")

	m, _ = pressEnter(m)

	if ctx.Err() == nil {
		t.Error("enter did not reach /cancel during a run")
	}
	if got := m.input.Value(); got != "" {
		t.Errorf("the input still holds %q, want it sent and cleared", got)
	}
}

// Everything else still waits for the turn to end, and stays in the input so
// it can be sent once it does rather than typed again.
func TestOtherCommandsStillWaitForTheTurnToEnd(t *testing.T) {
	m, _, _ := runningModel(t, "arc-1")
	m.input.SetValue("/model gpt-4")
	was := m.llm.Model

	m, _ = pressEnter(m)

	if m.llm.Model != was {
		t.Errorf("the model switched to %s mid-run, want it left at %s", m.llm.Model, was)
	}
	if got := m.input.Value(); got != "/model gpt-4" {
		t.Errorf("the input holds %q, want the command kept for later", got)
	}
}

// A mistyped command must not be answered mid-run either, or the reply lands
// in the middle of a reply being written.
func TestAnUnknownCommandIsNotAnsweredMidRun(t *testing.T) {
	m, _, _ := runningModel(t, "arc-1")
	m.input.SetValue("/canel")

	m, _ = pressEnter(m)

	if len(m.transcript.blocks) != 0 {
		t.Errorf("the screen shows %+v mid-run, want nothing", m.transcript.blocks)
	}
	if got := m.input.Value(); got != "/canel" {
		t.Errorf("the input holds %q, want the typo kept", got)
	}
}
