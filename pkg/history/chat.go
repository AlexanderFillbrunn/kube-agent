package history

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// Colors are ANSI palette indices rather than hex values, so the rendering
// follows whatever theme the user's terminal is set to.
var (
	chatTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12"))
	userMessageStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("10")).
				Padding(0, 1)
	assistantMessageStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("12")).
				Padding(0, 1)
	metadataStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))
	toolStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("14"))
	pendingStatusStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("11"))
	failedStatusStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("9"))
	successStatusStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("10"))
	blockedStatusStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("13"))
	cancelledStatusStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8"))
)

// RenderChat returns a colored terminal rendering of the conversation and its
// tool activity. Lip Gloss automatically selects an appropriate color profile
// for the user's terminal.
//
// This is the reading view: interrupts, async tasks, errors and cancels are
// left out. PrintHistory shows those.
func (h *History) RenderChat() string {
	if h == nil {
		return ""
	}

	var sections []string
	sections = append(sections, chatTitleStyle.Render(fmt.Sprintf("Chat · session %s", h.SessionID)))
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

	parts := []string{renderBubble(userMessageStyle, author, arc.Timestamp, arc.Content)}
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

	parts := []string{renderBubble(assistantMessageStyle, author, message.Timestamp, message.Content)}
	for _, call := range message.ToolCalls {
		parts = append(parts, renderToolCall(call))
	}
	return strings.Join(parts, "\n")
}

// renderBubble draws one message as a bordered block headed by its author and
// time.
func renderBubble(style lipgloss.Style, author string, timestamp time.Time, content string) string {
	header := fmt.Sprintf("%s · %s", author, formatTime(timestamp))
	return style.Render(lipgloss.JoinVertical(lipgloss.Left, header, content))
}

// renderToolCall renders a call indented beneath the message that requested
// it. A pending call shows no result line, since it has none yet.
func renderToolCall(call *ToolCall) string {
	lines := []string{
		toolStyle.Render(fmt.Sprintf("  ↳ %s %s", call.ToolName, renderToolStatus(call))),
		metadataStyle.Render(fmt.Sprintf("    args: %s", formatValue(call.Args))),
	}
	if call.Dispatches > 1 {
		lines = append(lines, metadataStyle.Render(fmt.Sprintf("    dispatched %d times, so the tool may have run more than once", call.Dispatches)))
	}
	if call.Status != ToolCallStatusPending {
		lines = append(lines, metadataStyle.Render(fmt.Sprintf("    result: %s", formatValue(call.Result))))
	}
	for _, event := range call.Events {
		lines = append(lines, metadataStyle.Render(fmt.Sprintf("    event: %s", formatValue(event.Payload))))
	}
	return strings.Join(lines, "\n")
}

// renderToolStatus colors an outcome so it is distinguishable at a glance. A
// call with no outcome yet shows its phase instead, since PENDING does not say
// whether the tool has been handed the call.
func renderToolStatus(call *ToolCall) string {
	style := pendingStatusStyle
	switch call.Status {
	case ToolCallStatusPending:
		return style.Render(string(call.Phase()))
	case ToolCallStatusFailed:
		style = failedStatusStyle
	case ToolCallStatusSuccess:
		style = successStatusStyle
	case ToolCallStatusBlocked:
		style = blockedStatusStyle
	case ToolCallStatusCancelled:
		style = cancelledStatusStyle
	}
	return style.Render(string(call.Status))
}
