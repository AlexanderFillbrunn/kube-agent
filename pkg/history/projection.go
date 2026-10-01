package history

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// This file defines the provider-agnostic shape a History is projected into
// before it is sent to a model, and the projection that produces it. A History
// records what happened, including records a model has no place for, such as
// interrupts and cancels; a ModelTranscript records only what the model is
// told. PROJECTION.md specifies the rules this file implements.

// ModelMessageRole identifies the author of a message in a model transcript.
type ModelMessageRole string

const (
	ModelRoleUser      ModelMessageRole = "USER"
	ModelRoleAssistant ModelMessageRole = "ASSISTANT"
	// ModelRoleInstruction is a system or developer prompt rather than
	// anything a participant said.
	ModelRoleInstruction ModelMessageRole = "INSTRUCTION"
	// ModelRoleTool is the result of a tool call, fed back to the model as its
	// own message.
	ModelRoleTool ModelMessageRole = "TOOL"
	// ModelRoleNote marks synthesized status text that no participant said.
	// Which role carries it to a provider is the encoder's decision, so the
	// projection leaves it in a role of its own. See ENCODING.md.
	ModelRoleNote ModelMessageRole = "NOTE"
)

// ModelMessage is one entry in a model transcript. The unexported method keeps
// the set of implementations closed to this package, so a projection cannot
// produce a message shape providers do not understand.
type ModelMessage interface {
	Role() ModelMessageRole
	modelMessage()
}

// ModelTextMessage is plain text from the user or an instruction given to the
// model. MessageRole says which, so the two share a type.
type ModelTextMessage struct {
	MessageRole ModelMessageRole // user, system, or developer
	Content     string
}

// Role returns the role the message was constructed with.
func (m ModelTextMessage) Role() ModelMessageRole { return m.MessageRole }

func (ModelTextMessage) modelMessage() {}

// ModelAssistantMessage is a reply from the model, including any tool calls it
// requested. Content and ToolCalls can both be set: a model may say something
// and call a tool in the same message.
type ModelAssistantMessage struct {
	Content   string
	ToolCalls []ModelToolCall

	// Reasoning is the provider's record of how the model reached this
	// message, opaque to everything here, and is nil where the projection
	// could not carry it. It only ever belongs to the message it sits on.
	Reasoning []byte
}

// Role reports that the message came from the model.
func (ModelAssistantMessage) Role() ModelMessageRole { return ModelRoleAssistant }

func (ModelAssistantMessage) modelMessage() {}

// ModelToolOutcome is how a call ended, as the model is told it. A call that
// never ran ended for one of three different reasons, and the model acts
// differently on each, so they do not collapse into one error flag.
type ModelToolOutcome string

const (
	// ModelToolOutcomeSuccess is a call that ran and returned.
	ModelToolOutcomeSuccess ModelToolOutcome = "SUCCESS"
	// ModelToolOutcomeFailed is a call that ran and failed.
	ModelToolOutcomeFailed ModelToolOutcome = "FAILED"
	// ModelToolOutcomeBlocked is a call an automatic policy refused. Nobody
	// was asked, so there is nobody to go back to.
	ModelToolOutcomeBlocked ModelToolOutcome = "BLOCKED"
	// ModelToolOutcomeRefused is a call a human refused when asked to approve
	// it.
	ModelToolOutcomeRefused ModelToolOutcome = "REFUSED"
	// ModelToolOutcomeCancelled is a call that was still pending when its arc
	// was cancelled.
	ModelToolOutcomeCancelled ModelToolOutcome = "CANCELLED"
	// ModelToolOutcomePending is the synthesized stand-in for a call with no
	// result at the cutoff. The log says nothing about how this call ended,
	// and the projection made the message up so the call is answered.
	ModelToolOutcomePending ModelToolOutcome = "PENDING"
)

// Synthesized reports whether the projection invented this outcome rather than
// reading it from the log.
func (o ModelToolOutcome) Synthesized() bool { return o == ModelToolOutcomePending }

