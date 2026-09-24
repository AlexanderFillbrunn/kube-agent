package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

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

	// Tokens receives the model's text as it streams. A nil sink discards it:
	// the reply is recorded whole whatever happens here, so a run nobody is
	// watching loses nothing.
	Tokens Sink[string]

	// Observer is handed every event once it has been folded, so that a caller
	// can follow a run as it happens rather than read it afterwards. It only
	// watches: nothing it does changes what the agent decides, and an observer
	// that fails stops the run rather than being ignored.
	Observer Sink[*events.Event]
}

type Agent struct {
	config *AgentConfig
	tools *ToolRegistry
	llm LLM
}

// NewAgent builds an agent from the parts it runs on. A nil config or registry
// is an agent with no system prompt and no tools, which is a usable agent; a
// missing model is not, since it has no sane default.
func NewAgent(config *AgentConfig, tools *ToolRegistry, llm LLM) (*Agent, error) {
	if llm == nil {
		return nil, errors.New("an agent needs a model")
	}
	if config == nil {
		config = &AgentConfig{}
	}
	if tools == nil {
		tools = NewToolRegistry()
	}
	return &Agent{config: config, tools: tools, llm: llm}, nil
}

type agentState struct {
	history *history.History
	arc *history.Arc
	foldState *history.FoldState
	persistor EventPersistor
}

// Run invokes the agent to complete an arc, i.e. a user message triggering the
// agent.
//
// The log to write to is named per run rather than per agent, because it is
// the conversation that belongs to a log and not the agent: one agent answers
// for as many sessions as it is handed, and what a run records lands wherever
// the caller filed the user message that opened the arc.
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
// error that ended it.
//
// A run that fails without writing anything leaves an arc the log cannot tell
// from one still being worked on, and every later request is projected against
// that: the model is told an answer is coming that never will. So a failure is
// a record like any other. A deadline is one of these, since nobody withdrew
// the work; it ran out of time.
//
// A cancelled run writes nothing. Cancelling is not the agent's decision to
// make or to report: whoever decided it records it and then stops the run,
// and an agent writing its own cancel would be claiming an authority it does
// not have and an actor it does not know.
func (a Agent) recordFailure(ctx context.Context, cause error, state *agentState) error {
	// A cancel reaches a run two ways — the context it was given, and a write
	// the store would not take — and neither is a failure to record. The arc
	// is already closed by the cancel that stopped it.
	if errors.Is(cause, context.Canceled) || errors.Is(cause, ErrCancelled) {
		return cause
	}

	failure := &events.Event{
		Typ:           events.ErrorEventType,
		CorrelationID: &state.arc.ID,
		Payload:       events.ErrorPayload{Message: cause.Error(), IsFatal: true},
	}

	// The context that ended the run cannot be the one that records it: a
	// cancelled context would refuse the very write that says so.
	if _, err := a.persistAndFold(context.WithoutCancel(ctx), failure, state); err != nil {
		// The cause is what went wrong; that it could not be written down is
		// a second failure, and neither is worth losing.
		return errors.Join(cause, err)
	}
	return cause
}

// steps runs the agent loop until it is the user#s turn again.
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
	// The close names the response that concluded the arc, and the fold
	// rejects a close naming no message it knows.
	if len(state.arc.Responses) == 0 {
		return fmt.Errorf("cannot close arc %s: no response to name as its conclusion", state.arc.ID)
	}
	last := state.arc.Responses[len(state.arc.Responses)-1]

	_, err := a.persistAndFold(ctx, &events.Event{
		Typ: events.ArcClosedEventType,
		CorrelationID: &state.arc.ID,
		Payload: events.ArcClosedPayload{CausationID: last.ID},
	}, state)
	return err
}

