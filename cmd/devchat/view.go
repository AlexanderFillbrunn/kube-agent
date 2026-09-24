package main

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// Colors are ANSI palette indices rather than hex values, so the harness
// follows whatever theme the terminal is set to, as pkg/history does.
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("12")).
			Padding(0, 1)
	subtitleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			Padding(0, 1)
	authorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("10"))
	userStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.ThickBorder()).
			BorderLeft(true).
			BorderForeground(lipgloss.Color("10")).
			PaddingLeft(1)
	assistantStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.ThickBorder()).
			BorderLeft(true).
			BorderForeground(lipgloss.Color("12")).
			PaddingLeft(1)
	errorStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.ThickBorder()).
			BorderLeft(true).
			BorderForeground(lipgloss.Color("9")).
			PaddingLeft(1).
			Foreground(lipgloss.Color("9"))
	noteStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			Italic(true).
			PaddingLeft(2)
	toolNameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Padding(0, 1)
	inputStyle    = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("8")).
			Padding(0, 1)
	busyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
)

// toolMarks are the glyph and color each state is shown with. A queued call
// and a running one look different on purpose: the second may have taken
// effect and the first provably has not.
var toolMarks = map[toolState]struct {
	glyph string
	color string
}{
	toolQueued:  {"◌", "8"},
	toolRunning: {"◍", "11"},
	toolDone:    {"●", "10"},
	toolFailed:  {"●", "9"},
	toolUnknown: {"◑", "13"},
}

// renderTranscript draws the conversation to a given width.
func renderTranscript(t *transcript, width int) string {
	if len(t.blocks) == 0 {
		return dimStyle.Render("Ask about the cluster. The tools here answer from canned data.")
	}

	rendered := make([]string, 0, len(t.blocks))
	for _, b := range t.blocks {
		rendered = append(rendered, renderBlock(b, width))
	}
	return strings.Join(rendered, "\n\n")
}

func renderBlock(b *block, width int) string {
	// Two columns go to the left border and its padding, and one more is left
	// free so that a full line does not wrap on its own.
	body := max(width-3, 20)

	switch b.kind {
	case blockUser:
		text := authorStyle.Render(b.author) + "\n" + b.text
		return userStyle.Width(body).Render(text)

	case blockAssistant:
		return assistantStyle.Width(body).Render(b.text)

	case blockError:
		return errorStyle.Width(body).Render(b.text)

	case blockNote:
		return noteStyle.Width(body).Render(b.text)

	case blockTool:
		return renderTool(b, body)
	}
	return ""
}

// renderTool draws a call as two lines: what was asked for, and what came
// back. A call with no result yet shows only the first.
func renderTool(b *block, width int) string {
	mark := toolMarks[b.state]
	glyph := lipgloss.NewStyle().Foreground(lipgloss.Color(mark.color)).Render(mark.glyph)

	head := fmt.Sprintf("%s %s %s",
		glyph,
		toolNameStyle.Render(b.tool),
		dimStyle.Render(truncate(b.args, max(width-len(b.tool)-6, 10))),
	)
	if b.state == toolQueued || b.state == toolRunning {
		return head + " " + dimStyle.Render(string(b.state))
	}
	if b.state == toolUnknown {
		return head + "\n  " + dimStyle.Render("? stopped before it reported; effect unknown")
	}

	arrow := "→"
	if b.state == toolFailed {
		arrow = "✗"
	}
	return head + "\n  " + dimStyle.Render(arrow+" "+truncate(b.result, max(width-4, 10)))
}

// renderHeader draws the bar across the top.
func renderHeader(modelName, storePath string, width int) string {
	title := titleStyle.Render("kube-agent")
	subtitle := subtitleStyle.Render(modelName + " · " + storePath)

	bar := lipgloss.JoinHorizontal(lipgloss.Top, title, subtitle)
	if lipgloss.Width(bar) > width {
		bar = lipgloss.JoinHorizontal(lipgloss.Top, title, subtitleStyle.Render(modelName))
	}
	return bar
}
