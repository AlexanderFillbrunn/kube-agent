package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
)

var (
	ErrInvalidArcState = errors.New("invalid arc state")
)

type arcState int

const (
	ArcStateNone      arcState = iota
	ArcStateLLM		  arcState = iota
	ArcStateToolCalls arcState = iota
	ArcStateDone      arcState = iota
	ArcStateClose	  arcState = iota
)

type AgentConfig struct {
	SystemPrompt string

	// Tokens receives the model's text as it streams. A nil sink discards it,
	// and the reply is recorded whole either way.
	Tokens Sink[string]

	// Observer is handed every event once it has been folded, so a caller can
	// follow a run as it happens. It only watches, and an observer that
	// returns an error stops the run.
	Observer Sink[*events.Event]
}

type Agent struct {
	config *AgentConfig
	tools *ToolRegistry
	llm LLM

	// tasks and controller are the two halves of async work: what may be
	// started, and how something already running is reached. Both are nil for
	// an agent that runs no tasks.
	tasks      *TaskRegistry
	controller TaskController
}

// AgentOption configures parts of an agent that not every agent has.
type AgentOption func(*Agent) error

// WithTasks gives an agent work it can start and a way to reach that work once
// it is running. Both are required: a registry with no controller starts tasks
// nobody can reach.
func WithTasks(tasks *TaskRegistry, controller TaskController) AgentOption {
	return func(a *Agent) error {
		if tasks == nil || controller == nil {
			return errors.New("tasks need both a registry and a controller")
		}
		a.tasks = tasks
		a.controller = controller
		return nil
	}
}

// NewAgent builds an agent from the parts it runs on. A nil config or registry
// means no system prompt and no tools. A model is required.
func NewAgent(config *AgentConfig, tools *ToolRegistry, llm LLM, options ...AgentOption) (*Agent, error) {
	if llm == nil {
		return nil, errors.New("an agent needs a model")
	}
	if config == nil {
		config = &AgentConfig{}
	}
	if tools == nil {
		tools = NewToolRegistry()
	}

	agent := &Agent{config: config, tools: tools, llm: llm}
	for _, option := range options {
		if err := option(agent); err != nil {
			return nil, err
		}
	}

	// The model picks by name and the agent routes by name, so one name
	// registered twice is a call that reaches whichever is checked first.
	for _, name := range agent.tasks.Names() {
		if _, taken := tools.Get(name); taken {
			return nil, fmt.Errorf("%s is registered as both a tool and a task", name)
		}
	}
	for _, name := range []string{CancelTaskTool, ProvideTaskInputTool} {
		if _, taken := tools.Get(name); taken && agent.tasks != nil {
			return nil, fmt.Errorf("%s is registered as a tool and is one of the tools tasks are addressed with", name)
		}
	}
	return agent, nil
}

type agentState struct {
	history *history.History
	arc *history.Arc
	foldState *history.FoldState
	persistor EventPersistor
}

// Run invokes the agent to complete an arc, i.e. a user message triggering the
// agent. The persistor is passed per run, so one agent serves as many sessions
// as it is handed.
func (a Agent) Run(ctx context.Context, persistor EventPersistor, h *history.History, arcID string) error {
	if persistor == nil {
		return errors.New("a run needs an event persistor")
	}

	var arc *history.Arc
	for _, a := range slices.Backward(h.Arcs) {
		if a.ID == arcID {
			arc = a
			break
		}
	}
	if arc == nil {
		return fmt.Errorf("arc not found in history: %s", arcID)
	}

	foldState := history.FoldStateFromHistory(h)

	state := &agentState{
		history: h,
		arc: arc,
		foldState: foldState,
		persistor: persistor,
	}

	if err := a.steps(ctx, state); err != nil {
		return a.recordFailure(ctx, err, state)
	}
	return nil
}

