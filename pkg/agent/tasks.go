package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file holds the three parts an async task is made of and the tools they
// are offered to the model through. Nothing here records what is running. That
// is the log, which only the agent reads and writes.

// The names of the two tools every task-carrying agent offers. The agent
// routes on them, so both sides read the names from here.
const (
	CancelTaskTool       = "cancel_task"
	ProvideTaskInputTool = "provide_task_input"
)

// TaskRef names one running task well enough to reach it from outside this
// process. ID alone is the task's identity, and the rest is addressing.
type TaskRef struct {
	// ArcRef is the arc that owns the task, and the session that arc is in. A
	// report quoting an arc that does not own the task named is rejected, and
	// a report arriving from another process has no other way to say which log
	// to write it to.
	ArcRef

	// ID identifies the task in the log.
	ID string

	// Name is the kind of task, as the registry knows it.
	Name string
}

// Starter launches one kind of task and returns as soon as the work is under
// way. It is handed the reference the task reports back under.
//
// An error means the launch failed, which is not the same as the task never
// having started: the work may be running anyway.
type Starter func(ctx context.Context, ref TaskRef, args any) error

// TaskController reaches a task that is already running. It says how to talk
// to a task, never what a task is doing, which is the log's to answer.
//
// Both methods may be called for a task that has already finished. An
// implementation that cannot find the task returns an error.
type TaskController interface {
	// Cancel asks a task to stop. It does not settle the task: what became of
	// work already under way is the runner's to report.
	Cancel(ctx context.Context, ref TaskRef, reason string) error

	// ProvideInput hands a waiting task what it asked for.
	ProvideInput(ctx context.Context, ref TaskRef, input string) error
}

// TaskDefinition is one kind of task: how the model is offered it, and how one
// is started.
type TaskDefinition struct {
	// Tool is the surface the model sees. Its name is the task's name and its
	// schema is what the arguments are checked against, exactly as for a tool.
	Tool *mcp.Tool

	// Start launches the task.
	Start Starter
}

// TaskRegistry holds the kinds of task an agent may start. It is written
// before any run begins, so one can be shared by every run in a process.
type TaskRegistry struct {
	tasks map[string]TaskDefinition
}

// NewTaskRegistry returns an empty registry ready to take task definitions.
func NewTaskRegistry() *TaskRegistry {
	return &TaskRegistry{tasks: map[string]TaskDefinition{}}
}

// RegisterTask adds one kind of task.
func (r *TaskRegistry) RegisterTask(tool *mcp.Tool, start Starter) error {
	if tool == nil || tool.Name == "" {
		return errors.New("a task needs a tool with a name")
	}
	if start == nil {
		return fmt.Errorf("task %s has no way to start", tool.Name)
	}
	if _, exists := r.tasks[tool.Name]; exists {
		return fmt.Errorf("a task with name %s has already been registered", tool.Name)
	}
	if tool.Name == CancelTaskTool || tool.Name == ProvideTaskInputTool {
		return fmt.Errorf("task name %s is one of the tools every task registry offers", tool.Name)
	}

	r.tasks[tool.Name] = TaskDefinition{Tool: tool, Start: start}
	return nil
}

// Get returns one kind of task by name.
func (r *TaskRegistry) Get(name string) (TaskDefinition, bool) {
	if r == nil {
		return TaskDefinition{}, false
	}
	definition, ok := r.tasks[name]
	return definition, ok
}

// Names returns every registered task name, in a fixed order.
func (r *TaskRegistry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.tasks))
	for name := range r.tasks {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// All returns what the model is offered for these tasks: one tool per kind of
// task, plus the two that address a task already running. The two are
// synthesized, since they are the same tools whatever tasks exist.
func (r *TaskRegistry) All() []*mcp.Tool {
	if r == nil || len(r.tasks) == 0 {
		// With no tasks to start there is nothing to cancel or answer.
		return nil
	}

	tools := make([]*mcp.Tool, 0, len(r.tasks)+2)
	for _, name := range r.Names() {
		tools = append(tools, r.tasks[name].Tool)
	}
	return append(tools, cancelTaskTool(), provideTaskInputTool())
}

func cancelTaskTool() *mcp.Tool {
	return &mcp.Tool{
		Name: CancelTaskTool,
		Description: "Ask a running task to stop. The task reports what became of it, " +
			"so a cancelled task may still have done some of its work.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{
					"type":        "string",
					"description": "The ID of the task to stop, as given in the notes about it.",
				},
				"reason": map[string]any{
					"type":        "string",
					"description": "Why it is being stopped.",
				},
			},
			"required": []any{"task_id"},
		},
	}
}

func provideTaskInputTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        ProvideTaskInputTool,
		Description: "Give a task that is waiting for input the answer it asked for.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{
					"type":        "string",
					"description": "The ID of the waiting task, as given in the notes about it.",
				},
				"input": map[string]any{
					"type":        "string",
					"description": "The answer to what the task asked.",
				},
			},
			"required": []any{"task_id", "input"},
		},
	}
}

// taskOperation is what a call turns out to be once its name has been looked
// up.
type taskOperation int

const (
	// opTool is an ordinary tool call, handled by the tool registry.
	opTool taskOperation = iota
	// opStart begins a task of the named kind.
	opStart
	// opCancel asks a running task to stop.
	opCancel
	// opProvideInput answers a task that is waiting.
	opProvideInput
)

// classify says what a call by this name is. A name that is neither a task's
// nor one of the two operations is an ordinary tool.
func (r *TaskRegistry) classify(name string) taskOperation {
	if r == nil || len(r.tasks) == 0 {
		return opTool
	}
	switch name {
	case CancelTaskTool:
		return opCancel
	case ProvideTaskInputTool:
		return opProvideInput
	}
	if _, ok := r.tasks[name]; ok {
		return opStart
	}
	return opTool
}
