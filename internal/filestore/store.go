// Package filestore keeps an event log in a single file, so the agent can be
// exercised end to end without standing anything up.
//
// It is a development and test store, under internal/ so that it cannot become
// anything else. It holds the whole log in memory on every read, scans the file
// for every query, never compacts, and assumes it is the only writer.
//
// What it does hold to is the contract the projection depends on. See
// GetEventsForArc.
package filestore

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/events"
)

// Store is an append-only log of JSON events, one per line.
type Store struct {
	mu   sync.Mutex
	path string
	file *os.File

	// seq is the sequence number of the last event written, or -1 for an empty
	// log, so that the first event is numbered 0 as the fold expects.
	seq int
}

// Open returns a store over the file at path, creating it and the directory
// holding it if they are not there, and picking up where an existing log left
// off.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	existing, err := readFile(path)
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	seq := -1
	if len(existing) > 0 {
		seq = existing[len(existing)-1].Seq
	}
	return &Store{path: path, file: file, seq: seq}, nil
}

// Rotate moves the log aside and starts an empty one in its place, returning
// the path the old one was moved to, or "" where there was nothing to move. The
// log is renamed rather than truncated, so the old session stays on disk.
func (s *Store) Rotate() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.seq < 0 {
		// An empty log is already a fresh one, and rotating it would leave an
		// empty file behind for every attempt.
		return "", nil
	}

	if err := s.file.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", s.path, err)
	}

	archived := fmt.Sprintf("%s.%s", s.path, time.Now().UTC().Format("20060102-150405"))
	if err := os.Rename(s.path, archived); err != nil {
		return "", fmt.Errorf("move %s aside: %w", s.path, err)
	}

	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", s.path, err)
	}
	s.file = file
	s.seq = -1
	return archived, nil
}

// Close releases the file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}

// Persist writes events and returns them as they were stored. The sequence
// number is the store's to assign, since nothing outside the lock knows the
// position in the log, and it is the only thing the store assigns.
//
// An event arrives named and timed, which is agent.MintIdentity's job on the
// way in. One that does not is a bug in the writer and is refused, since a log
// line with no ID is one nothing can refer to afterwards.
//
// A batch is one write and one sync, and the sequence number only advances
// once it is on disk. That is as close to atomic as a file gets: a write that
// fails part way still leaves a torn line that readFile refuses, which is the
// kind of thing this store is allowed to do and a real one is not.
func (s *Store) Persist(_ context.Context, evs ...*events.Event) ([]*events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(evs) == 0 {
		return nil, nil
	}

	seq := s.seq
	stored := make([]*events.Event, 0, len(evs))
	var lines []byte
	for _, ev := range evs {
		if ev.ID == "" {
			return nil, fmt.Errorf("%s event has no ID", ev.Typ)
		}
		if ev.Timestamp.IsZero() {
			return nil, fmt.Errorf("%s event %s has no timestamp", ev.Typ, ev.ID)
		}
		// The caller's event is left alone, since they may still be holding it.
		one := *ev
		seq++
		one.Seq = seq

		line, err := json.Marshal(&one)
		if err != nil {
			return nil, fmt.Errorf("marshal %s event: %w", one.Typ, err)
		}
		lines = append(append(lines, line...), '\n')
		stored = append(stored, &one)
	}

	if _, err := s.file.Write(lines); err != nil {
		return nil, fmt.Errorf("append to %s: %w", s.path, err)
	}
	// A crash between the write and the next step is exactly the case the
	// dispatch record exists for, so the lines are on disk before the caller
	// acts on them.
	if err := s.file.Sync(); err != nil {
		return nil, fmt.Errorf("sync %s: %w", s.path, err)
	}
	s.seq = seq
	return stored, nil
}

// Events returns the whole log in the order it was written.
func (s *Store) Events(_ context.Context) ([]*events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return pointers(readFile(s.path))
}

// GetEventsForArc returns the events one arc is projected from: everything in
// its session up to and including the user message that opened it, plus
// everything correlated to it afterwards.
//
// That query is the projection's cutoff. A store that returned more, or less,
// would change what the model is told. See PROJECTION.md.
//
// A file holds one session, so the session in the ref only ever rules events
// out here. A store that held more than one would partition on it.
func (s *Store) GetEventsForArc(_ context.Context, ref agent.ArcRef) ([]*events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := readFile(s.path)
	if err != nil {
		return nil, err
	}

	opening := -1
	for i := range all {
		if all[i].Typ == events.UserMessageEventType && belongsTo(&all[i], ref) {
			opening = all[i].Seq
			break
		}
	}
	if opening < 0 {
		return nil, fmt.Errorf("no arc %s in %s", ref, s.path)
	}

	var selected []*events.Event
	for i := range all {
		if all[i].SessionID != ref.SessionID {
			continue
		}
		if all[i].Seq <= opening || correlatesTo(&all[i], ref.ArcID) {
			selected = append(selected, &all[i])
		}
	}
	return selected, nil
}

// Cancelled returns the cancel recorded against an arc, or nil where there is
// none. It scans the log, which is what everything here does.
func (s *Store) Cancelled(_ context.Context, ref agent.ArcRef) (*events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := readFile(s.path)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Typ == events.CancelEventType && belongsTo(&all[i], ref) {
			return &all[i], nil
		}
	}
	return nil, nil
}

func correlatesTo(ev *events.Event, arcID string) bool {
	return ev.CorrelationID != nil && *ev.CorrelationID == arcID
}

// belongsTo reports whether an event is part of the arc named, in the session
// named. An arc ID is unique on its own, so the session only catches a ref
// that was built wrong.
func belongsTo(ev *events.Event, ref agent.ArcRef) bool {
	return ev.SessionID == ref.SessionID && correlatesTo(ev, ref.ArcID)
}

// readFile decodes the whole log. A missing file is an empty log, since a
// session starts by writing its first event.
func readFile(path string) ([]events.Event, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()

	var decoded []events.Event
	reader := bufio.NewReader(file)
	for line := 1; ; line++ {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			var ev events.Event
			if err := json.Unmarshal(raw, &ev); err != nil {
				return nil, fmt.Errorf("decode %s line %d: %w", path, line, err)
			}
			decoded = append(decoded, ev)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return decoded, nil
			}
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
}

func pointers(decoded []events.Event, err error) ([]*events.Event, error) {
	if err != nil {
		return nil, err
	}
	out := make([]*events.Event, 0, len(decoded))
	for i := range decoded {
		out = append(out, &decoded[i])
	}
	return out, nil
}

// NewID mints an identifier for an event or an arc.
func NewID() string {
	var buf [16]byte
	rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
