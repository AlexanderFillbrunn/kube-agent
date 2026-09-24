package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// A line starting with a slash is an instruction to the harness rather than
// something to say to the model. Commands only run between turns: the guard
// that ignores enter while the agent is working covers them too, so nothing
// here has to reason about a run in flight.

// slashCommand is one thing that can be typed instead of a message.
type slashCommand struct {
	name string

	// arg names what the command takes, for the help line, and is empty for a
	// command that takes nothing.
	arg     string
	summary string

	// run is given whatever followed the command name, trimmed.
	run func(model, string) (model, tea.Cmd)
}

// commands are shown in the order they are listed.
var commands = []slashCommand{
	{
		name:    "/model",
		arg:     "<name>",
		summary: "switch model",
		run:     switchModel,
	},
	{
		name:    "/clear",
		summary: "new session",
		run:     clearSession,
	},
	{
		name:    "/quit",
		summary: "leave",
		run:     func(m model, _ string) (model, tea.Cmd) { return m, tea.Quit },
	},
}

// isCommand reports whether a line is addressed to the harness.
func isCommand(line string) bool {
	return strings.HasPrefix(line, "/")
}

// runCommand dispatches a typed line. An unknown command is answered rather
// than sent to the model, since a mistyped command is not a question.
func runCommand(m model, line string) (model, tea.Cmd) {
	name, argument, _ := strings.Cut(line, " ")
	for _, command := range commands {
		if command.name == name {
			return command.run(m, strings.TrimSpace(argument))
		}
	}

	m.transcript.note(fmt.Sprintf("no command %s. there is %s", name, strings.Join(commandNames(), ", ")))
	return m.refresh(), nil
}

// switchModel points the rest of the session at another model, and with no
// name says which one is being used.
//
// Nothing is checked here, because nothing can be: whether a name is a model
// this endpoint serves is only answered by asking it. A name that is not
// shows up as a failed turn.
//
// The conversation carries over. Every reply already records the model that
// produced it, so a session that changed models halfway stays readable as one.
func switchModel(m model, name string) (model, tea.Cmd) {
	if name == "" {
		m.transcript.note("using " + m.llm.Model)
		return m.refresh(), nil
	}
	if name == m.llm.Model {
		m.transcript.note("already using " + name)
		return m.refresh(), nil
	}

	was := m.llm.Model
	m.llm.Model = name
	m.transcript.note(fmt.Sprintf("switched from %s to %s", was, name))
	return m.refresh(), nil
}

// clearSession starts the conversation over.
//
// Clearing only the screen would be a lie: the next turn replays the log, so
// the model would go on remembering what the screen had forgotten. The log is
// moved aside instead, which is the only way the two agree.
func clearSession(m model, _ string) (model, tea.Cmd) {
	archived, err := m.session.store.Rotate()
	if err != nil {
		m.transcript.fail(err)
		return m.refresh(), nil
	}

	m.transcript = newTranscript()
	if archived == "" {
		m.transcript.note("nothing to clear")
	} else {
		m.transcript.note("new session. the previous one is at " + archived)
	}
	return m.refresh(), nil
}

func commandNames() []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.name)
	}
	return names
}

// commandHelp lists the commands for the line under the input.
func commandHelp() string {
	described := make([]string, 0, len(commands))
	for _, command := range commands {
		label := command.name
		if command.arg != "" {
			label += " " + command.arg
		}
		described = append(described, fmt.Sprintf("%s %s", label, command.summary))
	}
	return strings.Join(described, " · ")
}