// recordFailure closes an arc the agent could not finish, and returns the
// error that ended it. An unrecorded failure leaves an arc the log cannot tell
// from one still being worked on.
//
// A cancelled run writes nothing, because whoever decided the cancel records
// it and the agent does not know that actor.
func (a Agent) recordFailure(ctx context.Context, cause error, state *agentState) error {
	// A cancel reaches a run through its context or through a refused write,
	// and neither is a failure to record.
	if errors.Is(cause, context.Canceled) || errors.Is(cause, ErrCancelled) {
		return cause
	}

	failure := &events.Event{
		Typ:           events.ErrorEventType,
		CorrelationID: &state.arc.ID,
		Payload:       events.ErrorPayload{Message: cause.Error(), IsFatal: true},
	}

	if _, err := a.persistAndFold(ctx, state, failure); err != nil {
		// Neither the cause nor the failure to record it is worth losing.
		return errors.Join(cause, err)
	}
	return cause
}

// steps runs the agent loop until it is the user's turn again.
func (a Agent) steps(ctx context.Context, state *agentState) error {
	done := false
	for !done {
		d, err := a.step(ctx, state)
		if err != nil {
			return err
		}
		done = d
	}

	return nil
}

// Step performs one step in the agent loop.
func (a Agent) step(ctx context.Context, state *agentState) (bool, error) {
	s, err := a.deriveArcState(state.arc)
	if err != nil {
		return true, err
	}
	switch s {
	case ArcStateLLM:
		return false, a.llmStep(ctx, state)
	case ArcStateToolCalls:
		return false, a.toolStep(ctx, state)
	case ArcStateClose:
		return false, a.closeArc(ctx, state)
	case ArcStateNone:
		return true, ErrInvalidArcState
	}
	return true, nil
}

func (a Agent) closeArc(ctx context.Context, state *agentState) error {
	// The fold rejects a close that names no message it knows.
	if len(state.arc.Responses) == 0 {
		return fmt.Errorf("cannot close arc %s: no response to name as its conclusion", state.arc.ID)
	}
	last := state.arc.Responses[len(state.arc.Responses)-1]

	_, err := a.persistAndFold(ctx, state, &events.Event{
		Typ: events.ArcClosedEventType,
		CorrelationID: &state.arc.ID,
		Payload: events.ArcClosedPayload{CausationID: last.ID},
	})
	return err
}

// persistAndFold writes one event and folds it into the history, returning the
// events as they were persisted. The persistor assigns the sequence number,
// and the ID and timestamp where the caller left them empty, so a caller
// naming one of these events later reads it from what comes back.
//
// Events passed together are written together or not at all, which is what
// keeps two events that say one thing from becoming one event that says
// something else.
func (a Agent) persistAndFold(ctx context.Context, state *agentState, evs ...*events.Event) ([]*events.Event, error) {
	// A write that starts work honours the cancel, because not starting it is
	// the point. Every other write records something that already happened,
	// and losing one leaves the log saying less than the agent did. One event
	// that starts work governs its whole batch, as it does for the guard.
	if _, starts := guardedArc(evs); !starts {
		ctx = context.WithoutCancel(ctx)
	}
	stored, err := state.persistor.Persist(ctx, evs...)
	if err != nil {
		return nil, fmt.Errorf("error persisting %s: %w", eventTypes(evs), err)
	}
	for _, ev := range stored {
		if err := state.history.Update(ev, state.foldState); err != nil {
			return nil, fmt.Errorf("error folding %s event: %w", ev.Typ, err)
		}
		// The observer is told after the fold, so it sees the history the next
		// step reads.
		if a.config.Observer != nil {
			if err := a.config.Observer.Consume(ev); err != nil {
				return nil, fmt.Errorf("error observing %s event: %w", ev.Typ, err)
			}
		}
	}
	return stored, nil
}

// eventTypes names a batch for an error message.
func eventTypes(evs []*events.Event) string {
	names := make([]string, 0, len(evs))
	for _, ev := range evs {
		names = append(names, string(ev.Typ))
	}
	return strings.Join(names, ", ")
}

