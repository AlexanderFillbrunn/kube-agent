package history

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/users"
)

func TestPrintHistoryIncludesAllHistoryRecords(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	vendorID := "vendor-message"
	reason := "user stopped the run"
	causationID := "tool-1"
	actor := &users.Actor{UserID: "user-1", TenantID: "tenant-1"}

	history := &History{
		SessionID: "session-1",
		LastSeq:   9,
		Arcs: []*Arc{{
			ID: "arc-1", Seq: 1, Timestamp: now, Content: "Hello\nthere", Actor: actor,
			Responses: []*AssistantMessage{{
				ID: "message-1", Seq: 2, CorrelationID: "arc-1", Timestamp: now,
				Content: "Looking it up.", VendorID: &vendorID,
				ToolCalls: []*ToolCall{{
					ID: "tool-1", VendorID: "vendor-tool", ToolName: "lookup", Args: map[string]any{"query": "status"},
					Result: "healthy", Status: ToolCallStatusSuccess, FinishedAt: &now,
					Events: []*ToolEvent{{ID: "tool-event-1", Payload: map[string]any{"progress": 50}}},
					Interrupt: &Interrupt{
						ID: "interrupt-1", CorrelationID: "arc-1", ToolCallID: "tool-1", Timestamp: now,
						Status: InterruptApproved, Reason: "needs approval", Response: "approved", Responder: actor,
					},
				}},
			}},
			Cancel: &Cancel{
				ID: "cancel-1", Seq: 8, Arc: "arc-1", Timestamp: now, Actor: actor, Reason: &reason, TerminatedWork: true,
			},
			Errors: []*Error{{
				ID: "error-1", Seq: 9, CorrelationID: "arc-1", Timestamp: now, Message: "request failed", CausationID: &causationID,
			}},
			AsyncTasks: []*AsyncTask{{
				ID: "task-1", CorrelationID: "arc-1", TaskName: "researcher", Status: AsyncTaskSuccess,
				Args: "topic", Result: "answer", FinishedAt: &now,
				Events: []AsyncTaskEvent{{ID: "task-event-1", Timestamp: now, Type: AsyncTaskEventNotification, Payload: "Need input"}},
			}},
		}},
	}

	var output bytes.Buffer
	if err := PrintHistory(&output, history); err != nil {
		t.Fatalf("PrintHistory() error = %v", err)
	}

	for _, want := range []string{
		"Session: session-1", "Chat", "Hello\n    there", "arc-1", "ASSISTANT", "Looking it up.",
		"tool call lookup", "tool-event-1", "interrupt interrupt-1", "Async tasks", "researcher", "task-event-1",
		"Cancel", "cancel-1", "terminated_work: true", "Errors", "error-1", "causation_id: tool-1",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("PrintHistory() output does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestPrintHistoryOmitsEmptySectionsOfAnArc(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	history := &History{
		SessionID: "session-1",
		Arcs: []*Arc{{
			ID: "arc-1", Seq: 0, Timestamp: now, Content: "Hello", Actor: &users.Actor{UserID: "user-1"},
			Responses: []*AssistantMessage{{ID: "message-1", Seq: 1, CorrelationID: "arc-1", Timestamp: now, Content: "Hi."}},
		}},
	}

	var output bytes.Buffer
	if err := PrintHistory(&output, history); err != nil {
		t.Fatalf("PrintHistory() error = %v", err)
	}

	for _, unwanted := range []string{"Interrupts", "Async tasks", "Errors", "Cancel"} {
		if strings.Contains(output.String(), unwanted) {
			t.Errorf("PrintHistory() output contains %q for an arc that has none:\n%s", unwanted, output.String())
		}
	}
}

func TestRenderChatIncludesMessagesAndToolActivity(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	history := &History{
		SessionID: "session-1",
		Arcs: []*Arc{{
			ID: "arc-1", Timestamp: now, Content: "Hello", Actor: &users.Actor{UserID: "user-1"},
			Responses: []*AssistantMessage{{
				ID: "message-1", CorrelationID: "arc-1", Timestamp: now, Content: "I found it.",
				ToolCalls: []*ToolCall{{
					ID: "tool-1", ToolName: "lookup", Args: "status", Result: "healthy", Status: ToolCallStatusSuccess,
					Events: []*ToolEvent{{ID: "event-1", Payload: "working"}},
				}},
			}},
		}},
	}

	output := history.RenderChat()
	for _, want := range []string{"Chat · session session-1", "user-1", "Hello", "I found it.", "lookup", "SUCCESS", "healthy", "working"} {
		if !strings.Contains(output, want) {
			t.Errorf("RenderChat() output does not contain %q:\n%s", want, output)
		}
	}
}