// persistAndFold writes one event and folds it into the history, returning the
// event as it was persisted. The persistor assigns the ID, sequence number and
// timestamp, so a caller that has to name this event in a later one reads them
// from what comes back rather than inventing them.
func (a Agent) persistAndFold(ctx context.Context, ev *events.Event, state *agentState) (*events.Event, error) {
	newEv, err := state.persistor.Persist(ctx, ev)
	if err != nil {
		return nil, fmt.Errorf("error persisting %s event: %w", ev.Typ, err)
	}
	if err := state.history.Update(newEv, state.foldState); err != nil {
		return nil, fmt.Errorf("error folding %s event: %w", newEv.Typ, err)
	}
	// The observer is told after the fold, so that what it sees and what the
	// next step reads are the same history.
	if a.config.Observer != nil {
		if err := a.config.Observer.Consume(newEv); err != nil {
			return nil, fmt.Errorf("error observing %s event: %w", newEv.Typ, err)
		}
	}
	return newEv, nil
}

// llmStep calls the LLM with the history so far and records what it replied.
func (a Agent) llmStep(ctx context.Context, state *agentState) error {
	tokSink := a.config.Tokens
	if tokSink == nil {
		tokSink = SinkFromFunc(func(string) error { return nil })
	}

	response, err := a.llm.StreamResponse(ctx, state.history, &LLMOptions{
		SystemPrompt: a.config.SystemPrompt,
		Tools:        a.tools,
	}, tokSink)
	if err != nil {
		return fmt.Errorf("error calling LLM: %w", err)
	}
	return a.recordResponse(ctx, response, state)
}

// recordResponse writes a reply to the log: the message first, then one event
// per call it requested, each naming the message the persistor just gave an ID
// to.
func (a Agent) recordResponse(ctx context.Context, response *LLMResponse, state *agentState) error {
	message, err := a.persistAndFold(ctx, &events.Event{
		Typ:           events.AssistantMessageEventType,
		CorrelationID: &state.arc.ID,
		Payload: events.AssistantMessagePayload{
			VendorID:  response.VendorID,
			ModelID:   response.ModelID,
			Content:   response.Content,
			Reasoning: response.Reasoning,
			Usage:     recordedUsage(response.Usage),
		},
	}, state)
	if err != nil {
		return err
	}

	if err := a.recordToolCalls(ctx, response.ToolCalls, message.ID, state); err != nil {
		return err
	}
	return nil
}

// recordedUsage carries a reply's cost into the record of it. Nothing is
// written where the provider reported nothing, so a zero in the log always
// means a measured zero.
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

// recordToolCalls writes the calls one message requested, each naming it.
func (a Agent) recordToolCalls(ctx context.Context, calls []LLMToolCall, messageID string, state *agentState) error {
	for _, call := range calls {
		_, err := a.persistAndFold(ctx, &events.Event{
			Typ:           events.ToolCallEventType,
			CorrelationID: &state.arc.ID,
			Payload: events.ToolCallPayload{
				MessageID: messageID,
				VendorID:  call.VendorID,
				ToolName:  call.ToolName,
				Args:      call.Args,
			},
		}, state)
		if err != nil {
			return err
		}
	}
	return nil
}

// toolStep runs every call the arc is still waiting on, in the order they were
// requested.
//
// Every pending call it finds has to end settled. The loop decides what to do
// next by reading the arc, so a call left pending is a call the agent asks
// itself about again, and again.
func (a Agent) toolStep(ctx context.Context, state *agentState) error {
	// The calls are collected before any of them runs, because running one
	// writes to the arc being read.
	var pending []*history.ToolCall
	for _, response := range state.arc.Responses {
		for _, call := range response.ToolCalls {
			if call.Status == history.ToolCallStatusPending {
				pending = append(pending, call)
			}
		}
	}

	for _, call := range pending {
		// The arc is re-read before each call rather than trusted from the
		// slice, because a cancel recorded elsewhere reaches this run through
		// the fold, and a run that has been stopped must not start the next
		// tool.
		if state.arc.Status() != history.ArcStatusPending {
			return nil
		}

		// A call another run already handed to its tool may have taken effect,
		// and running it again would be a second attempt at work that might
		// have succeeded the first time. Only a tool that says repeating it is
		// harmless gets a second attempt; the rest are settled as abandoned.
		if call.Phase() == history.ToolCallPhaseDispatched && !a.tools.SafeToRepeat(call.ToolName) {
			if err := a.abandonToolCall(ctx, call, state); err != nil {
				return err
			}
			continue
		}

		if err := a.runToolCall(ctx, call, state); err != nil {
			return err
		}
	}
	return nil
}

