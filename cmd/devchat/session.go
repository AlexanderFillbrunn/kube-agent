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
	agent *agent.Agent
	actor users.Actor
}

// turn opens an arc with what the user said and runs the agent to the end of
// it.
func (s *session) turn(ctx context.Context, message string) error {
	// The arc's ID is minted here because the user message carries it as its
	// correlation ID, and that is what every later record in the arc is filed
	// under.
	arcID := filestore.NewID()
	opening := &events.Event{
		SessionID:     s.id,
		Typ:           events.UserMessageEventType,
		CorrelationID: &arcID,
		Actor:         s.actor,
		Payload:       events.UserMessagePayload{Content: message, Actor: s.actor},
	}
	if _, err := s.store.Persist(ctx, opening); err != nil {
		return err
	}

	h, err := s.arcHistory(ctx, arcID)
	if err != nil {
		return err
	}
	return s.agent.Run(ctx, s.id, h, arcID)
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
