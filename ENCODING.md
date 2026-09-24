Encoding
========

Purpose
--------------------
Encoding is the second transition in the pipeline. Projection turns the event
log into a `ModelTranscript`. Encoding turns a `ModelTranscript` into one
provider's request body.

    event log  ──project──▶  ModelTranscript  ──encode──▶  provider messages

[PROJECTION.md](PROJECTION.md) specifies the first transition and decides what
the model is told. This document specifies the second and decides how it is
carried. An encoder never decides whether the model learns something. Anything
it would have to invent is a projection decision that was left open.

The encoder's work is four repairs and a shape translation.

    carrier     a note has no role. The encoder picks one.
    merging     adjacent same-role messages, where the provider rejects them
    trailing    the last message, where the provider constrains its role
    pairing     calls and results, in the shape and ID space the provider uses

Input
--------------------
A `ModelTranscript` is a flat ordered list of `ModelMessage`. Four shapes, from
`pkg/history/projection.go`.

    ModelTextMessage        text with a role, USER or INSTRUCTION
    ModelAssistantMessage   the model's text, plus the calls it requested
    ModelToolMessage        one call's outcome, keyed by tool call ID
    ModelToolCall           one call, carried by the assistant message

Two things in it are deliberately underspecified, and the encoder settles both.

Notes arrive as `ModelTextMessage`. A note is synthesized status text that no
participant said, so its role is a carrier question, not a fact about the log.

Tool call IDs are the log's own IDs. `ToolCall.VendorID` holds the provider's
identifier for a call that a provider already issued. See Pairing.

Provider constraints
--------------------
Four providers, four different answers. `*` marks a rule the provider does not
document and that is only observable as a 400.

                        adjacent user     must end on    per-message
                        messages          a user turn    system role
    Anthropic           combined          yes            yes, newer models
    OpenAI              accepted          no rule        yes, any position
    Mistral             400*              yes*           front only
    Gemini              400*              yes*           no

Anthropic combines consecutive same-role turns into one turn server-side. The
reference states it. The SDK error catalogue still lists a 400 for
non-alternating roles, so the two sources disagree. Verify against the models
you target before relying on the combining.

Mistral enforces both rules in the chat template and in `mistral-common`, not
in the API prose. The errors are `Conversation roles must alternate
user/assistant/user/assistant/...` and `Expected last role User or Tool (or
Assistant with prefix True) for serving but got assistant`.

Gemini enforces alternation between `user` and `model`, requires the first
`Content` to be `user`, and requires the request to end on a user turn or a
function response. Its errors are `Please ensure that multiturn requests
alternate between user and model` and `Please ensure that multiturn requests
ends with a user role or a function response`.

Prefill, an assistant message in final position, is a 400 on current Anthropic
models and is how Mistral satisfies its own trailing rule, through
`prefix: true`. The same message shape is forbidden by one provider and is the
escape hatch of another.

Carriers
--------------------
A note is text with no role. Each provider offers a different set of places to
put it.

    Anthropic   a `system` message inside `messages`, on Opus 5, Opus 4.8 and
                the Fable and Mythos families. Not on Sonnet 5. It must follow
                a user message, must not be `messages[0]`, and must be either
                last or followed by an assistant turn. It does not invalidate
                the cached prefix.

    OpenAI      a `developer` or `system` message, at any position.

    Mistral     no mid-conversation system role. A note is user text.

    Gemini      no per-message system role. `systemInstruction` is a separate
                field outside `contents`, so it cannot carry a note that
                belongs at a position. A note is user text.

Two carriers therefore exist across the four: an operator role where the
provider has one, and user text where it does not.

The choice changes what merging has to do. A note carried as user text is a
user message, so it can collide with a neighbouring user message and needs the
merge. A note carried as an operator message is its own role and usually does
not.

Do not render the same note through both carriers in one transcript. The model
reads position and role as provenance, and a note that is sometimes an operator
instruction and sometimes user speech teaches it nothing stable.

Merging
--------------------
Merging joins several `ModelMessage` of one role into one provider message. It
is required where a provider rejects adjacent same-role messages, and it is
worth doing even where the provider accepts them.

Anthropic combines adjacent same-role turns on its own, with no separator and
no attribution. That is unheadered merging performed by the server. The encoder
should merge first, so that the joining is explicit and labelled, rather than
let the API concatenate two people's words into one turn silently.

Adjacent user messages arise from interleaving. Two arcs opened by two
different people can sit next to each other with nothing between them, and a
note carried as user text lands next to a user message the same way.

Headers
--------------------
A merged message carries several original messages. Without a header the model
reads one speaker saying several things.