// ModelToolMessage returns the outcome of one tool call to the model.
type ModelToolMessage struct {
	// ToolCallID is the call this answers, matching the ID of the
	// ModelToolCall that requested it.
	ToolCallID string

	// Outcome is how the call ended. Content states the same thing in words,
	// for the model; Outcome is for the encoder and anything else reading the
	// transcript as data.
	Outcome ModelToolOutcome

	// Content is the tool's output, or what became of the call when it never
	// ran. Either way the model is told, so it can react.
	Content string

	// IsError is the flag providers carry on a tool result. Every outcome
	// other than success sets it.
	IsError bool
}

// Role reports that the message carries a tool result.
func (ModelToolMessage) Role() ModelMessageRole { return ModelRoleTool }

func (ModelToolMessage) modelMessage() {}

// ModelNoteMessage is synthesized status text: what the projection has to tell
// the model that no participant said. A note carries no role of its own.
type ModelNoteMessage struct {
	Content string
}

// Role reports that the message is a note rather than anything a participant
// said.
func (ModelNoteMessage) Role() ModelMessageRole { return ModelRoleNote }

func (ModelNoteMessage) modelMessage() {}

// ModelToolCall is one call requested by a ModelAssistantMessage.
type ModelToolCall struct {
	ID   string
	Name string
	Args any
}

// ModelTranscript is the conversation as the model sees it, in the order the
// messages are sent.
type ModelTranscript struct {
	Messages []ModelMessage
}

// Treatment is how much of one record a projection discloses. The values are
// ordered by how much they tell the model.
type Treatment int

const (
	// TreatmentElide drops the record.
	TreatmentElide Treatment = iota
	// TreatmentNote reduces the record to a synthesized note.
	TreatmentNote
	// TreatmentStandIn replaces the record with a synthesized message of the
	// same shape.
	TreatmentStandIn
	// TreatmentKeep projects the record in full.
	TreatmentKeep
)

// String names the treatment as PROJECTION.md writes it.
func (t Treatment) String() string {
	switch t {
	case TreatmentElide:
		return "elide"
	case TreatmentNote:
		return "note"
	case TreatmentStandIn:
		return "stand_in"
	case TreatmentKeep:
		return "keep"
	}
	return fmt.Sprintf("Treatment(%d)", int(t))
}

// Modes is the session-wide decision about how much of each record kind to
// disclose. Modes are fixed for a session, which is what makes a projection a
// pure function of the log and the arc.
type Modes struct {
	// ForeignArc governs an arc other than the one being projected that the
	// log does not show as closed: still open, dead of a fatal error, or
	// withdrawn before anything was attempted. An arc that closed normally is
	// kept as ordinary conversation history.
	ForeignArc Treatment // keep | note | elide

	// Pending governs tool calls with no result at the cutoff.
	Pending Treatment // stand_in | note | elide

	// ToolEvents governs progress notifications emitted by a running call.
	ToolEvents Treatment // note | elide

	// AsyncEvents governs notifications a running task emitted.
	AsyncEvents Treatment // note | elide

	// AsyncInput governs a task stopping to ask a human for input.
	AsyncInput Treatment // note | elide

	// Error governs a non-fatal error the arc survived.
	Error Treatment // note | elide

	// Interrupt governs an approval that was granted. A refusal is a fixed
	// rule and ignores this.
	Interrupt Treatment // note | elide

	// QuoteLimit is how much of a request a note may quote, in characters.
	// A note names the request it is about so the model can tell which one is
	// meant, which takes less than the whole of it. 0 quotes it whole.
	//
	// It is not a treatment: the modes above decide whether a record is
	// disclosed, and this decides how much of one a note spells out.
	QuoteLimit int
}

// DefaultQuoteLimit is how much of a request a note quotes unless a session
// says otherwise.
const DefaultQuoteLimit = 32

// DefaultModes returns the defaults PROJECTION.md marks with `*`.
func DefaultModes() Modes {
	return Modes{
		ForeignArc:  TreatmentKeep,
		Pending:     TreatmentStandIn,
		ToolEvents:  TreatmentElide,
		AsyncEvents: TreatmentNote,
		AsyncInput:  TreatmentNote,
		Error:       TreatmentElide,
		Interrupt:   TreatmentElide,
		QuoteLimit:  DefaultQuoteLimit,
	}
}

