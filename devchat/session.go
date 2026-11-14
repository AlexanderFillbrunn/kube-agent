package main

import (
	"context"
	"fmt"

	"fillbrunn.ai/kube-agent/devchat/filestore"
	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
)

// session is one conversation against one log. It knows nothing about the
// screen: what it writes reaches the interface through the sink the persistor
// publishes to.
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

	// cancels tells a run in progress that its arc was withdrawn, which is what
	// reaches a tool already running. It is nil for a session that only stops
	// runs between steps.
	cancels agent.CancelNotifier

	actor users.Actor
}

// newArcID mints the identifier for an arc about to be opened. The caller
// names the arc up front, because cancelling needs the ID before the run has
// written anything.
func (s *session) newArcID() string { return filestore.NewID() }

// ref addresses one of this session's arcs. Everything that stores, locks or
// routes by arc is told the session too, since nothing outside this harness
// can assume there is only one.
func (s *session) ref(arcID string) agent.ArcRef {
	return agent.ArcRef{SessionID: s.id, ArcID: arcID}
}

// turn opens an arc with what the user said and runs the agent to the end of
// it.
func (s *session) turn(ctx context.Context, arcID, message string) error {
	opening := &events.Event{
		SessionID:     s.id,
		AgentID:       localAgentID,
		Typ:           events.UserMessageEventType,
		CorrelationID: &arcID,
		Actor:         s.actor,
		Payload:       events.UserMessagePayload{Content: message, Actor: s.actor},
	}
	if _, err := s.persistor.Persist(ctx, opening); err != nil {
		return err
	}

	worked, err := s.worker.Work(ctx, s.ref(arcID), localAgentID)
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
// The record goes down first, because it is what the log and the next
// projection read and it is the only durable part. Telling the run is second
// and may fail: a run that hears nothing stops at its next guarded write
// anyway.
//
// The tasks the arc started are not stopped here. Nobody who cancels is in a
// position to reach work running somewhere else, so that is the worker's, and
// the arc is owed a run for it.
//
// The context here is not the run's, which is about to be cancelled.
func (s *session) cancel(ctx context.Context, arcID, reason string) error {
	payload := events.CancelPayload{}
	if reason != "" {
		payload.Reason = &reason
	}
	if _, err := s.persistor.Persist(ctx, &events.Event{
		SessionID:     s.id,
		AgentID:       localAgentID,
		Typ:           events.CancelEventType,
		CorrelationID: &arcID,
		Actor:         s.actor,
		Payload:       payload,
	}); err != nil {
		return err
	}
	if s.cancels == nil {
		return nil
	}
	return s.cancels.NotifyCancel(ctx, s.ref(arcID))
}

// arcHistory loads and folds the events one arc is run from.
func (s *session) arcHistory(ctx context.Context, arcID string) (*history.History, error) {
	stored, err := s.store.GetEventsForArc(ctx, s.ref(arcID))
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
