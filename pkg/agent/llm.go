package agent

import (
	"context"

	"fillbrunn.ai/kube-agent/pkg/history"
)

// LLMOptions holds information for an LLM to control how new prompts are handled.
type LLMOptions struct {
	SystemPrompt string
	Tools        *ToolRegistry
}

type Token struct {
	MessageID string
	Content   string
}

// LLMResponse is one reply from a model: what it said and the calls it asked
// for. It is what the model produced, not yet a record of it. The agent turns
// it into events, because a tool call names the message that requested it and
// only the persistor knows that message's ID.
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

	// Usage is what the reply cost, and is nil where the provider reported
	// nothing. Nil is not zero: a reply whose cost went unreported must not
	// be counted as a free one.
	Usage *TokenUsage

	ToolCalls []LLMToolCall
}

// TokenUsage is what one exchange cost, in tokens.
type TokenUsage struct {
	// Input counts every prompt token, the cached ones included. CachedInput
	// is a part of this number rather than an addition to it, which is how
	// providers report it and how they bill it.
	Input int

	// CachedInput is the part of the prompt that was served from the
	// provider's cache, and is billed at a lower rate than the rest.
	CachedInput int

	// Output counts the tokens the model generated, reasoning tokens
	// included where the provider charges for them.
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
	StreamResponse(ctx context.Context, history *history.History, opts *LLMOptions, tokSink Sink[string]) (*LLMResponse, error)
	Response(ctx context.Context, history *history.History, opts *LLMOptions) (*LLMResponse, error)
}
