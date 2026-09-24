package history

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// PrintHistory writes a complete command-line view of history to w. Callers
// that want to print to the terminal should pass os.Stdout.
//
// Everything the history holds is printed, including records RenderChat leaves
// out. Use this to inspect a session; use RenderChat to read one.
func PrintHistory(w io.Writer, history *History) error {
	if history == nil {
		return fmt.Errorf("history must not be nil")
	}

	if _, err := fmt.Fprintf(w, "Session: %s\nLast sequence: %d\n\nChat\n", history.SessionID, history.LastSeq); err != nil {
		return err
	}
	for _, arc := range history.Arcs {
		if err := printArc(w, arc); err != nil {
			return err
		}
	}
	return nil
}

// printArc prints the user message that opened the arc followed by everything
// recorded under it. Nesting is what carries the correlation, so the records
// themselves no longer repeat the arc ID.
func printArc(w io.Writer, arc *Arc) error {
	if _, err := fmt.Fprintf(w, "\n[%d] %s %s\n  id: %s\n  cancelled: %v\n  actor: %s\n  content:\n%s\n",
		arc.Seq,
		formatTime(arc.Timestamp),
		MessageRoleUser,
		arc.ID,
		arc.Cancel != nil,
		formatValue(arc.Actor),
		indent(arc.Content, "    "),
	); err != nil {
		return err
	}
	for _, response := range arc.Responses {
		if err := printAssistantMessage(w, response); err != nil {
			return err
		}
	}
	if err := printErrors(w, arc.Errors); err != nil {
		return err
	}
	if err := printAsyncTasks(w, arc.AsyncTasks); err != nil {
		return err
	}
	return printCancel(w, arc.Cancel)
}

// printAssistantMessage prints one of the agent's replies and the tool calls
// it requested.
func printAssistantMessage(w io.Writer, message *AssistantMessage) error {
	// Reasoning is opaque bytes, so the dump reports that it is there and how
	// much of it rather than printing it.
	if _, err := fmt.Fprintf(w, "\n  [%d] %s %s\n    id: %s\n    vendor_id: %s\n    reasoning: %d bytes\n    content:\n%s\n",
		message.Seq,
		formatTime(message.Timestamp),
		MessageRoleAssistant,
		message.ID,
		formatString(message.VendorID),
		len(message.Reasoning),
		indent(message.Content, "      "),
	); err != nil {
		return err
	}
	for _, call := range message.ToolCalls {
		if err := printToolCall(w, call); err != nil {
			return err
		}
	}
	return nil
}

// printToolCall prints a call followed by the progress events it emitted.
func printToolCall(w io.Writer, call *ToolCall) error {
	_, err := fmt.Fprintf(w, "    tool call %s\n      id: %s\n      vendor_id: %s\n      args: %s\n      status: %s\n      phase: %s\n      dispatched_at: %s\n      dispatches: %d\n      result: %s\n      finished_at: %s\n",
		call.ToolName,
		call.ID,
		call.VendorID,
		formatValue(call.Args),
		call.Status,
		call.Phase(),
		formatTimePtr(call.DispatchedAt),
		call.Dispatches,
		formatValue(call.Result),
		formatTimePtr(call.FinishedAt),
	)
	if err != nil {
		return err
	}
	for _, event := range call.Events {
		if _, err := fmt.Fprintf(w, "      tool event %s: %s\n", event.ID, formatValue(event.Payload)); err != nil {
			return err
		}
	}
	return printInterrupt(w, call.Interrupt)
}

// printInterrupt prints the human decision a call was held up for. A call
// nobody was asked about prints nothing.
func printInterrupt(w io.Writer, interrupt *Interrupt) error {
	if interrupt == nil {
		return nil
	}
	_, err := fmt.Fprintf(w, "      interrupt %s\n        timestamp: %s\n        status: %s\n        reason: %s\n        context: %s\n        response: %s\n        responder: %s\n",
		interrupt.ID,
		formatTime(interrupt.Timestamp),
		interrupt.Status,
		interrupt.Reason,
		formatValue(interrupt.Context),
		formatValue(interrupt.Response),
		formatValue(interrupt.Responder),
	)
	return err
}

// printAsyncTasks prints an arc's tasks, each followed by its transcript. Arcs
// that launched none print nothing.
func printAsyncTasks(w io.Writer, tasks []*AsyncTask) error {
	if len(tasks) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\n  Async tasks"); err != nil {
		return err
	}
	for _, task := range tasks {
		if _, err := fmt.Fprintf(w, "\n    async task %s\n      id: %s\n      status: %s\n      args: %s\n      result: %s\n      finished_at: %s\n",
			task.TaskName,
			task.ID,
			task.Status,
			formatValue(task.Args),
			formatValue(task.Result),
			formatTimePtr(task.FinishedAt),
		); err != nil {
			return err
		}
		for _, event := range task.Events {
			if _, err := fmt.Fprintf(w, "      [%s] %s %s\n%s\n", formatTime(event.Timestamp), event.Type, event.ID, indent(formatValue(event.Payload), "        ")); err != nil {
				return err
			}
		}
	}
	return nil
}

// printErrors prints an arc's recorded failures. Arcs that hit none print
// nothing.
func printErrors(w io.Writer, errors []*Error) error {
	if len(errors) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\n  Errors"); err != nil {
		return err
	}
	for _, record := range errors {
		if _, err := fmt.Fprintf(w, "\n    error %s\n      sequence: %d\n      timestamp: %s\n      message: %s\n      is_fatal: %t\n      causation_id: %s\n",
			record.ID,
			record.Seq,
			formatTime(record.Timestamp),
			record.Message,
			record.IsFatal,
			formatString(record.CausationID),
		); err != nil {
			return err
		}
	}
	return nil
}

// printCancel prints the one cancel an arc can carry. Nothing is printed for
// an arc that ran to completion.
func printCancel(w io.Writer, cancel *Cancel) error {
	if cancel == nil {
		return nil
	}
	_, err := fmt.Fprintf(w, "\n  Cancel\n    id: %s\n    sequence: %d\n    timestamp: %s\n    actor: %s\n    reason: %s\n    terminated_work: %t\n",
		cancel.ID,
		cancel.Seq,
		formatTime(cancel.Timestamp),
		formatValue(cancel.Actor),
		formatString(cancel.Reason),
		cancel.TerminatedWork,
	)
	return err
}

// formatValue renders an arbitrary payload as JSON, falling back to Go syntax
// for values JSON cannot represent.
func formatValue(value any) string {
	encoded, err := json.Marshal(value)
	if err == nil {
		return string(encoded)
	}
	return fmt.Sprintf("%#v", value)
}

// formatString renders an optional string, printing "null" when it is unset.
func formatString(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

func formatTime(value time.Time) string {
	return value.Format(time.RFC3339Nano)
}

// formatTimePtr renders an optional timestamp, printing "null" when it is
// unset, as it is for work that has not finished.
func formatTimePtr(value *time.Time) string {
	if value == nil {
		return "null"
	}
	return formatTime(*value)
}

// indent prefixes every line of value, so multi-line content stays inside the
// block it is printed under.
func indent(value, prefix string) string {
	return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
}
