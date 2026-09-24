package main

import (
	"testing"

	"fillbrunn.ai/kube-agent/pkg/events"
)

func arcEvent(id string, typ events.EventType, payload events.EventPayload) *events.Event {
	arc := "arc-1"
	return &events.Event{ID: id, Typ: typ, CorrelationID: &arc, Payload: payload}
}

// Tokens are what the reply looks like while it arrives; the recorded message
// is what it was. The second settles the first.
func TestRecordedReplySettlesTheStreamedOne(t *testing.T) {
	shown := newTranscript()
	shown.token("look")
	shown.token("ing")
	if got := shown.blocks[0].text; got != "looking" {
		t.Fatalf("streamed text = %q", got)
	}

	shown.event(arcEvent("msg-1", events.AssistantMessageEventType,
		events.AssistantMessagePayload{ModelID: "gpt-5", Content: "looking at kube-system"}))

	if len(shown.blocks) != 1 {
		t.Fatalf("transcript holds %d blocks, want the one reply", len(shown.blocks))
	}
	if shown.blocks[0].text != "looking at kube-system" {
		t.Errorf("reply = %q, want what was recorded", shown.blocks[0].text)
	}
	if shown.streaming != nil {
		t.Error("the finished reply is still being streamed into")
	}
}

// A reply that only asked for tools said nothing, and leaves no empty block
// sitting in the transcript.
func TestAReplyWithNoTextLeavesNoBlock(t *testing.T) {
	shown := newTranscript()
	shown.event(arcEvent("msg-1", events.AssistantMessageEventType,
		events.AssistantMessagePayload{ModelID: "gpt-5"}))

	if len(shown.blocks) != 0 {
		t.Errorf("transcript holds %+v, want nothing", shown.blocks)
	}
}

// A call moves through queued, running and done, and each step has to find the
// line the call created.
func TestToolCallTracksItsOwnLine(t *testing.T) {
	shown := newTranscript()
	shown.event(arcEvent("tc-1", events.ToolCallEventType, events.ToolCallPayload{
		MessageID: "msg-1", VendorID: "call_abc", ToolName: "list_pods",
		Args: map[string]any{"namespace": "kube-system"},
	}))

	called := shown.blocks[0]
	if called.state != toolQueued || called.args != `{"namespace":"kube-system"}` {
		t.Fatalf("call block = %+v, want it queued with its arguments", called)
	}

	shown.event(arcEvent("td-1", events.ToolDispatchedEventType, events.ToolDispatchedPayload{CausationID: "tc-1"}))
	if called.state != toolRunning {
		t.Errorf("a dispatched call is %s, want running", called.state)
	}

	shown.event(arcEvent("tr-1", events.ToolResultEventType, events.ToolResultPayload{
		CausationID: "tc-1", Result: map[string]any{"pods": 2},
	}))
	if called.state != toolDone || called.result != `{"pods":2}` {
		t.Errorf("a settled call is %+v, want it done with its result", called)
	}
	if len(shown.blocks) != 1 {
		t.Errorf("the call grew to %d blocks", len(shown.blocks))
	}
}

func TestAFailedResultShowsAsFailed(t *testing.T) {
	shown := newTranscript()
	shown.event(arcEvent("tc-1", events.ToolCallEventType,
		events.ToolCallPayload{MessageID: "msg-1", ToolName: "restart_deployment"}))
	shown.event(arcEvent("tr-1", events.ToolResultEventType,
		events.ToolResultPayload{CausationID: "tc-1", Result: "no such deployment", IsError: true}))

	if shown.blocks[0].state != toolFailed {
		t.Errorf("call is %s, want failed", shown.blocks[0].state)
	}
}