// llmStep calls the LLM with the history so far and records what it replied.
func (a Agent) llmStep(ctx context.Context, state *agentState) error {
	tokSink := a.config.Tokens
	if tokSink == nil {
		tokSink = SinkFromFunc(func(string) error { return nil })
	}

	response, err := a.llm.StreamResponse(ctx, state.history, &LLMOptions{
		SystemPrompt: a.config.SystemPrompt,
		Tools:        CombineTools(a.tools, a.tasks),
	}, tokSink)
	if err != nil {
		return fmt.Errorf("error calling LLM: %w", err)
	}
	return a.recordResponse(ctx, response, state)
}

// recordResponse writes a reply to the log: the message and one event per call
// it requested, in one write.
//
// They go together because a log holding the message alone reads as an answer
// the model never gave, and the next run closes the arc on it. The message ID
// is minted here rather than read back, since the calls have to name it before
// any of them is written.
func (a Agent) recordResponse(ctx context.Context, response *LLMResponse, state *agentState) error {
	messageID := NewEventID()

	batch := make([]*events.Event, 0, len(response.ToolCalls)+1)
	batch = append(batch, &events.Event{
		ID:            messageID,
		Typ:           events.AssistantMessageEventType,
		CorrelationID: &state.arc.ID,
		Payload: events.AssistantMessagePayload{
			VendorID:  response.VendorID,
			ModelID:   response.ModelID,
			Content:   response.Content,
			Reasoning: response.Reasoning,
			Usage:     recordedUsage(response.Usage),
		},
	})
	for _, call := range response.ToolCalls {
		batch = append(batch, &events.Event{
			Typ:           events.ToolCallEventType,
			CorrelationID: &state.arc.ID,
			Payload: events.ToolCallPayload{
				MessageID: messageID,
				VendorID:  call.VendorID,
				ToolName:  call.ToolName,
				Args:      call.Args,
			},
		})
	}

	_, err := a.persistAndFold(ctx, state, batch...)
	return err
}

// recordedUsage carries a reply's cost into the record of it. Nothing is
// written where the provider reported nothing, so a zero in the log is a
// measured zero.
func recordedUsage(usage *TokenUsage) *events.TokenUsage {
	if usage == nil {
		return nil
	}
	return &events.TokenUsage{
		Input:       usage.Input,
		CachedInput: usage.CachedInput,
		Output:      usage.Output,
	}
}

// toolStep runs every call the arc is still waiting on, in the order they were
// requested. Every pending call it finds has to end settled, since the loop
// reads the arc to decide what to do next.
func (a Agent) toolStep(ctx context.Context, state *agentState) error {
	// Collected before any of them runs, because running one writes to the
	// arc being read.
	var pending []*history.ToolCall
	for _, response := range state.arc.Responses {
		for _, call := range response.ToolCalls {
			if call.Status == history.ToolCallStatusPending {
				pending = append(pending, call)
			}
		}
	}

	for _, call := range pending {
		// Re-read before each call, because a cancel recorded elsewhere
		// reaches this run through the fold.
		if state.arc.Status() != history.ArcStatusPending {
			return nil
		}

		// A call another run already dispatched may have taken effect. Only a
		// tool declared safe to repeat gets a second attempt.
		if call.Phase() == history.ToolCallPhaseDispatched && !a.tools.SafeToRepeat(call.ToolName) {
			if err := a.abandonToolCall(ctx, call, state); err != nil {
				return err
			}
			continue
		}

		if err := a.runCall(ctx, call, state); err != nil {
			return err
		}
	}
	return nil
}

// runCall hands one call to whatever answers calls by that name. The name
// decides what the call is before anything runs.
func (a Agent) runCall(ctx context.Context, call *history.ToolCall, state *agentState) error {
	switch a.tasks.classify(call.ToolName) {
	case opStart:
		return a.startTask(ctx, call, state)
	case opCancel:
		return a.cancelTask(ctx, call, state)
	case opProvideInput:
		return a.provideTaskInput(ctx, call, state)
	default:
		return a.runToolCall(ctx, call, state)
	}
}

