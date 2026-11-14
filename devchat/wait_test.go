package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callWait(ctx context.Context, t *testing.T, seconds float64) *mcp.CallToolResult {
	t.Helper()
	args, err := json.Marshal(map[string]any{"seconds": seconds})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result, err := wait(ctx, &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "wait", Arguments: args},
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	return result
}

// The clamp is what keeps a model from tying up the harness, and the note is
// what keeps it from believing the wait it asked for happened.
func TestWaitIsClampedAndSaysSo(t *testing.T) {
	defer func(was time.Duration) { maxWait = was }(maxWait)
	maxWait = 20 * time.Millisecond

	started := time.Now()
	result := callWait(context.Background(), t, 600)

	if took := time.Since(started); took > time.Second {
		t.Errorf("the wait took %s, want it cut to %s", took, maxWait)
	}
	if result.IsError {
		t.Fatalf("a clamped wait reported an error: %v", result.Content)
	}

	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("the result is %T, want structured output", result.StructuredContent)
	}
	if got := structured["waited_seconds"]; got != maxWait.Seconds() {
		t.Errorf("it reports waiting %v seconds, want %v", got, maxWait.Seconds())
	}
	if structured["note"] == nil {
		t.Error("a shortened wait said nothing about being shortened")
	}
}

// The tool has to give up when the run is cancelled, or a /cancel waits out
// the tool before it takes effect.
func TestWaitStopsWhenTheRunIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	result := callWait(ctx, t, 10)

	if took := time.Since(started); took > time.Second {
		t.Errorf("a cancelled wait took %s, want it to give up at once", took)
	}
	// It did not do what it was asked, and it reports that as a result the
	// model can read rather than as a Go error, which the log would file as a
	// failure of the call itself.
	if !result.IsError {
		t.Error("a cancelled wait reported success")
	}
}

func TestWaitRefusesANegativeWait(t *testing.T) {
	if result := callWait(context.Background(), t, -1); !result.IsError {
		t.Error("a negative wait was accepted")
	}
}
