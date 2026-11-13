package agent

import (
	"context"
	"errors"
	"fmt"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
)

// This file is the inbound half of async work. A task says what it is doing by
// calling in here: the report is written to the log, and the arc it belongs to
// is worked again so the model can account for it.

// TaskReport is one thing a task has to say.
type TaskReport struct {
	// Kind says which of the three it is.
	Kind TaskReportKind

	// Message is what a notification or an input request says.
	Message string

	// RequestID names an input request, and is the ID its event is written
	// under. A task mints it before it asks, because the answer comes back
	// naming it and the task has to recognise which question it answers.
	RequestID string

	// Payload is what a notification carries, where it carries structure
	// rather than a sentence.
	Payload any

	// Result and Outcome settle the task, and are read for a completion.
	Result  any
	Outcome events.AsyncTaskOutcome
}

// TaskReportKind is what a task is calling in to say.
type TaskReportKind string

const (
	// TaskReportNotification is progress. The task keeps running.
	TaskReportNotification TaskReportKind = "NOTIFICATION"
	// TaskReportInputRequested is the task stopping to ask for something.
	TaskReportInputRequested TaskReportKind = "INPUT_REQUESTED"
	// TaskReportCompleted settles the task, however it ended.
	TaskReportCompleted TaskReportKind = "COMPLETED"
)

// TaskReports takes what running tasks have to say and turns it into work.
type TaskReports struct {
	// worker is how the arc is reached, shared with every other way in.
	worker *ArcWorker
}

// NewTaskReports builds the inbound half over the worker that runs arcs.
func NewTaskReports(worker *ArcWorker) (*TaskReports, error) {
	if worker == nil {
		return nil, errors.New("reports need an arc worker")
	}
	return &TaskReports{worker: worker}, nil
}

// Report records what a task said and gives its arc another turn. It returns
// false when somebody else held the arc: the report is in the log, and the
// caller is the one that knows when to come back for the turn.
//
// The report is one work item, and agentID names the agent that works the arc
// it hands over, by the same rule as ArcWorker.Work: a caller that cannot name
// one has nothing to hand the worker. Whoever hands a report over learns the
// agent the way the harness that owns the session does; nothing here derives
// it, since the framework has no way to know who works a session.
//
// The arc is worked whatever the report was, since whether there is anything
// to do is decided by reading the log. A report naming an arc that does not
// own the task is refused.
func (r *TaskReports) Report(ctx context.Context, ref TaskRef, agentID string, report TaskReport) (bool, error) {
	if agentID == "" {
		return false, fmt.Errorf("a report on task %s names no agent to work arc %s", ref.ID, ref.ArcID)
	}
	h, err := r.worker.arcHistory(ctx, ref.ArcRef)
	if err != nil {
		return false, err
	}
	if !holdsTask(h, ref) {
		return false, fmt.Errorf("arc %s holds no task %s", ref.ArcID, ref.ID)
	}

	ev, err := reportEvent(ref, report)
	if err != nil {
		return false, err
	}
	if _, err := AsAgent(agentID, r.worker.persistor).Persist(ctx, ev); err != nil {
		return false, err
	}

	return r.worker.Work(ctx, ref.ArcRef, agentID)
}

// holdsTask reports whether the arc named really owns the task named.
func holdsTask(h *history.History, ref TaskRef) bool {
	for _, arc := range h.Arcs {
		if arc.ID != ref.ArcID {
			continue
		}
		for _, task := range arc.AsyncTasks {
			if task.ID == ref.ID {
				return true
			}
		}
	}
	return false
}

// reportEvent turns a report into the record of it.
func reportEvent(ref TaskRef, report TaskReport) (*events.Event, error) {
	ev := &events.Event{SessionID: ref.SessionID, CorrelationID: &ref.ArcID}
	switch report.Kind {
	case TaskReportNotification:
		payload := report.Payload
		if payload == nil {
			payload = report.Message
		}
		ev.Typ = events.AsyncTaskEventEventType
		ev.Payload = events.AsyncTaskEventPayload{CausationID: ref.ID, Payload: payload}

	case TaskReportInputRequested:
		if report.RequestID == "" {
			return nil, fmt.Errorf("task %s asked for input without naming the request", ref.ID)
		}
		// The task named the request, so the event is written under that name
		// rather than one the store mints.
		ev.ID = report.RequestID
		ev.Typ = events.AsyncTaskInputRequestedEventType
		ev.Payload = events.AsyncTaskInputRequestedPayload{CausationID: ref.ID, Message: report.Message}

	case TaskReportCompleted:
		if !report.Outcome.Valid() {
			return nil, fmt.Errorf("task %s reported completion without saying how it ended", ref.ID)
		}
		ev.Typ = events.AsyncTaskResultEventType
		ev.Payload = events.AsyncTaskResultPayload{
			CausationID: ref.ID, Result: report.Result, Outcome: report.Outcome}

	default:
		return nil, fmt.Errorf("task %s reported something of no known kind: %q", ref.ID, report.Kind)
	}
	return ev, nil
}

// SettleAbandonedTasks closes off tasks whose runners died with the process
// that started them, and reports how many it settled. Left alone, each one
// holds its arc open for ever.
//
// They are settled as failed rather than cancelled, since nobody stopped
// them.
func SettleAbandonedTasks(ctx context.Context, persistor EventPersistor, h *history.History) (int, error) {
	settled := 0
	for _, arc := range h.Arcs {
		for _, task := range arc.AsyncTasks {
			if task.FinishedSeq != nil {
				continue
			}
			arcID := arc.ID
			_, err := persistor.Persist(ctx, &events.Event{
				SessionID:     h.SessionID,
				Typ:           events.AsyncTaskResultEventType,
				CorrelationID: &arcID,
				Payload: events.AsyncTaskResultPayload{
					CausationID: task.ID,
					Outcome:     events.AsyncTaskOutcomeFailed,
					Result: fmt.Sprintf(
						"task %s was started by a process that is no longer running, and nothing "+
							"has reported on it since. Whether it did its work is not known.",
						task.TaskName),
				},
			})
			if err != nil {
				return settled, err
			}
			settled++
		}
	}
	return settled, nil
}
