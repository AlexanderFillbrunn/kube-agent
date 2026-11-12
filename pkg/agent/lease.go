package agent

import (
	"context"
	"sync"
)

// ArcLease keeps two runs off one arc.
type ArcLease interface {
	// Acquire takes the arc. ok is false when somebody else holds it, which
	// is not a failure: the caller leaves the arc to them and the arc is owed
	// a run.
	//
	// An error means the lease could not be reached, and the caller learns
	// nothing about who holds the arc. The two have to be told apart, since a
	// caller that read an unreachable lock service as "nobody holds it" would
	// run the arc twice.
	//
	// Nothing here expires a lease or fences a writer. A holder that dies
	// holds the arc until whatever granted it decides otherwise, and a holder
	// whose lease expired goes on writing, because EventPersistor has no way
	// to say what the writer expected the log to look like.
	Acquire(ctx context.Context, ref ArcRef) (release func(), ok bool, err error)
}

// LocalLease is an ArcLease for one process.
type LocalLease struct {
	mu   sync.Mutex
	held map[ArcRef]bool
}

func NewLocalLease() *LocalLease { return &LocalLease{held: map[ArcRef]bool{}} }

// Acquire never returns an error: what it consults is in this process.
func (l *LocalLease) Acquire(_ context.Context, ref ArcRef) (func(), bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[ArcRef]bool{}
	}
	if l.held[ref] {
		return nil, false, nil
	}
	l.held[ref] = true

	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.held, ref)
	}, true, nil
}