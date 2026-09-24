package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"fillbrunn.ai/kube-agent/internal/filestore"
	"fillbrunn.ai/kube-agent/pkg/agent"
	"fillbrunn.ai/kube-agent/pkg/events"
	"fillbrunn.ai/kube-agent/pkg/history"
	"fillbrunn.ai/kube-agent/pkg/users"
)

// stubModel serves one scripted stream per request, so a turn can be driven
// without a provider.
func stubModel(t *testing.T, streams ...string) *httptest.Server {
	t.Helper()
	var served int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if served >= len(streams) {
			t.Errorf("the model was asked %d times, with %d streams scripted", served+1, len(streams))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streams[served])
		served++
	}))
	t.Cleanup(server.Close)
	return server
}

func sse(lines ...string) string {
	return strings.Join(append(lines, "data: [DONE]", ""), "\n\n")
}

// One turn has to reach the log as a whole arc: the question, the reply that
// called a tool, the dispatch, the result, the answer, and the close. This is
// the only test that runs the pieces together over a real file.
func TestTurnWritesAWholeArc(t *testing.T) {
	model := stubModel(t,
		sse(
			`data: {"id":"chatcmpl-1","model":"gpt-5","choices":[{"delta":{"content":"looking"}}]}`,
			`data: {"id":"chatcmpl-1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"list_pods","arguments":"{\"namespace\":\"kube-system\"}"}}]}}]}`,
			`data: {"id":"chatcmpl-1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		),
		sse(
			`data: {"id":"chatcmpl-2","model":"gpt-5","choices":[{"delta":{"content":"one pod is pending"}}]}`,
			`data: {"id":"chatcmpl-2","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		),
	)

	path := filepath.Join(t.TempDir(), "log.jsonl")
	store, err := filestore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	registry := agent.NewToolRegistry()
	if err := registerFakeTools(registry); err != nil {
		t.Fatalf("registerFakeTools: %v", err)
	}

	// The harness follows a run through these two sinks, so the test watches
	// the same way the interface does.
	shown := newTranscript()
	var tokens strings.Builder
	chat, err := agent.NewAgent(
		&agent.AgentConfig{
			SystemPrompt: "be terse",
			Tokens: agent.SinkFromFunc(func(token string) error {
				tokens.WriteString(token)
				return nil
			}),
			Observer: agent.SinkFromFunc(func(ev *events.Event) error {
				shown.event(ev)
				return nil
			}),
		},
		registry,
		agent.NewOpenAI("test-key", "gpt-5", agent.WithBaseURL(model.URL)),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	session := &session{
		id:        "session",
		store:     store,
		persistor: agent.StampSession(store, "session"),
		agent:     chat,
		actor:     users.Actor{UserID: "dev", TenantID: "dev"},
	}
	if err := session.turn(context.Background(), "what is pending?"); err != nil {
		t.Fatalf("turn: %v", err)
	}

	stored, err := store.Events(context.Background())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	want := []events.EventType{
		events.UserMessageEventType,
		events.AssistantMessageEventType,
		events.ToolCallEventType,
		events.ToolDispatchedEventType,
		events.ToolResultEventType,
		events.AssistantMessageEventType,
		events.ArcClosedEventType,
	}
	if len(stored) != len(want) {
		t.Fatalf("log holds %d events, want %d: %s", len(stored), len(want), typesOf(stored))
	}
	for i, typ := range want {
		if stored[i].Typ != typ {
			t.Fatalf("log is %s, want %v", typesOf(stored), want)
		}
	}

	// The log has to replay into the same conversation it recorded, since
	// every later turn is built by replaying it.
	replayed, err := history.Replay("session", values(stored))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	arc := replayed.Arcs[0]
	if arc.Status() != history.ArcStatusDone {
		t.Errorf("replayed arc is %s, want it closed", arc.Status())
	}
	call := arc.Responses[0].ToolCalls[0]
	if call.Status != history.ToolCallStatusSuccess || call.VendorID != "call_abc" {
		t.Errorf("replayed call = %+v, want a successful call_abc", call)
	}
	result, ok := call.Result.(map[string]any)
	if !ok || result["pods"] == nil {
		t.Errorf("call result = %#v, want the pods the tool returned", call.Result)
	}

	if tokens.String() != "lookingone pod is pending" {
		t.Errorf("streamed %q, want both replies as they were written", tokens.String())
	}

	// What the run showed has to match what it recorded: one call, done, with
	// the answer after it.
	var states []string
	for _, b := range shown.blocks {
		if b.kind == blockTool {
			states = append(states, b.tool+" "+string(b.state))
		}
	}
	if len(states) != 1 || states[0] != "list_pods done" {
		t.Errorf("transcript shows calls %v, want one finished list_pods", states)
	}
	last := shown.blocks[len(shown.blocks)-1]
	if last.kind != blockAssistant || last.text != "one pod is pending" {
		t.Errorf("transcript ends with %+v, want the answer", last)
	}
}

// The second turn has to carry the first one's calls back under the IDs the
// model issued them with.
func TestSecondTurnReplaysTheFirst(t *testing.T) {
	model := stubModel(t,
		sse(
			`data: {"id":"chatcmpl-1","model":"gpt-5","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"list_pods","arguments":"{}"}}]}}]}`,
			`data: {"id":"chatcmpl-1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		),
		sse(`data: {"id":"chatcmpl-2","model":"gpt-5","choices":[{"delta":{"content":"five pods"},"finish_reason":"stop"}]}`),
		sse(`data: {"id":"chatcmpl-3","model":"gpt-5","choices":[{"delta":{"content":"still five"},"finish_reason":"stop"}]}`),
	)

	var lastRequest map[string]any
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &lastRequest); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		proxy, err := http.Post(model.URL, "application/json", strings.NewReader(string(raw)))
		if err != nil {
			t.Errorf("proxy: %v", err)
			return
		}
		defer proxy.Body.Close()
		io.Copy(w, proxy.Body)
	}))
	defer recorder.Close()

	store, err := filestore.Open(filepath.Join(t.TempDir(), "log.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	registry := agent.NewToolRegistry()
	if err := registerFakeTools(registry); err != nil {
		t.Fatalf("registerFakeTools: %v", err)
	}
	chat, err := agent.NewAgent(nil, registry,
		agent.NewOpenAI("test-key", "gpt-5", agent.WithBaseURL(recorder.URL)))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	session := &session{id: "session", store: store, persistor: agent.StampSession(store, "session"), agent: chat}
	for _, message := range []string{"what is running?", "and now?"} {
		if err := session.turn(context.Background(), message); err != nil {
			t.Fatalf("turn %q: %v", message, err)
		}
	}

	// The last request is the second turn's, and it has to hold the first
	// turn's call and result.
	encoded, err := json.Marshal(lastRequest["messages"])
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	sent := string(encoded)
	if !strings.Contains(sent, "call_abc") {
		t.Errorf("the second turn did not carry the first call back: %s", sent)
	}
	if !strings.Contains(sent, `"role":"tool"`) {
		t.Errorf("the second turn carried no tool result: %s", sent)
	}
	if !strings.Contains(sent, "and now?") {
		t.Errorf("the second turn did not carry its own question: %s", sent)
	}
}

func typesOf(stored []*events.Event) string {
	var names []string
	for _, ev := range stored {
		names = append(names, string(ev.Typ))
	}
	return strings.Join(names, ", ")
}
