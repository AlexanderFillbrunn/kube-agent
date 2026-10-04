package agent

import (
	"context"

	"fillbrunn.ai/kube-agent/pkg/history"
)

// RawToken is a token emitted from the LLM during streaming.
type RawToken struct {
	Content string
	Seq int
}

// LLMOptions holds information for an LLM to control how new prompts are handled.
type LLMOptions struct {
	SystemPrompt string
	Tools        ToolSet
}

// LLMResponse is one reply from a model: what it said and the calls it asked
// for. The agent turns it into events.
type LLMResponse struct {
	// VendorID is the provider's own identifier for the message, where the
	// provider issues one.
	VendorID *string

	// ModelID is the model that produced the reply, as the provider reports
	// it, which need not be the model that was asked for.
	ModelID string

	Content string

	// Reasoning is the provider's record of how the model reached this reply,
	// opaque bytes to be handed back verbatim, and nil where the provider
	// returns none.
	Reasoning []byte

	// Usage is what the reply cost. It is nil where the provider reported
	// nothing, which is not the same as zero.
	Usage *TokenUsage

	ToolCalls []LLMToolCall
}

// TokenUsage is what one exchange cost, in tokens.
type TokenUsage struct {
	// Input counts every prompt token, CachedInput included.
	Input int

	// CachedInput is the part of the prompt served from the provider's cache.
	CachedInput int

	// Output counts the tokens the model generated, reasoning included.
	Output int
}

// LLMToolCall is one call a reply requested, under the provider's own ID.
type LLMToolCall struct {
	VendorID string
	ToolName string
	Args     any
}

// LLM represents a large language model with tool calling capabilities.
type LLM interface {
	StreamResponse(ctx context.Context, history *history.History, opts *LLMOptions, tokSink Sink[RawToken]) (*LLMResponse, error)
	Response(ctx context.Context, history *history.History, opts *LLMOptions) (*LLMResponse, error)
}