// Validate reports whether every mode holds a value that mode allows. A mode
// set outside its own vocabulary is a programming error rather than a
// condition to project around.
func (m Modes) Validate() error {
	const noteOrElide = 1<<TreatmentNote | 1<<TreatmentElide
	for _, check := range []struct {
		name    string
		value   Treatment
		allowed uint
	}{
		{"foreign_arc", m.ForeignArc, 1<<TreatmentKeep | noteOrElide},
		{"pending", m.Pending, 1<<TreatmentStandIn | noteOrElide},
		{"tool_events", m.ToolEvents, noteOrElide},
		{"async_events", m.AsyncEvents, noteOrElide},
		{"async_input", m.AsyncInput, noteOrElide},
		{"error", m.Error, noteOrElide},
		{"interrupt", m.Interrupt, noteOrElide},
	} {
		if check.allowed&(1<<check.value) == 0 {
			return fmt.Errorf("mode %s cannot be %s", check.name, check.value)
		}
	}
	if m.QuoteLimit < 0 {
		return fmt.Errorf("quote_limit cannot be negative, got %d", m.QuoteLimit)
	}
	return nil
}

// Project builds the transcript for one arc. The history must be the one the
// read model produced for this arc: every event up to the arc's own user
// message, plus every later event correlated to it. That query is the cutoff.
func (h *History) Project(arcID string, modes Modes) (*ModelTranscript, error) {
	if h == nil {
		return nil, errors.New("history must not be nil")
	}
	if err := modes.Validate(); err != nil {
		return nil, err
	}

	target := -1
	for i, arc := range slices.Backward(h.Arcs) {
		if arc.ID == arcID {
			target = i
			break
		}
	}
	if target < 0 {
		return nil, fmt.Errorf("no arc with ID=%s in history", arcID)
	}

	projector := newProjector(h, h.Arcs[target], modes)
	projector.run()
	return &ModelTranscript{Messages: projector.messages()}, nil
}

// unit is one emission, keyed by where it belongs in the log. A unit holds
// every message that has to stay contiguous, which is how a call and its
// result travel together and how a note is kept from landing between them.
type unit struct {
	seq      int
	sub      int
	messages []ModelMessage
}

// projector holds one projection in progress.
type projector struct {
	history *History
	target  *Arc
	modes   Modes

	units []unit
}

func newProjector(history *History, target *Arc, modes Modes) *projector {
	return &projector{history: history, target: target, modes: modes}
}

// run walks the arcs the history holds, once, and emits what survives. A
// foreign arc's records all precede the target's user message, and the target
// owns everything correlated to it.
func (p *projector) run() {
	for _, arc := range p.history.Arcs {
		p.projectArc(arc)
	}
}

// messages returns the emitted messages in log order. Units that share a
// sequence number keep the order they were emitted in.
func (p *projector) messages() []ModelMessage {
	sort.SliceStable(p.units, func(i, j int) bool {
		if p.units[i].seq != p.units[j].seq {
			return p.units[i].seq < p.units[j].seq
		}
		return p.units[i].sub < p.units[j].sub
	})
	messages := make([]ModelMessage, 0, len(p.units))
	for _, u := range p.units {
		messages = append(messages, u.messages...)
	}
	return messages
}

// emit places messages at a position in the log. sub orders several emissions
// that belong to the same position, so a note attached to a record lands after
// it.
func (p *projector) emit(seq, sub int, messages ...ModelMessage) {
	if len(messages) == 0 {
		return
	}
	p.units = append(p.units, unit{seq: seq, sub: sub, messages: messages})
}

// emitNote places one synthesized note.
func (p *projector) emitNote(seq, sub int, format string, args ...any) {
	p.emit(seq, sub, ModelNoteMessage{Content: fmt.Sprintf(format, args...)})
}