// abandonToolCall settles a call that an earlier run dispatched and never
// reported on, without running it again. It is recorded as a failure, and the
// text says that the tool may have done the work anyway.
func (a Agent) abandonToolCall(ctx context.Context, call *history.ToolCall, state *agentState) error {
	return a.settleCall(ctx, call, ToolOutcome{
		IsError: true,
		Result: fmt.Sprintf(
			"this call was handed to %s by a run that stopped before reporting. "+
				"Whether it took effect is not known, and it was not tried again "+
				"because %s is not declared safe to repeat. Check the current state "+
				"before deciding what to do.", call.ToolName, call.ToolName),
	}, state)
}

// runToolCall records that a call was handed to its tool, runs it, and records
// what came back. The dispatch is written first, so a worker that dies
// mid-call leaves that record with no result behind it.
func (a Agent) runToolCall(ctx context.Context, call *history.ToolCall, state *agentState) error {
	if err := a.recordDispatch(ctx, call, state); err != nil {
		return err
	}

	// A handler that returned has said what happened, whatever the context is
	// doing by then, so the outcome is recorded even for a cancelled run.
	outcome, err := a.tools.Invoke(ctx, call.ToolName, call.Args)
	if err != nil {
		outcome = ToolOutcome{Result: err.Error(), IsError: true}
	}

	if err := a.settleCall(ctx, call, outcome, state); err != nil {
		return err
	}

	// The call is settled either way. The run is over only if someone stopped
	// it, and both can be true at once.
	return ctx.Err()
}

// deriveArcState decides what the agent does next from the arc alone, so a run
// picking up an arc another run left behind reaches the same decision.
func (a Agent) deriveArcState(arc *history.Arc) (arcState, error) {
	if arc.Status() != history.ArcStatusPending {
		return ArcStateDone, nil
	}
	if len(arc.Responses) == 0 {
		return ArcStateLLM, nil
	}

	for _, r := range arc.Responses {
		for _, tc := range r.ToolCalls {
			if tc.Status != history.ToolCallStatusPending {
				continue
			}
			if !a.knows(tc.ToolName) {
				return ArcStateNone, fmt.Errorf("tool not registered: %s", tc.ToolName)
			}
			return ArcStateToolCalls, nil
		}
	}

	last := arc.Responses[len(arc.Responses)-1]
	if len(last.ToolCalls) == 0 {
		// A task that spoke after the last response gets the model a turn.
		for _, task := range arc.AsyncTasks {
			if unreportedTaskNews(task, last.Seq) {
				return ArcStateLLM, nil
			}
		}

		// A task outlives the response that started it, so an arc holding one
		// is not finished however final that response reads.
		for _, task := range arc.AsyncTasks {
			if task.FinishedSeq == nil {
				return ArcStateDone, nil
			}
		}
		return ArcStateClose, nil
	}
	return ArcStateLLM, nil
}

// unreportedTaskNews reports whether a task has said something the arc's last
// response predates: a result nobody has accounted for, or a question still
// waiting on an answer. Both are decided by sequence number, so a turn already
// taken is not taken again.
//
// A task's own notifications do not count, since whether they reach the model
// at all is a projection setting.
func unreportedTaskNews(task *history.AsyncTask, respondedSeq int) bool {
	if task.FinishedSeq != nil && *task.FinishedSeq > respondedSeq {
		return true
	}
	// Only a question still outstanding is worth a turn. One that has been
	// answered is the task's to get on with, and the answer reaches the model
	// with whatever the task makes of it.
	return task.Status == history.AsyncTaskWaiting && lastInputRequestSeq(task) > respondedSeq
}