// abandonToolCall settles a call that an earlier run dispatched and never
// reported on, without running it again.
//
// It is recorded as a failure because it did not succeed, and the text says
// the part the status cannot: that the tool may have done the work anyway.
// Leaving the call pending instead would be honest and unworkable — the loop
// reads the arc to decide what to do next, so a call nobody settles is one the
// agent asks itself about forever.
func (a Agent) abandonToolCall(ctx context.Context, call *history.ToolCall, state *agentState) error {
	_, err := a.persistAndFold(ctx, &events.Event{
		Typ:           events.ToolResultEventType,
		CorrelationID: &state.arc.ID,
		Payload: events.ToolResultPayload{
			CausationID: call.ID,
			IsError:     true,
			Result: fmt.Sprintf(
				"this call was handed to %s by a run that stopped before reporting. "+
					"Whether it took effect is not known, and it was not tried again "+
					"because %s is not declared safe to repeat. Check the current state "+
					"before deciding what to do.", call.ToolName, call.ToolName),
		},
	}, state)
	return err
}

// runToolCall records that a call was handed to its tool, runs it, and records
// what came back.
//
// The dispatch is written first and on purpose: a worker that dies mid-call
// leaves that record with no result behind it, which is how the next run tells
// a call that may have taken effect from one that provably never ran.
func (a Agent) runToolCall(ctx context.Context, call *history.ToolCall, state *agentState) error {
	if _, err := a.persistAndFold(ctx, &events.Event{
		Typ:           events.ToolDispatchedEventType,
		CorrelationID: &state.arc.ID,
		Payload:       events.ToolDispatchedPayload{CausationID: call.ID},
	}, state); err != nil {
		return err
	}

	// A tool call is synchronous, so a handler that returned has said what
	// happened, whatever it returned and whatever the context is doing by
	// then. Dropping that because the run was cancelled would leave a call
	// looking like one nobody ever heard back from, which is the one thing
	// the log reserves for a tool that never reported at all.
	outcome, err := a.tools.Invoke(ctx, call.ToolName, call.Args)
	if err != nil {
		outcome = ToolOutcome{Result: err.Error(), IsError: true}
	}

	if _, err := a.persistAndFold(ctx, &events.Event{
		Typ:           events.ToolResultEventType,
		CorrelationID: &state.arc.ID,
		Payload: events.ToolResultPayload{
			CausationID: call.ID,
			Result:      outcome.Result,
			IsError:     outcome.IsError,
		},
	}, state); err != nil {
		return err
	}

	// The call is settled either way; the run is over only if someone stopped
	// it. Both are true at once when a cancel interrupted the tool, and both
	// have to be reported.
	return ctx.Err()
}

// deriveArcState decides what the agent does next by reading the arc alone, so
// a run that picks up an arc another run left behind reaches the same decision.
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
			if _, ok := a.tools.Get(tc.ToolName); !ok {
				return ArcStateNone, fmt.Errorf("tool not registered: %s", tc.ToolName)
			}
			return ArcStateToolCalls, nil
		}
	}

	last := arc.Responses[len(arc.Responses)-1]
	if len(last.ToolCalls) == 0 {
		// A task outlives the response that started it, so an arc holding one
		// is not finished however final that response reads. Closing here
		// would leave the model unable to account for work it started, and the
		// task's own result is what starts the next run.
		for _, task := range arc.AsyncTasks {
			if task.FinishedSeq == nil {
				return ArcStateDone, nil
			}
		}
		return ArcStateClose, nil
	}
	return ArcStateLLM, nil
}
