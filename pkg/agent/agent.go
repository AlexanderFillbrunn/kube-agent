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
	ArcStateLLM       arcState = iota
	ArcStateToolCalls arcState = iota
	ArcStateDone      arcState = iota
	ArcStateClose     arcState = iota
)

type Token struct {
	MessageID string
	Content   string
	Seq       int
}

// AgentConfig is what an agent is built with, and holds nothing that differs
// from one run to the next. Following a run is not here: notification hangs
// off the persistor, so an agent has no idea whether anybody is watching. See
// EventSink and docs/EVENT-SINK.md.
type AgentConfig struct {
	SystemPrompt string
}

type Agent struct {
	config *AgentConfig
	tools  ToolProvider
	llm    LLM

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

// NewAgent builds an agent from the parts it runs on. A nil config or tool
// provider means no system prompt and an empty registry. A model is required.
func NewAgent(config *AgentConfig, tools ToolProvider, llm LLM, options ...AgentOption) (*Agent, error) {
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

// RunOptions are the parts of a run that differ from one run to the next. The
// agent is built once and holds none of them, so one agent serves as many
// sessions as it is handed.
type RunOptions struct {
	// Persistor is where the run writes.
	Persistor EventPersistor

	// AgentID names the agent this run runs under. The run stamps nothing
	// with it — that is the persistor's wrapper's job — but the tasks it
	// starts carry it in their refs, so a report hands the arc back to the
	// agent that started it.
	AgentID string

	// History is the arc's log, already folded. The session it names is what
	// the run's own events are filed under.
	History *history.History

	// ArcID names the arc in that history the run is to work.
	ArcID string

	// Tokens follows the model's text as it streams, labelled with the message
	// that will record it. It is nil for a run nobody is watching, which is
	// every run of a worker with no interface attached, and a watcher's error
	// is dropped either way.
	Tokens Sink[Token]
}

type agentState struct {
	history   *history.History
	arc       *history.Arc
	foldState *history.FoldState
	persistor EventPersistor
	tokens    Sink[Token]
	agentID   string
}

// ref names the arc the run is working, for anything that has to address it
// from outside this process.
func (s *agentState) ref() ArcRef {
	return ArcRef{SessionID: s.history.SessionID, ArcID: s.arc.ID}
}

// Run invokes the agent to complete an arc, i.e. a user message triggering the
// agent.
func (a Agent) Run(ctx context.Context, run RunOptions) error {
	if run.Persistor == nil {
		return errors.New("a run needs an event persistor")
	}
	if run.History == nil {
		return errors.New("a run needs a history")
	}
	if run.History.SessionID == "" {
		return errors.New("a run needs a history that names its session")
	}
	if run.AgentID == "" {
		return errors.New("a run needs the agent it runs under")
	}

	var arc *history.Arc
	for _, candidate := range slices.Backward(run.History.Arcs) {
		if candidate.ID == run.ArcID {
			arc = candidate
			break
		}
	}
	if arc == nil {
		return fmt.Errorf("arc not found in history: %s", run.ArcID)
	}

	state := &agentState{
		history:   run.History,
		arc:       arc,
		foldState: history.FoldStateFromHistory(run.History),
		persistor: run.Persistor,
		tokens:    run.Tokens,
		agentID:   run.AgentID,
	}

	// A cancelled arc has no agent work left, but the tasks it started might
	// still be running, so closing here.
	if state.arc.Cancel != nil {
		return a.stopArcTasks(ctx, state)
	}

	if err := a.steps(ctx, state); err != nil {
		return a.recordFailure(ctx, err, state)
	}
	return nil
}

// stopArcTasks asks the tasks a cancelled arc started to stop.
//
// It writes nothing. The cancel ends the waiting, and what became of work
// already under way is the runner's to report, so the log learns the outcome
// when the task says what it was.
//
// A task that could not be reached fails the run, so whoever handed the arc
// over comes back with it. Asking a task to stop twice is harmless, which is
// what makes coming back safe.
func (a Agent) stopArcTasks(ctx context.Context, state *agentState) error {
	if a.controller == nil {
		return nil
	}

	reason := ""
	if state.arc.Cancel != nil && state.arc.Cancel.Reason != nil {
		reason = *state.arc.Cancel.Reason
	}

	var unreached []error
	for _, task := range state.arc.AsyncTasks {
		if task.FinishedSeq != nil {
			continue
		}
		ref := TaskRef{ArcRef: state.ref(), ID: task.ID, Name: task.TaskName, AgentID: state.agentID}
		if err := a.controller.Cancel(ctx, ref, reason); err != nil {
			unreached = append(unreached, fmt.Errorf("stop task %s: %w", task.TaskName, err))
		}
	}
	return errors.Join(unreached...)
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
		Typ:           events.ArcClosedEventType,
		CorrelationID: &state.arc.ID,
		Payload:       events.ArcClosedPayload{CausationID: last.ID},
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

	// Every writer names the session it writes for, and the agent's is the
	// history it was given. Nothing further down the chain invents one.
	for _, ev := range evs {
		ev.SessionID = state.history.SessionID
	}

	stored, err := state.persistor.Persist(ctx, evs...)
	if err != nil {
		return nil, fmt.Errorf("error persisting %s: %w", eventTypes(evs), err)
	}
	for _, ev := range stored {
		if err := state.history.Update(ev, state.foldState); err != nil {
			return nil, fmt.Errorf("error folding %s event: %w", ev.Typ, err)
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
	// The message is named before the model is called rather than when the
	// reply is written, because the text being streamed has to say which
	// message will record it.
	messageID := NewEventID()

	response, err := a.llm.StreamResponse(ctx, state.history, &LLMOptions{
		SystemPrompt: a.config.SystemPrompt,
		Tools:        CombineTools(a.tools, a.tasks),
	}, state.tokenSink(messageID))
	if err != nil {
		return fmt.Errorf("error calling LLM: %w", err)
	}
	return a.recordResponse(ctx, response, messageID, state)
}

// tokenSink labels the model's text with the message that will record it. A
// run nobody is watching discards it, and the watcher's error is dropped
// rather than returned: a screen cannot fail a run.
//
// The provider is handed a plain Sink[string], so no LLM implementation learns
// what an arc or a message is.
func (s *agentState) tokenSink(messageID string) Sink[RawToken] {
	return SinkFromFunc(func(tok RawToken) error {
		if s.tokens != nil {
			_ = s.tokens.Consume(Token{MessageID: messageID, Content: tok.Content, Seq: tok.Seq})
		}
		return nil
	})
}

// recordResponse writes a reply to the log: the message and one event per call
// it requested, in one write.
//
// They go together because a log holding the message alone reads as an answer
// the model never gave, and the next run closes the arc on it. The message ID
// is the one llmStep minted before the call rather than one read back, since
// the calls have to name it before any of them is written and the tokens had
// to name it before that.
func (a Agent) recordResponse(ctx context.Context, response *LLMResponse, messageID string, state *agentState) error {
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
		// A call another run already dispatched may have taken effect. Only a
		// tool declared safe to repeat gets a second attempt.
		tool, ok := a.tools.Get(call.ToolName)
		if call.Phase() == history.ToolCallPhaseDispatched && !(ok && SafeToRepeat(tool)) {
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
	if request := lastInputRequest(task); request != nil {
		return request.Seq
	}
	return -1
}

// lastInputRequest is the question the task last stopped at, or nil for a task
// that never has. A waiting task always has one, since asking is what put it
// in that state.
func lastInputRequest(task *history.AsyncTask) *history.AsyncTaskEvent {
	var last *history.AsyncTaskEvent
	for i := range task.Events {
		if task.Events[i].Type == history.AsyncTaskEventInputRequested {
			last = &task.Events[i]
		}
	}
	return last
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

	ref := TaskRef{ArcRef: state.ref(), ID: invocationID, Name: call.ToolName, AgentID: state.agentID}
	if err := definition.Start(ctx, ref, call.Args); err != nil {
		return a.settleUnstartedTask(ctx, call, ref, err, state)
	}

	return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
		"task %s started, id %s. It runs on its own, so say what you have and end your turn. "+
			"Anything it says reaches you as a turn of its own, a question included, so there is "+
			"nothing here to wait for and nothing to poll. Answer a question of its with %s once "+
			"you have been given one, and stop it with %s. Both take that id.",
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
	ref := TaskRef{
		ArcRef:  ArcRef{SessionID: state.history.SessionID, ArcID: task.CorrelationID},
		ID:      task.ID,
		Name:    task.TaskName,
		AgentID: state.agentID,
	}
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
	// A run reads the log as it stood when the run began, so a question asked
	// since is not here yet and this says so rather than claiming the task is
	// not asking. Answering ahead of the question is the other way in.
	if task.Status != history.AsyncTaskWaiting {
		return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
			"task %s has asked nothing you have been given to answer, and reads as %s here. Wait "+
				"to be handed its question rather than answering one you expect it to ask.",
			task.TaskName, task.Status), IsError: true}, state)
	}

	// The answer names the question it answers, so a task that has moved on
	// refuses it rather than taking it for whatever it is waiting on now.
	request := lastInputRequest(task)
	if request == nil {
		return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
			"task %s is waiting, but the log holds no question of its to answer.",
			task.TaskName), IsError: true}, state)
	}

	ref := TaskRef{
		SessionID: state.history.SessionID,
		ArcID:     task.CorrelationID,
		ID:        task.ID,
		Name:      task.TaskName,
		AgentID:   state.agentID,
	}
	// A task somebody else answered first refuses this answer, and a task that
	// cannot be reached at all fails the same way. Nothing here tells the two
	// apart, so the text covers both.
	if err := a.controller.ProvideInput(ctx, ref, request.ID, input); err != nil {
		return a.settleCall(ctx, call, ToolOutcome{Result: fmt.Sprintf(
			"task %s did not take the input: %s. It may have been answered already, or have "+
				"finished, so read what it says next before answering again.",
			task.TaskName, err), IsError: true}, state)
	}

	// Both events say the same already-settled thing, that the input reached
	// the task, so neither may survive without the other.
	//
	// The record of the input belongs to the arc that owns the task, so the
	// arc waiting on this answer is the one that reads it. It is the only
	// event the agent writes outside the arc it is working on, and so the
	// only write that needs the persistor to be atomic across two arcs.
	_, err := a.persistAndFold(ctx, state,
		&events.Event{
			Typ:           events.AsyncTaskInputProvidedEventType,
			CorrelationID: &task.CorrelationID,
			Payload:       events.AsyncTaskInputProvidedPayload{CausationID: task.ID, Message: input},
		},
		callResult(call, ToolOutcome{Result: fmt.Sprintf(
			"task %s was given the input and is running again.", task.TaskName)}, state),
	)
	return err
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
	_, err := a.persistAndFold(ctx, state, callResult(call, outcome, state))
	return err
}

// callResult is the record of a call having been answered, for a caller that
// writes it alongside something else.
func callResult(call *history.ToolCall, outcome ToolOutcome, state *agentState) *events.Event {
	return &events.Event{
		Typ:           events.ToolResultEventType,
		CorrelationID: &state.arc.ID,
		Payload: events.ToolResultPayload{
			CausationID: call.ID,
			Result:      outcome.Result,
			IsError:     outcome.IsError,
		},
	}
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
