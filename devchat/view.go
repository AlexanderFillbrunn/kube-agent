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
	toolNameStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	taskBadgeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("0")).
			Background(lipgloss.Color("14")).
			Padding(0, 1)
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	helpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Padding(0, 1)
	inputStyle = lipgloss.NewStyle().
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
	toolWaiting: {"◔", "14"},
	toolStopped: {"◐", "8"},
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

	case blockTask:
		return renderTask(b, body)
	}
	return ""
}

// renderTask draws a task as a line that stays put and changes. A task reports
// several times over minutes, and a new line per report would push the
// conversation off the screen to say the same job is still going.
func renderTask(b *block, width int) string {
	mark := toolMarks[b.state]
	glyph := lipgloss.NewStyle().Foreground(lipgloss.Color(mark.color)).Render(mark.glyph)

	head := fmt.Sprintf("%s %s %s",
		glyph,
		toolNameStyle.Render("task "+b.tool),
		dimStyle.Render(string(b.state)),
	)
	return head + "\n  " + dimStyle.Render(truncate(b.text, max(width-4, 10)))
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

// renderHeader draws the bar across the top: what this is, what it is talking
// to, and a badge for every task still running. It stays one line, because the
// viewport is sized against its height.
func renderHeader(modelName string, tasks []string, width int) string {
	title := titleStyle.Render("kube-agent")
	details := subtitleStyle.Render(modelName)

	room := width - lipgloss.Width(title) - lipgloss.Width(details)
	if room < 0 {
		return lipgloss.JoinHorizontal(lipgloss.Top, title, details)
	}

	// Names where they fit, and where not even one does, the count: the least
	// a badge can say and still be worth the room.
	badges := namedBadges(tasks, room)
	if badges == "" {
		badges = summaryBadge(tasks, room)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, title, details, badges)
}

// namedBadges draws a badge per task, dropping them from the end until they
// fit and counting off whatever was dropped. It is empty when not even one
// fits. Badges are dropped rather than cut short, since cutting one by
// character would slice through its colour codes.
func namedBadges(tasks []string, room int) string {
	for count := len(tasks); count > 0; count-- {
		badges := renderBadges(tasks[:count])
		if dropped := len(tasks) - count; dropped > 0 {
			badges += " " + taskBadgeStyle.Render(fmt.Sprintf("+%d", dropped))
		}
		if lipgloss.Width(badges) <= room {
			return badges
		}
	}
	return ""
}

// summaryBadge says how many tasks are running, for when no name fits.
func summaryBadge(tasks []string, room int) string {
	if len(tasks) == 0 {
		return ""
	}
	badge := taskBadgeStyle.Render(fmt.Sprintf("%d %s", len(tasks), plural("task", len(tasks))))
	if lipgloss.Width(badge)+1 > room {
		return ""
	}
	return " " + badge
}

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func renderBadges(tasks []string) string {
	badges := make([]string, 0, len(tasks))
	for _, name := range tasks {
		badges = append(badges, taskBadgeStyle.Render(name))
	}
	return " " + strings.Join(badges, " ")
}
