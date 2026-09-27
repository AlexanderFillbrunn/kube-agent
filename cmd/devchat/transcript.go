package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
)

// This file turns a run into something renderable, and nothing here draws.
// The transcript is fed from two directions — text as the model streams it,
// and events as the agent records them — and it is the events that are
// authoritative: a reply is the one the log holds, not the one the tokens
// happened to spell.

type blockKind int

const (
	blockUser blockKind = iota
	blockAssistant
	blockTool
	blockError
	// blockNote is the harness speaking, rather than anyone in the
	// conversation. Nothing in a note reaches the model.
	blockNote
	// blockTask is an async task: what it is, and the last thing it said.
	blockTask
)

// toolState is how a call is doing, which is not its recorded status: a call
// the tool is still working on has no status yet, and that is the state a
// reader most wants to see.
type toolState string

const (
	toolQueued  toolState = "queued"
	toolRunning toolState = "running"
	toolDone    toolState = "done"
	toolFailed  toolState = "failed"
	// toolUnknown is a call that reached its tool and never reported before
	// everything stopped. Whether it took effect is not known, and showing it
	// as still running would claim work nobody is doing.
	toolUnknown toolState = "unknown"
	// toolWaiting is a task that has stopped to ask for something. Only a task
	// reaches it: a tool call that is waiting is waiting on its own tool.
	toolWaiting toolState = "waiting"
	// toolStopped is a task whose runner reported that it was called off.
	toolStopped toolState = "stopped"
)

type block struct {
	kind   blockKind
	author string
	text   string

	// Tool and task blocks only.
	id     string
	tool   string
	args   string
	state  toolState
	result string
}

// transcript is the conversation as the screen shows it.
type transcript struct {
	blocks []*block

	// streaming is the reply being typed out, and is nil between replies. It
	// is already in blocks: the pointer is kept so tokens have somewhere to
	// land.
	streaming *block

	// tools indexes tool blocks by the ID of the call that created them, so a
	// dispatch or a result finds the line it belongs to.
	tools map[string]*block

	// tasks indexes task blocks by task ID, for the same reason: everything a
	// task says lands on the one line that is already on the screen.
	tasks map[string]*block
}

func newTranscript() *transcript {
	return &transcript{tools: map[string]*block{}, tasks: map[string]*block{}}
}

// fromHistory fills a transcript with a conversation that is already recorded.
func (t *transcript) fromHistory(h *history.History) {
	for _, arc := range h.Arcs {
		author := "you"
		if arc.Actor != nil {
			author = arc.Actor.UserID
		}
		stopped := arc.Status() == history.ArcStatusCancelled || arc.Status() == history.ArcStatusError
		t.blocks = append(t.blocks, &block{kind: blockUser, author: author, text: arc.Content})

		for _, response := range arc.Responses {
			if response.Content != "" {
				t.blocks = append(t.blocks, &block{kind: blockAssistant, text: response.Content})
			}
			for _, call := range response.ToolCalls {
				t.blocks = append(t.blocks, &block{
					kind:   blockTool,
					id:     call.ID,
					tool:   call.ToolName,
					args:   compact(call.Args),
					state:  stateOf(call, stopped),
					result: compact(call.Result),
				})
			}
		}

		// Tasks come after the arc's replies rather than in the order they
		// were invoked. A task is shown as where it has got to, and where it
		// has got to is a thing about now, not about the point in the
		// conversation it was started at.
		for _, task := range arc.AsyncTasks {
			shown := &block{
				kind:  blockTask,
				id:    task.ID,
				tool:  task.TaskName,
				args:  compact(task.Args),
				state: taskStateOf(task),
				text:  taskTextOf(task),
			}
			t.blocks = append(t.blocks, shown)
			t.tasks[task.ID] = shown
		}
	}
}

// taskStateOf reads how a task is doing from the log.
func taskStateOf(task *history.AsyncTask) toolState {
	switch task.Status {
	case history.AsyncTaskSuccess:
		return toolDone
	case history.AsyncTaskFailed:
		return toolFailed
	case history.AsyncTaskCancelled:
		return toolStopped
	case history.AsyncTaskWaiting:
		return toolWaiting
	default:
		return toolRunning
	}
}

// taskTextOf is the last thing a task said, which for a finished one is what
// it finished with and for a running one is whatever it reported last.
func taskTextOf(task *history.AsyncTask) string {
	if task.FinishedSeq != nil {
		return compact(task.Result)
	}
	if len(task.Events) > 0 {
		last := task.Events[len(task.Events)-1]
		if last.Type == history.AsyncTaskEventInputRequested {
			return "asks: " + compact(last.Payload)
		}
		return compact(last.Payload)
	}
	return "started"
}

// token adds streamed text to the reply being written.
func (t *transcript) token(text string) {
	if t.streaming == nil {
		t.streaming = &block{kind: blockAssistant}
		t.blocks = append(t.blocks, t.streaming)
	}
	t.streaming.text += text
}