// arcPlan is what a projection decided about one arc as a whole.
type arcPlan struct {
	// material is how much of the arc's own records reaches the transcript.
	material Treatment
	// status names the note the arc needs, or none.
	status arcNote
	// statusSeq is where that note belongs, which is where the record that
	// created the status was written.
	statusSeq int
}

// arcNote names the kind of status a foreign arc needs reported.
type arcNote int

const (
	arcNoteNone arcNote = iota
	arcNoteInflight
	arcNoteDead
	arcNoteWithdrawn
	arcNoteStopped
)

// projectArc emits one arc under the mode that governs it.
func (p *projector) projectArc(arc *Arc) {
	plan := planArc(arc, p.modes, arc.ID == p.target.ID)

	switch plan.material {
	case TreatmentElide:
		return
	case TreatmentNote:
		p.emitArcNote(arc, plan)
		return
	}

	p.keepArc(arc)

	// The note goes with the material, including when the material is nothing
	// but the user message. A bare user turn reads as a request addressed to
	// this turn, which a model would answer under the wrong arc.
	if plan.status != arcNoteNone {
		p.emitArcNote(arc, plan)
	}
}

// quote renders a request for a note: one line, cut to the length the session
// allows. A request that was cut ends in an ellipsis, so it cannot be read as
// one that was always that short.
func (p *projector) quote(content string) string {
	folded := strings.Join(strings.Fields(content), " ")
	if p.modes.QuoteLimit <= 0 || len([]rune(folded)) <= p.modes.QuoteLimit {
		return folded
	}
	return strings.TrimRight(string([]rune(folded)[:p.modes.QuoteLimit]), " ") + "…"
}

// planArc decides how much of an arc reaches the transcript and which status
// note it needs.
func planArc(arc *Arc, modes Modes, isTarget bool) arcPlan {
	plan := arcPlan{material: TreatmentKeep, statusSeq: arc.Seq}

	fatal := fatalError(arc)
	status := arc.Status()
	switch {
	case isTarget:
		// The arc being projected is never foreign to itself.
	case fatal != nil:
		plan.material = modes.ForeignArc
		plan.status = arcNoteDead
		plan.statusSeq = fatal.Seq
	case status == ArcStatusCancelled:
		// The cancel closed the arc, so it stopped either work in progress or
		// a request nothing had been done about yet. A cancel that reached
		// only an async task lands under Done instead.
		plan.statusSeq = arc.Cancel.Seq
		if len(arc.Responses) > 0 {
			// Fixed rule: a cancel that stopped work in progress is always
			// disclosed, together with the work it stopped.
			plan.status = arcNoteStopped
		} else {
			plan.material = modes.ForeignArc
			plan.status = arcNoteWithdrawn
		}
	case status == ArcStatusDone:
		// A closed arc is ordinary conversation history. A cancel that arrived
		// too late lands here, because it stopped nothing the arc was doing.
	default:
		plan.material = modes.ForeignArc
		plan.status = arcNoteInflight
		plan.statusSeq = lastResponseSeq(arc)
	}
	return plan
}

// keepArc emits an arc's own records.
func (p *projector) keepArc(arc *Arc) {
	p.emit(arc.Seq, 0, ModelTextMessage{MessageRole: ModelRoleUser, Content: arc.Content})

	// An arc that was cancelled or died is not coming back, which changes what
	// an unanswered call under it means: not "no result yet" but "no result,
	// and none is coming".
	stoppedSeq, stopped := arcEnd(arc)
	for _, message := range arc.Responses {
		p.projectResponse(message, stopped)
	}
	p.projectErrors(arc)
	p.projectAsyncTasks(arc, stoppedSeq, stopped)
}

