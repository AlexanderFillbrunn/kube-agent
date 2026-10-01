package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"fillbrunn.ai/kube-agent/pkg/history"
)

const (
	openAIDefaultBaseURL = "https://api.openai.com/v1"
	openAIDoneSentinel   = "[DONE]"
)

// OpenAI is an LLM backed by the OpenAI Chat Completions API. The zero value
// is not usable; build one with NewOpenAI.
type OpenAI struct {
	// Model is the model ID sent with every request. It may be changed between
	// requests to switch models mid-session, but changing it while a request
	// is in flight is a race. What a reply was produced by is read from the
	// provider's answer, not from here.
	Model string

	// BaseURL is the API root without a trailing slash, so an
	// OpenAI-compatible endpoint can be used in place of OpenAI's own.
	BaseURL string

	apiKey string
	client *http.Client

	// modes is how much of the log each request discloses. It is fixed for the
	// session, so a projection is a pure function of the log and the arc.
	modes history.Modes

	// noteRole carries synthesized notes. OpenAI offers `developer` and
	// `system`; compatible endpoints frequently reject `developer`.
	noteRole string
}

// OpenAIOption configures an OpenAI client.
type OpenAIOption func(*OpenAI)

// WithBaseURL points the client at an OpenAI-compatible endpoint.
func WithBaseURL(url string) OpenAIOption {
	return func(o *OpenAI) { o.BaseURL = strings.TrimSuffix(url, "/") }
}

// WithHTTPClient supplies the client used for requests, which is where
// timeouts, retries and transport-level instrumentation belong.
func WithHTTPClient(client *http.Client) OpenAIOption {
	return func(o *OpenAI) { o.client = client }
}

// WithProjectionModes sets how much of the log the model is told. See
// PROJECTION.md.
func WithProjectionModes(modes history.Modes) OpenAIOption {
	return func(o *OpenAI) { o.modes = modes }
}

// WithSystemNoteRole carries notes as `system` rather than `developer`, for
// endpoints that do not know the developer role.
func WithSystemNoteRole() OpenAIOption {
	return func(o *OpenAI) { o.noteRole = openAIRoleSystem }
}

// NewOpenAI returns a client for one model.
func NewOpenAI(apiKey, model string, opts ...OpenAIOption) *OpenAI {
	llm := &OpenAI{
		Model:    model,
		BaseURL:  openAIDefaultBaseURL,
		apiKey:   apiKey,
		client:   http.DefaultClient,
		modes:    history.DefaultModes(),
		noteRole: openAIRoleDeveloper,
	}
	for _, opt := range opts {
		opt(llm)
	}
	return llm
}

var _ LLM = (*OpenAI)(nil)