// lastInputRequestSeq is where the task last stopped to ask, or -1 for a task
// that never has.
func lastInputRequestSeq(task *history.AsyncTask) int {
	seq := -1
	for _, event := range task.Events {
		if event.Type == history.AsyncTaskEventInputRequested {
			seq = event.Seq
		}
	}
	return seq
}

// knows reports whether a call by this name has somewhere to go: a registered
// tool, a kind of task, or one of the two tools tasks are addressed with.
func (a Agent) knows(name string) bool {
	if a.tasks.classify(name) != opTool {
		return true
	}
	_, ok := a.tools.Get(name)
	return ok
}

// startTask records a task, starts it, and answers the call that asked for it.
//
// The dispatch and the invocation are written before the task starts, in one
// write, because the record is what names the task. The invocation ID is
// minted here so the task can be started under it.
func (a Agent) startTask(ctx context.Context, call *history.ToolCall, state *agentState) error {
	definition, ok := a.tasks.Get(call.ToolName)
	if !ok {
		return fmt.Errorf("task not registered: %s", call.ToolName)
	}

	invocationID := NewEventID()
	if _, err := a.persistAndFold(ctx, state,
		&events.Event{
			Typ:           events.ToolDispatchedEventType,
			CorrelationID: &state.arc.ID,
			Payload:       events.ToolDispatchedPayload{CausationID: call.ID},
		},
		&events.Event{
			ID:            invocationID,
			Typ:           events.AsyncTaskInvokeEventType,
			CorrelationID: &state.arc.ID,
			Payload:       events.AsyncTaskInvokePayload{TaskName: call.ToolName, Args: call.Args},
		},
	); err != nil {
		return err
	}

	ref := TaskRef{ArcID: state.arc.ID, ID: invocationID, Name: call.ToolName}
	if err := definition.Start(ctx, ref, call.Args); err != nil {
		return a.settleUnstartedTask(ctx, call, ref, err, state)
	}

	return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
		"task %s started, id %s. It runs on its own and reports back; you will be told what it "+
			"says. Answer it with %s and stop it with %s, both of which take that id.",
		ref.Name, ref.ID, ProvideTaskInputTool, CancelTaskTool)}, state)
}

// settleUnstartedTask closes off a task whose launch failed. The task and its
// call are settled as failed, since nothing will report on them, and the text
// says the work may be running anyway.
func (a Agent) settleUnstartedTask(ctx context.Context, call *history.ToolCall, ref TaskRef, cause error, state *agentState) error {
	reason := fmt.Sprintf(
		"task %s could not be started: %s. Whether it took hold before the failure is not known, "+
			"so check before starting it again.", ref.Name, cause)

	if _, err := a.persistAndFold(ctx, state, &events.Event{
		Typ:           events.AsyncTaskResultEventType,
		CorrelationID: &state.arc.ID,
		Payload: events.AsyncTaskResultPayload{
			CausationID: ref.ID,
			Result:      reason,
			Outcome:     events.AsyncTaskOutcomeFailed,
		},
	}); err != nil {
		return err
	}
	return a.settleCall(ctx, call, ToolOutcome{Result: reason, IsError: true}, state)
}

// cancelTask asks a running task to stop. It writes nothing about the task,
// since what became of work already under way is the runner's to report.
func (a Agent) cancelTask(ctx context.Context, call *history.ToolCall, state *agentState) error {
	if err := a.recordDispatch(ctx, call, state); err != nil {
		return err
	}

	task, failure := a.addressedTask(call, state)
	if failure != "" {
		return a.settleCall(ctx, call, ToolOutcome{Result: failure, IsError: true}, state)
	}
	if task.FinishedSeq != nil {
		return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
			"task %s had already finished (%s), so there was nothing to stop.",
			task.TaskName, task.Status), IsError: true}, state)
	}

	reason, _ := stringArg(call.Args, "reason")
	ref := TaskRef{ArcID: task.CorrelationID, ID: task.ID, Name: task.TaskName}
	if err := a.controller.Cancel(ctx, ref, reason); err != nil {
		return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
			"task %s could not be reached to stop it: %s", task.TaskName, err), IsError: true}, state)
	}

	return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
		"task %s was asked to stop. It reports what became of it, so it may have done some of "+
			"its work already.", task.TaskName)}, state)
}