// projectResponse emits one assistant message with its calls, their results
// and any notes its calls produced.
func (p *projector) projectResponse(message *AssistantMessage, arcStopped bool) {
	assistant := ModelAssistantMessage{Content: message.Content}
	var results []ModelMessage
	var notes []ModelMessage

	for _, call := range message.ToolCalls {
		p.projectInterrupt(call)
		if call.FinishedSeq == nil {
			switch p.modes.Pending {
			case TreatmentStandIn:
				assistant.ToolCalls = append(assistant.ToolCalls, modelToolCall(call))
				results = append(results, ModelToolMessage{
					ToolCallID: call.ID,
					Outcome:    ModelToolOutcomePending,
					Content:    pendingStandIn(call, arcStopped),
					IsError:    false,
				})
			case TreatmentNote:
				notes = append(notes, ModelNoteMessage{
					Content: fmt.Sprintf("[a call to %s has not returned yet]", call.ToolName),
				})
			case TreatmentElide:
				// The call goes with the result it never got. Leaving the
				// request in place would put an unanswered call in front of
				// the provider.
			}
			continue
		}

		assistant.ToolCalls = append(assistant.ToolCalls, modelToolCall(call))
		results = append(results, toolResult(call))

		if p.modes.ToolEvents == TreatmentNote {
			for _, event := range call.Events {
				notes = append(notes, ModelNoteMessage{
					Content: fmt.Sprintf("[%s reported: %s]", call.ToolName, formatValue(event.Payload)),
				})
			}
		}
	}

	// A provider that takes reasoning back validates it against the message it
	// is sent with. A message that lost a call to a mode is no longer that
	// message, so its reasoning is dropped.
	if len(assistant.ToolCalls) == len(message.ToolCalls) {
		assistant.Reasoning = message.Reasoning
	}

	// Phase 1: an assistant message left with no text and no surviving calls
	// is dropped. Notes its calls produced are not part of the message and
	// survive it.
	if assistant.Content != "" || len(assistant.ToolCalls) > 0 {
		messages := make([]ModelMessage, 0, len(results)+1)
		messages = append(messages, assistant)
		messages = append(messages, results...)
		p.emit(message.Seq, 0, messages...)
	}
	for _, note := range notes {
		p.emit(message.Seq, 1, note)
	}
}

// pendingStandIn says why a call has no result. The four reasons are not
// interchangeable, since the model acts differently on each.
//
// arcStopped says the arc is over, so nothing further is coming.
func pendingStandIn(call *ToolCall, arcStopped bool) string {
	if interrupt := call.Interrupt; interrupt != nil && interrupt.ResolvedSeq == nil {
		return "[this call is waiting for a human to approve it]"
	}
	if arcStopped {
		if call.DispatchedSeq != nil {
			// The one thing the log cannot say. Claiming either that it ran or
			// that it did not would be inventing the answer, and a model told
			// it did not run is a model that runs it again.
			return "[this call was handed to its tool and then stopped before it reported. Whether it took effect is not known.]"
		}
		return "[this call was never made: the request it belonged to ended first]"
	}
	return "[this call has not returned yet]"
}

// projectErrors emits the non-fatal errors an arc survived. The fatal one is
// the arc's status and is reported by planArc.
func (p *projector) projectErrors(arc *Arc) {
	for _, failure := range arc.Errors {
		if failure.IsFatal || p.modes.Error != TreatmentNote {
			continue
		}
		if failure.CausationID != nil {
			p.emitNote(failure.Seq, 0, "[an error was recorded while working on this: %s]", failure.Message)
			continue
		}
		p.emitNote(failure.Seq, 0, "[an error was recorded: %s]", failure.Message)
	}
}

// projectInterrupt emits the notes for the human decision a call was held up
// for. A refusal is not among them: it settled the call, so it reaches the
// model as that call's result.
//
// Each note sits where its event happened, and the two can be far apart, so
// the decision names the interrupt rather than relying on position.
func (p *projector) projectInterrupt(call *ToolCall) {
	interrupt := call.Interrupt
	if interrupt == nil || p.modes.Interrupt != TreatmentNote {
		return
	}

	// The ask, built from the interrupt alone. It says nothing about the
	// outcome, so every transcript that contains it contains the same bytes
	// and a decision arriving later leaves the cached prefix intact.
	p.emit(interrupt.Seq, 0, ModelNoteMessage{Content: askNote(interrupt)})
	if interrupt.ResolvedSeq == nil {
		// Nothing came back. The ask is what tells the model it is waiting on
		// a human.
		return
	}
	if interrupt.Status == InterruptApproved {
		// An approval leaves no mark on the call, which runs as if nobody had
		// been asked, so a note is the only place it can be told.
		p.emitNote(*interrupt.ResolvedSeq, 0, "[interrupt %s: a human approved it]", interrupt.ID)
	}
}

