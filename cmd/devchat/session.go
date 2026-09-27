package main

import (
	"context"

	"fillbrunn.ai/kube-agent/internal/filestore"
	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
)

// session is one conversation against one log. It knows nothing about the
// screen: what a run produces reaches the interface through the sinks the
// agent was built with.
type session struct {
	// id names the session in the records it writes. The log's path serves,
	// since a session and its file are the same thing here.
	id    string
	store *filestore.Store
	// persistor is the store with the session's own writing rules around it:
	// what it files is filed under this session, and it refuses work on an arc
	// somebody has cancelled.
	persistor agent.EventPersistor
	agent     *agent.Agent
	actor     users.Actor

	// controller reaches the tasks an arc started, so that stopping the arc
	// stops them too. It is nil for a session with no tasks.
	controller agent.TaskController
}

// newArcID mints the identifier for an arc about to be opened.
//
// The caller names the arc rather than learning its name afterwards, because
// cancelling needs the ID while the run is still going and a run that has not
// written anything yet has nothing to read it from.
func (s *session) newArcID() string { return filestore.NewID() }

// turn opens an arc with what the user said and runs the agent to the end of
// it.
func (s *session) turn(ctx context.Context, arcID, message string) error {
	opening := &events.Event{
		Typ:           events.UserMessageEventType,
		CorrelationID: &arcID,
		Actor:         s.actor,
		Payload:       events.UserMessagePayload{Content: message, Actor: s.actor},
	}
	if _, err := s.persistor.Persist(ctx, opening); err != nil {
		return err
	}

	h, err := s.arcHistory(ctx, arcID)
	if err != nil {
		return err
	}
	return s.agent.Run(ctx, s.persistor, h, arcID)
}

// cancel records that the user withdrew an arc.
//
// Writing this is the caller's job rather than the agent's: whoever decided
// the cancel is the one who can name the actor, and the agent stops without
// ever writing a cancel of its own. Stopping the run is separate, and both are
// needed — the record is what the log and the next turn's projection read, and
// the context is what interrupts a tool that is already running.
//
// The context here is not the run's. That one is about to be cancelled, and a
// cancelled context must not be the thing carrying the write that says so.
func (s *session) cancel(ctx context.Context, arcID, reason string) error {
	payload := events.CancelPayload{}
	if reason != "" {
		payload.Reason = &reason
	}
	if _, err := s.persistor.Persist(ctx, &events.Event{
		Typ:           events.CancelEventType,
		CorrelationID: &arcID,
		Actor:         s.actor,
		Payload:       payload,
	}); err != nil {
		return err
	}
	return s.stopTasks(ctx, arcID, reason)
}

// stopTasks asks whatever the arc started to stop.
//
// Which tasks those are is a question for the log, which is why nothing here
// keeps a list: the arc holds its tasks and an unfinished one is one worth
// asking about. Nothing is written. A cancel ends the waiting, and what became
// of work already under way is the runner's to report.
//
// A task that cannot be reached is not worth failing the cancel over. The
// cancel is recorded, the arc is stopped, and a task nobody can reach is one
// the log already cannot account for.
func (s *session) stopTasks(ctx context.Context, arcID, reason string) error {
	if s.controller == nil {
		return nil
	}
	h, err := s.arcHistory(ctx, arcID)
	if err != nil {
		return err
	}
	for _, arc := range h.Arcs {
		if arc.ID != arcID {
			continue
		}
		for _, task := range arc.AsyncTasks {
			if task.FinishedSeq != nil {
				continue
			}
			ref := agent.TaskRef{ArcID: arc.ID, ID: task.ID, Name: task.TaskName}
			_ = s.controller.Cancel(ctx, ref, reason)
		}
	}
	return nil
}

// arcHistory loads and folds the events one arc is run from.
func (s *session) arcHistory(ctx context.Context, arcID string) (*history.History, error) {
	stored, err := s.store.GetEventsForArc(ctx, arcID)
	if err != nil {
		return nil, err
	}
	return history.Replay(s.id, values(stored))
}

// fullHistory folds the whole log, which is what the conversation already on
// disk is rendered from when the session opens.
func (s *session) fullHistory(ctx context.Context) (*history.History, error) {
	stored, err := s.store.Events(ctx)
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return history.NewHistory(s.id), nil
	}
	return history.Replay(s.id, values(stored))
}

func values(stored []*events.Event) []events.Event {
	out := make([]events.Event, 0, len(stored))
	for _, ev := range stored {
		out = append(out, *ev)
	}
	return out
}