// provideTaskInput hands a waiting task the answer it asked for. The task is
// given the input before the log is told, so the log cannot claim input the
// task never received.
func (a Agent) provideTaskInput(ctx context.Context, call *history.ToolCall, state *agentState) error {
	if err := a.recordDispatch(ctx, call, state); err != nil {
		return err
	}

	task, failure := a.addressedTask(call, state)
	if failure != "" {
		return a.settleCall(ctx, call, ToolOutcome{Result: failure, IsError: true}, state)
	}
	input, ok := stringArg(call.Args, "input")
	if !ok {
		return a.settleCall(ctx, call, ToolOutcome{
			Result: "no input was given to hand to the task", IsError: true}, state)
	}
	if task.Status != history.AsyncTaskWaiting {
		return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
			"task %s is %s and is not waiting for input.", task.TaskName, task.Status), IsError: true}, state)
	}

	ref := TaskRef{ArcID: task.CorrelationID, ID: task.ID, Name: task.TaskName}
	if err := a.controller.ProvideInput(ctx, ref, input); err != nil {
		return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
			"task %s could not be reached: %s", task.TaskName, err), IsError: true}, state)
	}

	// The record belongs to the arc that owns the task, so the arc waiting on
	// this answer is the one that reads it. It is the only event the agent
	// writes outside the arc it is working on.
	if _, err := a.persistAndFold(ctx, state, &events.Event{
		Typ:           events.AsyncTaskInputProvidedEventType,
		CorrelationID: &task.CorrelationID,
		Payload:       events.AsyncTaskInputProvidedPayload{CausationID: task.ID, Message: input},
	}); err != nil {
		return err
	}

	return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
		"task %s was given the input and is running again.", task.TaskName)}, state)
}

// addressedTask finds the task a call names, or says why it could not. The
// reason is written for the model to act on.
func (a Agent) addressedTask(call *history.ToolCall, state *agentState) (*history.AsyncTask, string) {
	id, ok := stringArg(call.Args, "task_id")
	if !ok {
		return nil, "no task id was given"
	}
	for _, arc := range state.history.Arcs {
		for _, task := range arc.AsyncTasks {
			if task.ID == id {
				return task, ""
			}
		}
	}
	// A task outside this conversation is as absent as one never started.
	return nil, fmt.Sprintf("no task with id %s is part of this conversation", id)
}

// recordDispatch says that a call was handed to whatever answers it, before
// the work starts.
func (a Agent) recordDispatch(ctx context.Context, call *history.ToolCall, state *agentState) error {
	_, err := a.persistAndFold(ctx, state, &events.Event{
		Typ:           events.ToolDispatchedEventType,
		CorrelationID: &state.arc.ID,
		Payload:       events.ToolDispatchedPayload{CausationID: call.ID},
	})
	return err
}

// settleCall answers one call with what came of it.
func (a Agent) settleCall(ctx context.Context, call *history.ToolCall, outcome ToolOutcome, state *agentState) error {
	_, err := a.persistAndFold(ctx, state, &events.Event{
		Typ:           events.ToolResultEventType,
		CorrelationID: &state.arc.ID,
		Payload: events.ToolResultPayload{
			CausationID: call.ID,
			Result:      outcome.Result,
			IsError:     outcome.IsError,
		},
	})
	return err
}

// stringArg reads one string argument out of a call's arguments, which the log
// holds as decoded JSON.
func stringArg(args any, key string) (string, bool) {
	fields, ok := args.(map[string]any)
	if !ok {
		return "", false
	}
	value, ok := fields[key].(string)
	if !ok || value == "" {
		return "", false
	}
	return value, true
}