// askNote renders the note an interrupt gets when it is raised. It reads only
// fields a raised interrupt already has, so the note cannot change once the
// decision arrives.
func askNote(interrupt *Interrupt) string {
	note := fmt.Sprintf("[interrupt %s: a human was asked to approve this call before it runs", interrupt.ID)
	if interrupt.Reason != "" {
		note += ": " + interrupt.Reason
	}
	if interrupt.Context != nil {
		// What the human was shown. The model is told the same thing, so it
		// can account for a refusal without guessing what was on the screen.
		note += ". They were shown: " + formatValue(interrupt.Context)
	}
	return note + "]"
}

// projectAsyncTasks emits the two notes every task gets, plus whatever its
// transcript is allowed to say.
// arcEnd reports where an arc stopped and whether it stopped at all. An arc
// that closed normally did not stop: it finished.
func arcEnd(arc *Arc) (int, bool) {
	switch arc.Status() {
	case ArcStatusCancelled:
		return arc.Cancel.Seq, true
	case ArcStatusError:
		if fatal := fatalError(arc); fatal != nil {
			return fatal.Seq, true
		}
	}
	return 0, false
}

// taskLabel names a task in a note. The ID travels with the name because a
// note is the only place the model learns it, and the tools that address a
// task take an ID. It is written whole, since it is what the model hands
// back.
func taskLabel(task *AsyncTask) string {
	return fmt.Sprintf("%s (id %s)", task.TaskName, task.ID)
}

func (p *projector) projectAsyncTasks(arc *Arc, stoppedSeq int, arcStopped bool) {
	for _, task := range arc.AsyncTasks {
		// Fixed rule: a task is always two notes, one at invocation and one
		// at completion.
		p.emitNote(task.Seq, 0, "[task %s started: %s]", taskLabel(task), formatValue(task.Args))

		for _, event := range task.Events {
			switch event.Type {
			case AsyncTaskEventNotification:
				if p.modes.AsyncEvents == TreatmentNote {
					p.emitNote(event.Seq, 0, "[task %s reported: %s]", taskLabel(task), formatValue(event.Payload))
				}
			case AsyncTaskEventInputRequested:
				if p.modes.AsyncInput == TreatmentNote {
					p.emitNote(event.Seq, 0, "[task %s is waiting for input: %s]", taskLabel(task), resultText(event.Payload))
				}
			case AsyncTaskEventInputProvided:
				if p.modes.AsyncInput == TreatmentNote {
					p.emitNote(event.Seq, 0, "[task %s was given input: %s]", taskLabel(task), resultText(event.Payload))
				}
			}
		}

		if task.FinishedSeq == nil {
			// A task always gets its second note. An arc that is over will
			// produce no result, so the note says what the log knows: the
			// task was running and nobody reported what became of it.
			if arcStopped {
				// The note belongs where the arc ended, which is after
				// everything the task managed to report.
				p.emitNote(stoppedSeq, 1,
					"[task %s was still running when this request was stopped. Whether it finished is not known.]",
					taskLabel(task))
			}
			continue
		}
		switch task.Status {
		case AsyncTaskFailed:
			p.emitNote(*task.FinishedSeq, 1, "[task %s failed: %s]", taskLabel(task), resultText(task.Result))
		case AsyncTaskCancelled:
			p.emitNote(*task.FinishedSeq, 1, "[task %s was cancelled before it finished]", taskLabel(task))
		default:
			p.emitNote(*task.FinishedSeq, 1, "[task %s finished: %s]", taskLabel(task), resultText(task.Result))
		}
	}
}

