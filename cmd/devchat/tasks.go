package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/events"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The task here is a goroutine standing in for work that runs somewhere else.
// It reports the way a real one would, by calling the ingress rather than
// writing to the log.
//
// Reaching a goroutine needs a table of handles. That table lives here, in the
// implementation, and is never consulted for status: what a task is doing is
// read from the log.

// auditDelay is how long the fake audit spends on each of its steps. It is a
// variable so a test does not have to wait out a demo.
var auditDelay = 4 * time.Second

// localTasks runs tasks as goroutines and is the controller that reaches them.
// The two halves are one object because they share the handle table.
type localTasks struct {
	mu      sync.Mutex
	running map[string]*localTask

	// reports is how a task says what it is doing. It is set after
	// construction because the ingress is built from an agent, and the agent
	// is built from the registry these tasks are registered in.
	reports *agent.TaskReports

	// unfiled is told about a report that could not be written. Everything a
	// task says that does reach the log reaches the screen through the event
	// sink; this is the one case there is nothing in the log to publish.
	unfiled func(ref agent.TaskRef, err error)

	// deferred is told when a report did not get its arc a turn because
	// something else was working it. The report is filed either way; what is
	// owed is the turn.
	deferred func(arcID string)
}

// localTask is one running goroutine and the two ways to reach it.
type localTask struct {
	input  chan string
	cancel context.CancelFunc

	// awaiting is the request the task has stopped at, and is empty whenever
	// it is not waiting for anything. It is read and cleared under
	// localTasks.mu, so of two answers to one question only the first finds
	// it there.
	awaiting string
}

func newLocalTasks() *localTasks {
	return &localTasks{running: map[string]*localTask{}}
}

// attach gives the tasks somewhere to report to, once there is one.
func (l *localTasks) attach(reports *agent.TaskReports, unfiled func(agent.TaskRef, error), deferred func(string)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reports, l.unfiled, l.deferred = reports, unfiled, deferred
}

// register adds the canned tasks to a registry.
func (l *localTasks) register(registry *agent.TaskRegistry) error {
	return registry.RegisterTask(&mcp.Tool{
		Name: "audit_namespaces",
		Description: "Audit every namespace for problems. This takes a while, runs on its own, " +
			"and will ask which namespace to look at in detail.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"depth": map[string]any{
					"type":        "string",
					"description": "How thorough to be: quick or full.",
				},
			},
		},
	}, l.start)
}

// start launches an audit and returns, the way a call to a job runner would.
// The context is not the run's, since a task outlives the run that started
// it.
func (l *localTasks) start(_ context.Context, ref agent.TaskRef, args any) error {
	ctx, cancel := context.WithCancel(context.Background())
	task := &localTask{input: make(chan string, 1), cancel: cancel}

	l.mu.Lock()
	l.running[ref.ID] = task
	l.mu.Unlock()

	go l.audit(ctx, ref, task)
	return nil
}

// Cancel asks a running audit to stop. It settles nothing: the goroutine
// reports what became of it, which is what a job runner would do too.
func (l *localTasks) Cancel(_ context.Context, ref agent.TaskRef, _ string) error {
	task, ok := l.lookup(ref.ID)
	if !ok {
		return fmt.Errorf("task %s is not running here", ref.ID)
	}
	task.cancel()
	return nil
}

// ProvideInput hands a waiting audit its answer, and refuses an answer to a
// question it has moved on from. This is the only place that can: whoever
// calls decided from a replay taken before the call.
func (l *localTasks) ProvideInput(_ context.Context, ref agent.TaskRef, requestID, input string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	task, ok := l.running[ref.ID]
	if !ok {
		return fmt.Errorf("task %s is not running here", ref.ID)
	}
	if task.awaiting != requestID {
		return fmt.Errorf("task %s is not waiting on request %s", ref.ID, requestID)
	}

	// Clearing the request first is what makes the refusal exact. The channel
	// is buffered and a task asks again only once it has taken the last
	// answer, so the send cannot block under the lock.
	task.awaiting = ""
	task.input <- input
	return nil
}

// await records which question a task has stopped at, so an answer to it can
// be told from an answer to one it has already taken.
func (l *localTasks) await(id, requestID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if task, ok := l.running[id]; ok {
		task.awaiting = requestID
	}
}

func (l *localTasks) lookup(id string) (*localTask, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	task, ok := l.running[id]
	return task, ok
}

func (l *localTasks) forget(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.running, id)
}

// audit is the work. It reports twice, stops to ask a question, and finishes.
// Every report goes through the ingress: write the event, then give the arc a
// turn.
func (l *localTasks) audit(ctx context.Context, ref agent.TaskRef, task *localTask) {
	defer l.forget(ref.ID)

	if !l.pause(ctx, ref, "scanning namespaces") {
		return
	}
	l.report(ref, agent.TaskReport{
		Kind: agent.TaskReportNotification, Message: "scanned 41 namespaces, 3 look busy"})

	// The request is named and taken up before it is filed, because filing it
	// works the arc, and the answer can be back before report returns.
	requestID := agent.NewEventID()
	l.await(ref.ID, requestID)
	l.report(ref, agent.TaskReport{
		Kind: agent.TaskReportInputRequested, RequestID: requestID,
		Message: "which of kube-system, ingress or data should I look at in detail?"})

	var chosen string
	select {
	case chosen = <-task.input:
	case <-ctx.Done():
		l.stopped(ref)
		return
	}

	if !l.pause(ctx, ref, "auditing "+chosen) {
		return
	}
	l.report(ref, agent.TaskReport{
		Kind:    agent.TaskReportCompleted,
		Outcome: events.AsyncTaskOutcomeSuccess,
		Result:  fmt.Sprintf("%s: 2 pods without resource limits, 1 deployment with a single replica", chosen),
	})
}

// pause waits, and says whether the task should carry on. A task that was
// stopped reports that it was, because nothing else can say so.
func (l *localTasks) pause(ctx context.Context, ref agent.TaskRef, _ string) bool {
	select {
	case <-time.After(auditDelay):
		return true
	case <-ctx.Done():
		l.stopped(ref)
		return false
	}
}

func (l *localTasks) stopped(ref agent.TaskRef) {
	l.report(ref, agent.TaskReport{
		Kind:    agent.TaskReportCompleted,
		Outcome: events.AsyncTaskOutcomeCancelled,
		Result:  "stopped before the audit finished",
	})
}

// report calls the ingress, which writes the report and gives the arc a turn.
// Nothing here tells the screen what was said: the report is published from
// the persistor that wrote it, like everything else in the log.
//
// A report that cannot be filed is the exception, since there is nothing to
// publish and nobody above a task to hand the failure to.
func (l *localTasks) report(ref agent.TaskRef, report agent.TaskReport) {
	l.mu.Lock()
	reports, unfiled, deferred := l.reports, l.unfiled, l.deferred
	l.mu.Unlock()

	if reports == nil {
		return
	}

	worked, err := reports.Report(context.Background(), ref, report)
	if err != nil {
		if unfiled != nil {
			unfiled(ref, err)
		}
		return
	}
	if !worked && deferred != nil {
		deferred(ref.ArcID)
	}
}
