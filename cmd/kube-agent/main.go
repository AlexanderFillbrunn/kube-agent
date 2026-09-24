package main

import (
	"fmt"
	"os"
	"time"

	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
)

const sessionID = "demo-session"

func main() {
	h, err := history.Replay(sessionID, demoEvents())
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Print(h.RenderChat())
}

func demoEvents() []events.Event {
	user := users.Actor{UserID: "demo-user", TenantID: "demo-tenant"}
	arc := "message-user-1"
	vendorID := "demo-assistant"
	start := time.Date(2026, time.September, 16, 9, 0, 0, 0, time.UTC)

	return []events.Event{
		{
			SessionID: sessionID, ID: arc, Seq: 0, Timestamp: start,
			Typ: events.UserMessageEventType, CorrelationID: &arc, Actor: user,
			Payload: events.UserMessagePayload{
				Content: "What is the deployment status?", Actor: user,
			},
		},
		{
			SessionID: sessionID, ID: "message-assistant-1", Seq: 1, Timestamp: start.Add(time.Second),
			Typ: events.AssistantMessageEventType, CorrelationID: &arc,
			Payload: events.AssistantMessagePayload{
				VendorID: &vendorID, ModelID: "demo-model", Content: "I will check the deployment.",
			},
		},
		{
			SessionID: sessionID, ID: "tool-call-1", Seq: 2, Timestamp: start.Add(2 * time.Second),
			Typ: events.ToolCallEventType, CorrelationID: &arc,
			Payload: events.ToolCallPayload{
				MessageID: "message-assistant-1", VendorID: "tool-call-demo-1", ToolName: "get_deployment",
				Args: map[string]any{"name": "api", "namespace": "default"},
			},
		},
		{
			SessionID: sessionID, ID: "tool-event-1", Seq: 3, Timestamp: start.Add(3 * time.Second),
			Typ: events.ToolEventEventType, CorrelationID: &arc,
			Payload: events.ToolEventPayload{
				CausationID: "tool-call-1", Payload: map[string]any{"phase": "fetching"},
			},
		},
		{
			SessionID: sessionID, ID: "tool-result-1", Seq: 4, Timestamp: start.Add(4 * time.Second),
			Typ: events.ToolResultEventType, CorrelationID: &arc,
			Payload: events.ToolResultPayload{
				CausationID: "tool-call-1", Result: map[string]any{"available_replicas": 3, "desired_replicas": 3},
			},
		},
		{
			SessionID: sessionID, ID: "message-assistant-2", Seq: 5, Timestamp: start.Add(5 * time.Second),
			Typ: events.AssistantMessageEventType, CorrelationID: &arc,
			Payload: events.AssistantMessagePayload{
				VendorID: &vendorID, ModelID: "demo-model", Content: "The api deployment is healthy: 3 of 3 replicas are available.",
			},
		},
	}
}
