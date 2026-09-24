package main

import (
	"strings"
	"testing"

	"fillbrunn.ai/kube-agent/pkg/agent"
)

// A narrow terminal is the case that breaks a layout, because a style given a
// negative width panics rather than shrinking.
func TestTheViewSurvivesEveryWidth(t *testing.T) {
	shown := newTranscript()
	shown.blocks = append(shown.blocks,
		&block{kind: blockUser, author: "dev", text: strings.Repeat("a long question ", 8)},
		&block{kind: blockAssistant, text: strings.Repeat("a long answer ", 8)},
		&block{kind: blockTool, tool: "list_pods", args: `{"namespace":"kube-system"}`,
			state: toolDone, result: strings.Repeat(`{"pods":1}`, 12)},
		&block{kind: blockError, text: "the cluster is unreachable"},
	)

	for _, size := range []struct{ width, height int }{
		{20, 8}, {40, 14}, {96, 30}, {200, 60},
	} {
		llm := agent.NewOpenAI("test-key", "gpt-5")
		m := newModel(&session{}, &sender{}, llm, "/tmp/session.jsonl", shown).resize(size.width, size.height)
		if content := m.View().Content; content == "" {
			t.Errorf("empty view at %dx%d", size.width, size.height)
		}
	}
}

// An empty session says what to do rather than showing a blank screen.
func TestAnEmptyTranscriptInvites(t *testing.T) {
	rendered := renderTranscript(newTranscript(), 60)
	if !strings.Contains(rendered, "canned data") {
		t.Errorf("an empty transcript rendered as %q", rendered)
	}
}