// event folds one recorded event into the transcript.
func (t *transcript) event(ev *events.Event) {
	switch payload := ev.Payload.(type) {
	case events.UserMessagePayload:
		author := payload.Actor.UserID
		if author == "" {
			author = "you"
		}
		t.blocks = append(t.blocks, &block{kind: blockUser, author: author, text: payload.Content})

	case events.AssistantMessagePayload:
		// The recorded reply settles what was said. A message with no text is
		// a message that only asked for tools, and it leaves no block behind.
		switch {
		case t.streaming != nil:
			t.streaming.text = payload.Content
			if payload.Content == "" {
				t.drop(t.streaming)
			}
		case payload.Content != "":
			t.blocks = append(t.blocks, &block{kind: blockAssistant, text: payload.Content})
		}
		t.streaming = nil

	case events.ToolCallPayload:
		called := &block{
			kind:  blockTool,
			id:    ev.ID,
			tool:  payload.ToolName,
			args:  compact(payload.Args),
			state: toolQueued,
		}
		t.blocks = append(t.blocks, called)
		t.tools[ev.ID] = called

	case events.ToolDispatchedPayload:
		if called, ok := t.tools[payload.CausationID]; ok {
			called.state = toolRunning
		}

	case events.ToolResultPayload:
		if called, ok := t.tools[payload.CausationID]; ok {
			called.state = toolDone
			if payload.IsError {
				called.state = toolFailed
			}
			called.result = compact(payload.Result)
		}

	case events.AsyncTaskInvokePayload:
		started := &block{
			kind:  blockTask,
			id:    ev.ID,
			tool:  payload.TaskName,
			args:  compact(payload.Args),
			state: toolRunning,
			text:  "started",
		}
		t.blocks = append(t.blocks, started)
		t.tasks[ev.ID] = started

	case events.AsyncTaskEventPayload:
		t.onTask(payload.CausationID, toolRunning, compact(payload.Payload))

	case events.AsyncTaskInputRequestedPayload:
		t.onTask(payload.CausationID, toolWaiting, "asks: "+payload.Message)

	case events.AsyncTaskInputProvidedPayload:
		t.onTask(payload.CausationID, toolRunning, "was told: "+payload.Message)

	case events.AsyncTaskResultPayload:
		state := toolDone
		switch payload.Outcome {
		case events.AsyncTaskOutcomeFailed:
			state = toolFailed
		case events.AsyncTaskOutcomeCancelled:
			state = toolStopped
		}
		t.onTask(payload.CausationID, state, compact(payload.Result))

	case events.ErrorPayload:
		t.blocks = append(t.blocks, &block{kind: blockError, text: payload.Message})
	}
}

// onTask puts the latest thing a task said on the line that task already has.
// A task reports many times and each report replaces the last, because what a
// reader wants from a line about a long job is where it has got to.
func (t *transcript) onTask(taskID string, state toolState, text string) {
	task, ok := t.tasks[taskID]
	if !ok {
		return
	}
	task.state = state
	task.text = text
}

// taskSaid shows what a task reported.
//
// It comes from the task rather than from the log, because what the log holds
// is written on a goroutine of its own and never passes the sink the interface
// watches. The line it lands on is the same one the log's own records reach, so
// a reopened session and a running one look alike.
func (t *transcript) taskSaid(taskID, name, text string, state toolState) {
	task, ok := t.tasks[taskID]
	if !ok {
		task = &block{kind: blockTask, id: taskID, tool: name}
		t.blocks = append(t.blocks, task)
		t.tasks[taskID] = task
	}
	task.text = text
	task.state = state
}

// runningTasks names the tasks that have not finished, in the order they were
// started.
//
// It reads the transcript rather than the log because the transcript is what
// the interface already keeps up to date: a task reports from a goroutine of
// its own, and re-reading the file on every redraw to say the same thing would
// be work for nothing.
//
// A task waiting on input counts as running. It has not finished, and it is the
// one most worth naming: somebody has to answer it.
func (t *transcript) runningTasks() []string {
	var names []string
	for _, b := range t.blocks {
		if b.kind == blockTask && (b.state == toolRunning || b.state == toolWaiting) {
			names = append(names, b.tool)
		}
	}
	return names
}

// note records something the harness has to say for itself.
func (t *transcript) note(text string) {
	t.blocks = append(t.blocks, &block{kind: blockNote, text: text})
}

// fail records something that went wrong.
//
// The agent writes down what it could not finish, so a failed run reaches the
// screen twice: once as the error it recorded and once as the error it
// returned. They say the same thing, and it is only worth saying once.
func (t *transcript) fail(err error) {
	t.streaming = nil
	if len(t.blocks) > 0 {
		if last := t.blocks[len(t.blocks)-1]; last.kind == blockError && last.text == err.Error() {
			return
		}
	}
	t.blocks = append(t.blocks, &block{kind: blockError, text: err.Error()})
}

func (t *transcript) drop(target *block) {
	for i, b := range t.blocks {
		if b == target {
			t.blocks = append(t.blocks[:i], t.blocks[i+1:]...)
			return
		}
	}
}

// stateOf reads how a call is doing. arcStopped says the arc is over, so a
// call with no result is not one still being worked on.
func stateOf(call *history.ToolCall, arcStopped bool) toolState {
	switch call.Status {
	case history.ToolCallStatusSuccess:
		return toolDone
	case history.ToolCallStatusPending:
		switch {
		case call.Phase() != history.ToolCallPhaseDispatched:
			return toolQueued
		case arcStopped:
			return toolUnknown
		default:
			return toolRunning
		}
	default:
		return toolFailed
	}
}

// compact renders a value as one line. Tool arguments and results are JSON in
// the log, and a line of it says more here than a pretty-printed page would.
func compact(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.Join(strings.Fields(text), " ")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

// truncate shortens a line to fit, marking that it was cut.
func truncate(text string, width int) string {
	if width <= 1 || len([]rune(text)) <= width {
		return text
	}
	return string([]rune(text)[:width-1]) + "…"
}
