package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/events"
)

// Messages carrying a run into the interface. A run happens on its own
// goroutine and reports through the program, which is the one way into the
// update loop that is safe from outside it.
type (
	tokenMsg    string
	eventMsg    struct{ event *events.Event }
	turnDoneMsg struct{ err error }
)

// sender hands messages to the program. It exists because the sinks have to be
// built before the program they report to: the model is passed to NewProgram,
// so nothing can hold the program until after that call.
type sender struct {
	mu      sync.RWMutex
	program *tea.Program
}

func (s *sender) attach(program *tea.Program) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.program = program
}

func (s *sender) send(msg tea.Msg) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.program != nil {
		s.program.Send(msg)
	}
}

type model struct {
	session    *session
	transcript *transcript
	sender     *sender

	viewport viewport.Model
	input    textarea.Model
	spinner  spinner.Model

	// llm is held so that /model can point the session at another one. It is
	// only ever changed between turns.
	llm       *agent.OpenAI
	storePath string

	// cancel stops the run in progress, and is nil when nothing is running.
	cancel context.CancelFunc

	// arc names what the run in progress is working on, and is empty when
	// nothing is running. A cancel is recorded against an arc, so the name has
	// to be to hand before the run has written anything to read it from.
	arc string

	width  int
	height int
	ready  bool
}

func newModel(s *session, sender *sender, llm *agent.OpenAI, storePath string, existing *transcript) model {
	input := textarea.New()
	input.Placeholder = "Ask about the cluster…"
	input.ShowLineNumbers = false
	input.CharLimit = 4000
	input.SetHeight(2)
	input.Focus()

	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(busyStyle))

	return model{
		session:    s,
		transcript: existing,
		sender:     sender,
		input:      input,
		spinner:    spin,
		llm:        llm,
		storePath:  storePath,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.input.Focus(), m.spinner.Tick)
}

func (m model) running() bool { return m.cancel != nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.resize(msg.Width, msg.Height), nil

	case tea.KeyPressMsg:
		return m.key(msg)

	case tokenMsg:
		m.transcript.token(string(msg))
		return m.refresh(), nil

	case eventMsg:
		m.transcript.event(msg.event)
		return m.refresh(), nil

	case turnDoneMsg:
		m.cancel = nil
		m.arc = ""
		switch {
		case msg.err == nil:
		case errors.Is(msg.err, context.Canceled), errors.Is(msg.err, agent.ErrCancelled):
			// The cancel is already written down, so the screen only has to
			// say the run is over rather than repeat it as a failure. A run
			// the guard refused a write to reads the same way to whoever asked
			// for it, and arrives as ErrCancelled instead.
			m.transcript.note("stopped")
		default:
			m.transcript.fail(msg.err)
		}
		return m.refresh(), nil
	}

	var commands []tea.Cmd
	var command tea.Cmd

	m.spinner, command = m.spinner.Update(msg)
	commands = append(commands, command)
	m.input, command = m.input.Update(msg)
	commands = append(commands, command)
	m.viewport, command = m.viewport.Update(msg)
	commands = append(commands, command)

	return m, tea.Batch(commands...)
}

func (m model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		// While a run is in flight, the interrupt belongs to the run: the
		// agent stops where it is and the log keeps what it had written. It
		// goes the same way as /cancel, since it is the same decision and the
		// two must not leave the log in different states.
		if m.running() {
			return cancelTurn(m, "")
		}
		return m, tea.Quit

	case "esc":
		if m.running() {
			return m, nil
		}
		return m, tea.Quit

	case "enter":
		message := strings.TrimSpace(m.input.Value())
		if message == "" {
			return m, nil
		}
		if isCommand(message) {
			// A command that is not written for a run in flight is left in the
			// input rather than dropped, so it can be sent once the turn is
			// over instead of typed again.
			if m.running() && !runsDuringTurn(message) {
				return m, nil
			}
			m.input.Reset()
			return runCommand(m, message)
		}
		if m.running() {
			return m, nil
		}
		m.input.Reset()
		return m.start(message)
	}

	var command tea.Cmd
	m.input, command = m.input.Update(msg)
	return m, command
}

// start runs one turn on its own goroutine. Everything it produces comes back
// as a message, so the update loop stays the only thing touching the model.
func (m model) start(message string) (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.arc = m.session.newArcID()

	session := m.session
	sender := m.sender
	arc := m.arc
	go func() {
		defer cancel()
		sender.send(turnDoneMsg{err: session.turn(ctx, arc, message)})
	}()

	// The user's own message is shown without waiting for the log, so that
	// pressing enter does something immediately.
	m.transcript.blocks = append(m.transcript.blocks, &block{
		kind:   blockUser,
		author: m.session.actor.UserID,
		text:   message,
	})
	return m.refresh(), m.spinner.Tick
}

func (m model) resize(width, height int) model {
	m.width, m.height = width, height

	inputHeight := lipgloss.Height(m.inputView())
	chrome := lipgloss.Height(renderHeader(m.llm.Model, m.storePath, width)) + inputHeight + 3

	if !m.ready {
		m.viewport = viewport.New(viewport.WithWidth(width), viewport.WithHeight(max(height-chrome, 3)))
		m.ready = true
	} else {
		m.viewport.SetWidth(width)
		m.viewport.SetHeight(max(height-chrome, 3))
	}
	m.input.SetWidth(width - 4)
	return m.refresh()
}

// refresh redraws the transcript and follows it, so a reply being typed stays
// in view.
func (m model) refresh() model {
	if !m.ready {
		return m
	}
	content := renderTranscript(m.transcript, m.viewport.Width())

	// A short conversation is pushed to the bottom, so the newest line is
	// always in the same place rather than drifting down the screen as the
	// session grows.
	if gap := m.viewport.Height() - lipgloss.Height(content); gap > 0 {
		content = strings.Repeat("\n", gap) + content
	}

	m.viewport.SetContent(content)
	m.viewport.GotoBottom()
	return m
}

func (m model) View() tea.View {
	if !m.ready {
		view := tea.NewView("starting…")
		view.AltScreen = true
		return view
	}

	view := tea.NewView(lipgloss.JoinVertical(lipgloss.Left,
		renderHeader(m.llm.Model, m.storePath, m.width),
		"",
		m.viewport.View(),
		"",
		m.inputView(),
		m.helpView(),
	))
	view.AltScreen = true
	view.WindowTitle = "kube-agent · devchat"
	return view
}

func (m model) inputView() string {
	return inputStyle.Width(max(m.width, 22)).Render(m.input.View())
}

func (m model) helpView() string {
	if m.running() {
		return helpStyle.Render(fmt.Sprintf("%s working · /cancel [why] or ctrl+c stops it", m.spinner.View()))
	}
	return helpStyle.Render(commandHelp() + " · enter send")
}