Every part of a merged message gets a header line. The header names the origin
and nothing else.

    [from alice #7f3a]
    how many pods are pending?

    [from bob #7f3a]
    restart the ingress controller

    [note #7f3a]
    the first request is still running. Another agent is on it.

Rules:

1. Headers appear on every part of a merged message, including the first. A
   leading part with no header reads as the frame and the rest as quoted
   material inside it.

2. In a session with more than one actor, every user message carries a header,
   merged or not. Otherwise the absence of a header on an unmerged message
   reads as "the same person as last time".

3. In a single-actor session an unmerged user message carries no header. There
   is no one to distinguish it from.

4. The header names the actor from `Arc.Actor`, or `note` for synthesized
   text. It does not carry timestamps, arc IDs or status. Those are the note's
   own content when they matter.

5. Order inside a merged message is log order. Merging joins, it never sorts.

The `#7f3a` suffix is a token drawn per request. User text can contain
`[from root]`, so a header without a nonce is forgeable, and a forged header
is an identity claim the model has no way to check. Generate the token per
request, put it in every header, and reject or escape any occurrence of the
token inside the content being merged. A content match is either a collision
or an attack, and both are handled the same way.

What may not merge
--------------------
Tool results. Every result of one assistant message belongs in one provider
message, and on Anthropic the `tool_result` blocks come first in that message's
content. A note may join that message only after the last result, never
between two results and never before the first.

Messages of different roles. Merging is within one role. A note carried as user
text next to an assistant message is not a merge candidate, it is an
alternation problem. See Synthetic messages.

Assistant messages, by default. Two adjacent assistant messages are two answers
to two different requests, and a header inside an assistant message is text the
model did not write. Where a provider forbids adjacent model turns, prefer a
synthetic user message between them. Merge assistant text only when that is
unavailable, and never merge two assistant messages that both carry tool calls,
because their results would have to be reordered to sit in one following
message.

Synthetic messages
--------------------
A synthetic message is one the encoder adds that carries no projected content.
Two cases need one.

*Separating two model turns.* Where a provider requires strict alternation and
the transcript holds two adjacent assistant messages, insert a user message
between them rather than merging them. The separator is a note, so it is
subject to Carriers and Headers, and it says what it is.

    a(R2.1) n{separator} a(R1.2)

*Closing on a user turn.* Where a provider requires the request to end on a
user turn and the transcript ends on an assistant message, the encoder appends
one. Mistral's `prefix: true` is the alternative and it means something else,
that the model should continue the text rather than answer it, so it is not a
drop-in for a finished assistant turn.

Both cases are rare. See [PROJECTION.md](PROJECTION.md) Phases: `pending:
stand_in` answers every in-flight call with a tool result, which is a user-role
message, so a transcript under the default modes ends on a user turn already.
A non-default `pending` value is what makes the trailing repair reachable.

Synthetic messages are the encoder's only licence to add content, and it is
narrow. Anything with meaning belongs to projection.

Pairing
--------------------
A call and its result travel together and the provider decides the shape.

    Anthropic   `tool_use` blocks inside the assistant message, `tool_result`
                blocks inside the next user message, matched by `tool_use_id`.
                All results of one assistant turn in one message.

    OpenAI      `tool_calls` on the assistant message, one `tool` role message
                per result, matched by `tool_call_id`.

    Mistral     `tool_calls` on the assistant message, `tool` role results.

    Gemini      a `functionCall` part in a `model` Content, a `functionResponse`
                part in a `user` Content. The response is user-role, which is
                how a tool result keeps Gemini's alternation intact.

IDs are per provider. `ModelToolCall.ID` is the log's ID and is not what goes
on the wire. Use `ToolCall.VendorID` where the log has one, because the model
issued that call under that ID and a transcript that renames it is a transcript
the model cannot match to its own request. A synthesized stand-in call has no
`VendorID`, so the encoder derives one deterministically from the log ID.

Instructions
--------------------
A `ModelTextMessage` with role INSTRUCTION is a real system prompt.

    Anthropic   the top-level `system` parameter
    OpenAI      a `developer` or `system` message in the array
    Mistral     a system message at the front
    Gemini      the `systemInstruction` field

Do not move a note into these. A system prompt is stable across the session and
sits in the cached prefix. A note is per turn and belongs at its position.

Open questions
--------------------
*Gemini function response keying.* Whether a `functionResponse` is matched to
its call by function name or by an ID is not settled here. Parallel calls to
one tool are the case that distinguishes them. Verify before encoding parallel
calls for Gemini.

*OpenAI unanswered calls.* Whether OpenAI rejects a transcript in which an
assistant `tool_calls` entry has no matching `tool` message is not confirmed.
It decides whether `pending: stand_in` is required for OpenAI or merely
preferred.

*Header placement under an operator carrier.* A note carried as an Anthropic
`system` message is already distinguishable by role. Whether it still carries a
`[note]` header, for consistency with the user-text carrier, is open.

*Anthropic combining.* The reference says consecutive same-role turns are
combined. The error catalogue says they are a 400. Settle it with a request
before relying on either.

Sources
--------------------
    Anthropic   https://platform.claude.com/docs/en/api/messages
    OpenAI      https://developers.openai.com/api/docs/api-reference/chat/create
    Mistral     https://docs.mistral.ai/capabilities/completion/
                https://docs.mistral.ai/cookbooks/concept-deep-dive-tokenization-chat_templates
                https://github.com/mistralai/mistral-common/pull/246
    Gemini      https://ai.google.dev/gemini-api/docs/function-calling
                https://ai.google.dev/api/generate-content

The Mistral and Gemini rules marked `*` in Provider constraints come from the
errors themselves, reported against client libraries, not from the providers'
prose.

    https://github.com/langchain-ai/langchain/issues/23703   Mistral, last role
    https://github.com/vllm-project/vllm/issues/6862         Mistral, alternation
    https://github.com/langchain-ai/langchain/issues/22651   Gemini, alternation
    https://github.com/langchain-ai/langchain/issues/16288   Gemini, first turn
    https://github.com/danny-avila/LibreChat/discussions/1561 Gemini, last turn
