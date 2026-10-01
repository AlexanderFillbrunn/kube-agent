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

	// watcher is told what a task reported, so the interface can show it. The
	// log is the record; this is only the screen.
	watcher func(ref agent.TaskRef, text string, state toolState)

	// deferred is told when a report did not get its arc a turn because
	// something else was working it. The report is filed either way; what is
	// owed is the turn.
	deferred func(arcID string)
}

// localTask is one running goroutine and the two ways to reach it.
type localTask struct {
	input  chan string
	cancel context.CancelFunc
}

func newLocalTasks() *localTasks {
	return &localTasks{running: map[string]*localTask{}}
}

// attach gives the tasks somewhere to report to, once there is one.
func (l *localTasks) attach(reports *agent.TaskReports, watcher func(agent.TaskRef, string, toolState), deferred func(string)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reports, l.watcher, l.deferred = reports, watcher, deferred
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

// ProvideInput hands a waiting audit its answer.
func (l *localTasks) ProvideInput(_ context.Context, ref agent.TaskRef, input string) error {
	task, ok := l.lookup(ref.ID)
	if !ok {
		return fmt.Errorf("task %s is not running here", ref.ID)
	}
	select {
	case task.input <- input:
		return nil
	default:
		return fmt.Errorf("task %s is not waiting for input", ref.ID)
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

	l.report(ref, agent.TaskReport{
		Kind: agent.TaskReportInputRequested, Message: "which of kube-system, ingress or data should I look at in detail?"})

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

// report calls the ingress and tells the screen what was said. A report that
// cannot be filed is shown and dropped, since there is nobody above a task to
// hand the failure to.
func (l *localTasks) report(ref agent.TaskRef, report agent.TaskReport) {
	l.mu.Lock()
	reports, watcher, deferred := l.reports, l.watcher, l.deferred
	l.mu.Unlock()

	if watcher != nil {
		watcher(ref, reportText(report), reportState(report))
	}
	if reports == nil {
		return
	}

	worked, err := reports.Report(context.Background(), ref, report)
	if err != nil {
		if watcher != nil {
			watcher(ref, fmt.Sprintf("could not be filed: %v", err), toolFailed)
		}
		return
	}
	if !worked && deferred != nil {
		deferred(ref.ArcID)
	}
}

func reportText(report agent.TaskReport) string {
	switch report.Kind {
	case agent.TaskReportInputRequested:
		return "asks: " + report.Message
	case agent.TaskReportCompleted:
		return fmt.Sprintf("%v", report.Result)
	default:
		return report.Message
	}
}

func reportState(report agent.TaskReport) toolState {
	switch {
	case report.Kind == agent.TaskReportInputRequested:
		return toolWaiting
	case report.Kind != agent.TaskReportCompleted:
		return toolRunning
	case report.Outcome == events.AsyncTaskOutcomeCancelled:
		return toolStopped
	case report.Outcome == events.AsyncTaskOutcomeFailed:
		return toolFailed
	default:
		return toolDone
	}
}
