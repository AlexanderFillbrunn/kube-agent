package agent

import (
	"context"
	"errors"
	"sync"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
)

// ArcLease keeps two runs off one arc.
type ArcLease interface {
	// Acquire takes the arc. It returns false when the arc is already held.
	Acquire(arcID string) (release func(), ok bool)
}

// LocalLease is an ArcLease for one process.
type LocalLease struct {
	mu   sync.Mutex
	held map[string]bool
}

func NewLocalLease() *LocalLease { return &LocalLease{held: map[string]bool{}} }

func (l *LocalLease) Acquire(arcID string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[arcID] {
		return nil, false
	}
	if l.held == nil {
		l.held = map[string]bool{}
	}
	l.held[arcID] = true

	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.held, arcID)
	}, true
}

// ArcWorker runs the agent over an arc that already exists, under the lease
// that keeps two runs off it. Every way into an arc shares one worker, because
// the lease only excludes callers that acquire the same one.
type ArcWorker struct {
	sessionID string
	persistor EventPersistor
	source    EventSource
	agent     *Agent
	lease     ArcLease
}

// NewArcWorker builds a worker from the parts a run is made of. A nil lease
// becomes a LocalLease.
func NewArcWorker(sessionID string, persistor EventPersistor, source EventSource, agent *Agent, lease ArcLease) (*ArcWorker, error) {
	if persistor == nil || source == nil || agent == nil {
		return nil, errors.New("an arc worker needs a persistor, a source and an agent")
	}
	if lease == nil {
		lease = NewLocalLease()
	}
	return &ArcWorker{sessionID: sessionID, persistor: persistor, source: source, agent: agent, lease: lease}, nil
}

// Work replays an arc from the log and runs the agent over it. It returns
// false without running when another caller holds the arc, which leaves that
// arc owed a run.
func (w *ArcWorker) Work(ctx context.Context, arcID string) (bool, error) {
	release, ok := w.lease.Acquire(arcID)
	if !ok {
		return false, nil
	}
	defer release()

	h, err := w.arcHistory(ctx, arcID)
	if err != nil {
		return false, err
	}
	return true, w.agent.Run(ctx, w.persistor, h, arcID)
}

// arcHistory loads and folds the events one arc is run from.
func (w *ArcWorker) arcHistory(ctx context.Context, arcID string) (*history.History, error) {
	stored, err := w.source.GetEventsForArc(ctx, arcID)
	if err != nil {
		return nil, err
	}
	folded := make([]events.Event, 0, len(stored))
	for _, ev := range stored {
		folded = append(folded, *ev)
	}
	return history.Replay(w.sessionID, folded)
}