// openAIRequest is the body of a Chat Completions call.
type openAIRequest struct {
	Model         string               `json:"model"`
	Messages      []openAIMessage      `json:"messages"`
	Tools         []openAITool         `json:"tools,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}

// openAIStreamOptions asks for the usage chunk. A stream reports nothing about
// what it cost unless this is set, and it arrives as a final chunk carrying no
// choices.
type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// openAIUsage is what the provider says the exchange cost. Cached tokens are
// counted within PromptTokens rather than alongside it.
type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *openAIUsage) tokenUsage() *TokenUsage {
	if u == nil {
		return nil
	}
	return &TokenUsage{
		Input:       u.PromptTokens,
		CachedInput: u.PromptTokensDetails.CachedTokens,
		Output:      u.CompletionTokens,
	}
}

// StreamResponse asks the model for one response and streams its text to
// tokSink as it arrives. The reply itself is returned whole.
func (o *OpenAI) StreamResponse(ctx context.Context, h *history.History, opts *LLMOptions, tokSink Sink[string]) (*LLMResponse, error) {
	return o.respond(ctx, h, opts, tokSink)
}

// Response asks the model for one response without streaming it.
func (o *OpenAI) Response(ctx context.Context, h *history.History, opts *LLMOptions) (*LLMResponse, error) {
	return o.respond(ctx, h, opts, nil)
}

// respond runs one exchange. A nil tokSink means the response is not
// streamed.
func (o *OpenAI) respond(ctx context.Context, h *history.History, opts *LLMOptions, tokSink Sink[string]) (*LLMResponse, error) {
	if opts == nil {
		opts = &LLMOptions{}
	}
	arc, err := targetArc(h)
	if err != nil {
		return nil, err
	}

	body, err := o.buildRequest(h, arc.ID, opts, tokSink != nil)
	if err != nil {
		return nil, err
	}

	response, err := o.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer response.Close()

	var completion *openAICompletion
	if tokSink != nil {
		completion, err = readStream(response, tokSink)
	} else {
		completion, err = readCompletion(response)
	}
	if err != nil {
		return nil, err
	}

	return o.asResponse(completion), nil
}

// targetArc returns the arc the response belongs to, which is the last arc in
// the history.
func targetArc(h *history.History) (*history.Arc, error) {
	if h == nil || len(h.Arcs) == 0 {
		return nil, errors.New("cannot respond to a history with no arcs")
	}
	return h.Arcs[len(h.Arcs)-1], nil
}

func (o *OpenAI) buildRequest(h *history.History, arcID string, opts *LLMOptions, stream bool) (*openAIRequest, error) {
	transcript, err := h.Project(arcID, o.modes)
	if err != nil {
		return nil, fmt.Errorf("project arc %s: %w", arcID, err)
	}

	enc := encoder{vendorIDs: vendorToolCallIDs(h), noteRole: o.noteRole}
	messages, err := enc.encodeTranscript(transcript, opts.SystemPrompt)
	if err != nil {
		return nil, fmt.Errorf("encode transcript of arc %s: %w", arcID, err)
	}

	request := &openAIRequest{
		Model:    o.Model,
		Messages: messages,
		Tools:    encodeTools(opts.Tools),
		Stream:   stream,
	}
	if stream {
		request.StreamOptions = &openAIStreamOptions{IncludeUsage: true}
	}
	return request, nil
}

// post sends the request and returns the response body, which the caller
// closes. An error status is drained here so that the caller only ever deals
// with a body it can parse.
func (o *OpenAI) post(ctx context.Context, body *openAIRequest) (io.ReadCloser, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	if body.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call OpenAI: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, openAIError(resp)
	}
	return resp.Body, nil
}

// openAIError turns an error response into an error carrying whatever the API
// said about it.
func openAIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error.Message != "" {
		return fmt.Errorf("openai: %s (%s %s): %s", resp.Status, payload.Error.Type, payload.Error.Code, payload.Error.Message)
	}
	return fmt.Errorf("openai: %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

// openAICompletion is one response, however it arrived: the streamed and
// non-streamed readers both produce this.
type openAICompletion struct {
	// vendorID is the provider's own identifier for the message.
	vendorID string

	model        string
	content      string
	toolCalls    []openAIToolCall
	finishReason string

	// usage is nil where the provider reported none, which an
	// OpenAI-compatible endpoint that ignores stream_options will do.
	usage *openAIUsage
}

// readCompletion reads a non-streamed response.
func readCompletion(body io.Reader) (*openAICompletion, error) {
	var payload struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string           `json:"content"`
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode completion: %w", err)
	}
	if len(payload.Choices) == 0 {
		return nil, errors.New("openai returned a completion with no choices")
	}

	choice := payload.Choices[0]
	return &openAICompletion{
		vendorID:     payload.ID,
		model:        payload.Model,
		content:      choice.Message.Content,
		toolCalls:    choice.Message.ToolCalls,
		finishReason: choice.FinishReason,
		usage:        payload.Usage,
	}, nil
}

// openAIChunk is one server-sent event of a streamed completion.
type openAIChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string                `json:"content"`
			ToolCalls []openAIToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// openAIToolCallDelta is a fragment of one tool call. Index identifies the
// call within the message and is the only field every fragment carries.
type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// readStream consumes the SSE stream, handing each text fragment to tokSink as
// it arrives and assembling the completion behind it.
func readStream(body io.Reader, tokSink Sink[string]) (*openAICompletion, error) {
	completion := &openAICompletion{}
	// Calls are keyed by their index rather than appended, because fragments
	// of two parallel calls interleave.
	calls := map[int]*openAIToolCall{}
	var order []int

	reader := bufio.NewReader(body)
	for {
		line, err := reader.ReadString('\n')
		if data, ok := sseData(line); ok {
			if data == openAIDoneSentinel {
				break
			}
			var chunk openAIChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return nil, fmt.Errorf("decode stream chunk: %w", err)
			}
			if chunk.Error != nil {
				return nil, fmt.Errorf("openai: %s", chunk.Error.Message)
			}
			if err := applyChunk(completion, calls, &order, chunk, tokSink); err != nil {
				return nil, err
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read stream: %w", err)
		}
	}

	for _, index := range order {
		completion.toolCalls = append(completion.toolCalls, *calls[index])
	}
	return completion, nil
}

// sseData returns the payload of a `data:` line. Every other line of the
// protocol, including the blank line that ends an event, carries nothing this
// needs.
func sseData(line string) (string, bool) {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")), true
}

// applyChunk folds one chunk into the completion being assembled.
func applyChunk(completion *openAICompletion, calls map[int]*openAIToolCall, order *[]int, chunk openAIChunk, tokSink Sink[string]) error {
	if chunk.ID != "" {
		completion.vendorID = chunk.ID
	}
	if chunk.Model != "" {
		completion.model = chunk.Model
	}
	if chunk.Usage != nil {
		completion.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		// The usage chunk carries no choice.
		return nil
	}

	choice := chunk.Choices[0]
	if choice.FinishReason != "" {
		completion.finishReason = choice.FinishReason
	}
	if choice.Delta.Content != "" {
		completion.content += choice.Delta.Content
		if err := tokSink.Consume(choice.Delta.Content); err != nil {
			return fmt.Errorf("consume token: %w", err)
		}
	}

	for _, delta := range choice.Delta.ToolCalls {
		call, ok := calls[delta.Index]
		if !ok {
			call = &openAIToolCall{Type: "function"}
			calls[delta.Index] = call
			*order = append(*order, delta.Index)
		}
		if delta.ID != "" {
			call.ID = delta.ID
		}
		if delta.Type != "" {
			call.Type = delta.Type
		}
		if delta.Function.Name != "" {
			call.Function.Name = delta.Function.Name
		}
		call.Function.Arguments += delta.Function.Arguments
	}
	return nil
}

// asResponse turns a completion into the reply the agent records. The model
// the provider reports is preferred over the one that was asked for, so the
// log names the model that ran rather than an alias.
func (o *OpenAI) asResponse(completion *openAICompletion) *LLMResponse {
	response := &LLMResponse{
		ModelID: completion.model,
		Content: completion.content,
		Usage:   completion.usage.tokenUsage(),
	}
	if response.ModelID == "" {
		response.ModelID = o.Model
	}
	if completion.vendorID != "" {
		response.VendorID = &completion.vendorID
	}

	for _, call := range completion.toolCalls {
		response.ToolCalls = append(response.ToolCalls, LLMToolCall{
			VendorID: call.ID,
			ToolName: call.Function.Name,
			Args:     decodeArguments(call.Function.Arguments),
		})
	}
	return response
}

// decodeArguments parses the arguments the model produced. Malformed JSON is
// kept verbatim, so the model is told about the call it made and can correct
// itself.
func decodeArguments(raw string) any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var args any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return raw
	}
	return args
}