// emitArcNote reports a foreign arc's status.
func (p *projector) emitArcNote(arc *Arc, plan arcPlan) {
	switch plan.status {
	case arcNoteInflight:
		p.emitNote(plan.statusSeq, 1,
			"[another request is still being worked on: %q. It is not yours to answer and its result is not available yet.]",
			p.quote(arc.Content))
	case arcNoteDead:
		p.emitNote(plan.statusSeq, 1,
			"[an earlier request, %q, failed and was never answered.]", p.quote(arc.Content))
	case arcNoteWithdrawn:
		p.emitNote(plan.statusSeq, 1,
			"[an earlier request, %q, was withdrawn before it was acted on.]", p.quote(arc.Content))
	case arcNoteStopped:
		// The note must not guess at motive. Cancel.Reason is optional free
		// text, so without it the log records that a human stopped this and
		// not why.
		if reason := cancelReason(arc.Cancel); reason != "" {
			p.emitNote(plan.statusSeq, 1,
				"[a human stopped this request while it was running: %s. The approach above did not finish.]", reason)
			return
		}
		p.emitNote(plan.statusSeq, 1,
			"[a human stopped this request while it was running. The approach above did not finish.]")
	}
}

// fatalError returns the failure that closed the arc, or nil.
func fatalError(arc *Arc) *Error {
	for _, failure := range arc.Errors {
		if failure.IsFatal {
			return failure
		}
	}
	return nil
}

// lastResponseSeq returns where an open arc's status note belongs: after the
// last thing it said, which is where its unanswered calls are.
func lastResponseSeq(arc *Arc) int {
	seq := arc.Seq
	for _, response := range arc.Responses {
		if response.Seq > seq {
			seq = response.Seq
		}
	}
	return seq
}

// modelToolCall carries a call to the model under the log's own ID. The
// provider's identifier for it is ToolCall.VendorID and belongs to the
// encoder.
func modelToolCall(call *ToolCall) ModelToolCall {
	return ModelToolCall{ID: call.ID, Name: call.ToolName, Args: call.Args}
}

// toolResult renders a settled call's outcome. Every terminal status is
// reported, because a failure the model is not told about is a failure it
// repeats.
//
// A call that never ran says who stopped it, since a policy, a human and a
// cancel are three different things to the model.
func toolResult(call *ToolCall) ModelToolMessage {
	message := ModelToolMessage{
		ToolCallID: call.ID,
		Outcome:    ModelToolOutcomeSuccess,
		Content:    resultText(call.Result),
	}
	switch call.Status {
	case ToolCallStatusFailed:
		message.Outcome = ModelToolOutcomeFailed
	case ToolCallStatusBlocked:
		message.Outcome = ModelToolOutcomeBlocked
		message.Content = refusalText("a policy refused this call, without asking anyone", message.Content)
	case ToolCallStatusRefused:
		message.Outcome = ModelToolOutcomeRefused
		message.Content = refusalText("a human refused this call", message.Content)
	case ToolCallStatusCancelled:
		// A cancel only settles a call its tool never saw, so this one
		// provably never ran. A call that was already dispatched is still
		// pending and is reported by pendingStandIn instead.
		message.Outcome = ModelToolOutcomeCancelled
		message.Content = refusalText("this call was cancelled before it ran", message.Content)
	}
	message.IsError = message.Outcome != ModelToolOutcomeSuccess
	return message
}

// refusalText states who stopped a call, and what they said about it where the
// log has that.
func refusalText(by, detail string) string {
	if detail == "" {
		return fmt.Sprintf("[%s]", by)
	}
	return fmt.Sprintf("[%s: %s]", by, detail)
}

// cancelReason returns the reason a human gave, or an empty string when the
// log recorded none.
func cancelReason(cancel *Cancel) string {
	if cancel == nil || cancel.Reason == nil {
		return ""
	}
	return *cancel.Reason
}

// resultText renders a payload as text for the model. Strings are passed
// through, because a tool that returned text should not reach the model as a
// JSON string literal.
func resultText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case *string:
		if typed == nil {
			return ""
		}
		return *typed
	}
	return formatValue(value)
}
