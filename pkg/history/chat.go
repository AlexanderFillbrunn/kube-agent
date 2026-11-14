package history

import (
	"fmt"
	"strings"
	"time"
)

// RenderChat returns a plain-text rendering of the conversation and its tool
// activity, readable in any terminal and in a file.
//
// This is the reading view: interrupts, async tasks, errors and cancels are
// left out. PrintHistory shows those.
func (h *History) RenderChat() string {
	if h == nil {
		return ""
	}

	var sections []string
	sections = append(sections, fmt.Sprintf("Chat · session %s", h.SessionID))
	for _, arc := range h.Arcs {
		sections = append(sections, renderArc(arc))
	}
	return strings.Join(sections, "\n\n") + "\n"
}

// renderArc renders the user message that opened the arc followed by every
// response the agent gave under it.
func renderArc(arc *Arc) string {
	author := string(MessageRoleUser)
	if arc.Actor != nil {
		author = arc.Actor.UserID
	}

	parts := []string{renderBubble(author, arc.Timestamp, arc.Content)}
	for _, response := range arc.Responses {
		parts = append(parts, renderAssistantMessage(response))
	}
	return strings.Join(parts, "\n\n")
}

// renderAssistantMessage renders one reply followed by its tool calls. The
// message is attributed to the model that produced it where that is known.
func renderAssistantMessage(message *AssistantMessage) string {
	author := string(MessageRoleAssistant)
	if message.VendorID != nil {
		author = *message.VendorID
	}

	parts := []string{renderBubble(author, message.Timestamp, message.Content)}
	for _, call := range message.ToolCalls {
		parts = append(parts, renderToolCall(call))
	}
	return strings.Join(parts, "\n")
}

// renderBubble draws one message headed by its author and time.
func renderBubble(author string, timestamp time.Time, content string) string {
	return fmt.Sprintf("%s · %s\n%s", author, formatTime(timestamp), content)
}

// renderToolCall renders a call indented beneath the message that requested
// it. A pending call shows no result line, since it has none yet.
func renderToolCall(call *ToolCall) string {
	lines := []string{
		fmt.Sprintf("  ↳ %s %s", call.ToolName, renderToolStatus(call)),
		fmt.Sprintf("    args: %s", formatValue(call.Args)),
	}
	if call.Dispatches > 1 {
		lines = append(lines, fmt.Sprintf("    dispatched %d times, so the tool may have run more than once", call.Dispatches))
	}
	if call.Status != ToolCallStatusPending {
		lines = append(lines, fmt.Sprintf("    result: %s", formatValue(call.Result)))
	}
	for _, event := range call.Events {
		lines = append(lines, fmt.Sprintf("    event: %s", formatValue(event.Payload)))
	}
	return strings.Join(lines, "\n")
}

// renderToolStatus names an outcome. A call with no outcome yet shows its
// phase instead, since PENDING does not say whether the tool has been handed
// the call.
func renderToolStatus(call *ToolCall) string {
	if call.Status == ToolCallStatusPending {
		return string(call.Phase())
	}
	return string(call.Status)
}
