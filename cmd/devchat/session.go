package main

import (
	"context"
	"fmt"

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

	// worker runs the arc, under the lease that keeps two runs off it. A
	// person's turn acquires it like everything else that enters an arc.
	worker *agent.ArcWorker

	actor users.Actor

	// controller reaches the tasks an arc started, so that stopping the arc
	// stops them too. It is nil for a session with no tasks.
	controller agent.TaskController
}

// newArcID mints the identifier for an arc about to be opened. The caller
// names the arc up front, because cancelling needs the ID before the run has
// written anything.
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

	worked, err := s.worker.Work(ctx, arcID)
	if err != nil {
		return err
	}
	if !worked {
		// The message above opened this arc, so nothing can be holding it.
		// Something that does means the ID was not as new as it looked.
		return fmt.Errorf("arc %s is already being worked", arcID)
	}
	return nil
}

// cancel records that the user withdrew an arc. Writing it is the caller's
// job, since only they can name the actor.
//
// Stopping the run is separate and both are needed: the record is what the log
// and the next projection read, and the context is what interrupts a running
// tool. The context here is not the run's, which is about to be cancelled.
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

// stopTasks asks whatever the arc started to stop. Which tasks those are is
// read from the log, and nothing is written: what became of work already under
// way is the runner's to report.
//
// A task that cannot be reached does not fail the cancel.
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
