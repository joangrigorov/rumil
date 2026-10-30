package reviewer

import (
	"context"
	"sync"
	"time"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Observer receives the progress of one review session. The engine calls it
// from the goroutines the kit session runs on, so an observer must be safe for
// concurrent use and must return quickly: a slow observer delays the model.
//
// Observation is best effort. The engine ignores an observer's own failures;
// the review is the product, and the transcript is a trace of it.
type Observer interface {
	Observe(ctx context.Context, event Event)
}

// Event is one recorded step. The type is stable and the payload is rendered
// as JSON by the observation deck.
type Event struct {
	Type    string
	At      time.Time
	Payload map[string]any
}

// sessionMetadataEvent is emitted once the engine knows what it is reviewing.
func sessionMetadataEvent(title, headSHA, baseSHA string, changedFiles int) Event {
	return Event{
		Type: "session_metadata",
		At:   time.Now(),
		Payload: map[string]any{
			"title":         title,
			"head_sha":      headSHA,
			"base_sha":      baseSHA,
			"changed_files": changedFiles,
		},
	}
}

// observeKitEvent maps the kit session's lifecycle onto the deck's vocabulary.
// These are the events that carry the model's reasoning, its text, the tool
// calls it made, and what the tools answered.
func (e *Engine) observeKitEvent(ctx context.Context, observer Observer, event kit.Event) {
	var mapped Event
	mapped.At = time.Now()

	switch event := event.(type) {
	case kit.TurnStartEvent:
		mapped = Event{Type: "turn_start", At: mapped.At, Payload: map[string]any{"prompt": event.Prompt}}
	case kit.StepStartEvent:
		mapped = Event{Type: "step_start", At: mapped.At, Payload: map[string]any{"step": event.StepNumber}}
	case kit.ReasoningStartEvent:
		mapped = Event{Type: "reasoning_start", At: mapped.At, Payload: map[string]any{"id": event.ID}}
	case kit.ReasoningDeltaEvent:
		mapped = Event{Type: "reasoning_delta", At: mapped.At, Payload: map[string]any{"delta": event.Delta}}
	case kit.ReasoningCompleteEvent:
		mapped = Event{Type: "reasoning_end", At: mapped.At, Payload: map[string]any{}}
	case kit.MessageStartEvent:
		mapped = Event{Type: "message_start", At: mapped.At, Payload: map[string]any{}}
	case kit.MessageUpdateEvent:
		mapped = Event{Type: "message_delta", At: mapped.At, Payload: map[string]any{"delta": event.Chunk}}
	case kit.MessageEndEvent:
		mapped = Event{Type: "message_end", At: mapped.At, Payload: map[string]any{"content": event.Content}}
	case kit.ToolCallStartEvent:
		mapped = Event{Type: "tool_call_start", At: mapped.At, Payload: map[string]any{
			"id": event.ToolCallID, "name": event.ToolName, "kind": event.ToolKind,
		}}
	case kit.ToolCallDeltaEvent:
		mapped = Event{Type: "tool_args_delta", At: mapped.At, Payload: map[string]any{
			"id": event.ToolCallID, "delta": event.Delta,
		}}
	case kit.ToolCallEndEvent:
		mapped = Event{Type: "tool_call_end", At: mapped.At, Payload: map[string]any{"id": event.ToolCallID}}
	case kit.ToolCallEvent:
		mapped = Event{Type: "tool_call", At: mapped.At, Payload: map[string]any{
			"id": event.ToolCallID, "name": event.ToolName, "kind": event.ToolKind, "args": toolArgs(event.ToolArgs, event.ParsedArgs),
		}}
	case kit.ToolCallContentEvent:
		mapped = Event{Type: "tool_content", At: mapped.At, Payload: map[string]any{"content": event.Content}}
	case kit.ToolExecutionStartEvent:
		mapped = Event{Type: "tool_start", At: mapped.At, Payload: map[string]any{
			"id": event.ToolCallID, "name": event.ToolName, "kind": event.ToolKind, "args": event.ToolArgs,
		}}
	case kit.ToolResultEvent:
		mapped = Event{Type: "tool_result", At: mapped.At, Payload: map[string]any{
			"id": event.ToolCallID, "name": event.ToolName, "kind": event.ToolKind,
			"args": toolArgs(event.ToolArgs, event.ParsedArgs), "result": event.Result, "is_error": event.IsError,
		}}
	case kit.ToolOutputEvent:
		mapped = Event{Type: "tool_output", At: mapped.At, Payload: map[string]any{
			"id": event.ToolCallID, "name": event.ToolName, "chunk": event.Chunk, "is_stderr": event.IsStderr,
		}}
	case kit.StepUsageEvent:
		mapped = Event{Type: "step_usage", At: mapped.At, Payload: usagePayload(int64(event.InputTokens), int64(event.OutputTokens), int64(event.CacheReadTokens), int64(event.CacheWriteTokens), 0, 0)}
	case kit.StepFinishEvent:
		mapped = Event{Type: "step_finish", At: mapped.At, Payload: map[string]any{
			"step": event.StepNumber, "has_tool_calls": event.HasToolCalls,
			"finish_reason": event.FinishReason,
			"usage":         usagePayload(event.Usage.InputTokens, event.Usage.OutputTokens, event.Usage.CacheReadTokens, event.Usage.CacheCreationTokens, event.Usage.TotalTokens, event.Usage.ReasoningTokens),
		}}
	case kit.TurnEndEvent:
		mapped = Event{Type: "turn_end", At: mapped.At, Payload: map[string]any{
			"response": event.Response, "stop_reason": event.StopReason, "error": errorText(event.Error),
		}}
	case kit.ErrorEvent:
		mapped = Event{Type: "error", At: mapped.At, Payload: map[string]any{"error": errorText(event.Error)}}
	case kit.RetryEvent:
		mapped = Event{Type: "retry", At: mapped.At, Payload: map[string]any{
			"attempt": event.Attempt, "delay_ms": event.Delay.Milliseconds(), "error": errorText(event.Error),
		}}
	case kit.StepResumeEvent:
		mapped = Event{Type: "step_resume", At: mapped.At, Payload: map[string]any{
			"step": event.StepNumber, "resume": event.Resume,
			"delay_ms": event.Delay.Milliseconds(), "error": errorText(event.Error),
		}}
	case kit.WarningsEvent:
		mapped = Event{Type: "warnings", At: mapped.At, Payload: map[string]any{"warnings": event.Warnings}}
	case kit.CompactionEvent:
		mapped = Event{Type: "compaction", At: mapped.At, Payload: map[string]any{
			"summary": event.Summary, "original_tokens": event.OriginalTokens,
			"compacted_tokens": event.CompactedTokens, "messages_removed": event.MessagesRemoved,
			"error": errorText(event.Err),
		}}
	default:
		return
	}

	observer.Observe(ctx, mapped)
}

// toolArgs prefers the parsed arguments, which the deck renders as structured
// JSON, and falls back to the wire text when the call did not parse.
func toolArgs(raw string, parsed map[string]any) any {
	if parsed != nil {
		return parsed
	}
	return raw
}

// usagePayload is the one shape every usage event carries, so the deck can add
// the numbers without knowing which kit event produced them.
func usagePayload(input, output, cacheRead, cacheWrite, total, reasoning int64) map[string]any {
	usage := map[string]any{
		"input_tokens":  input,
		"output_tokens": output,
		"cache_read":    cacheRead,
		"cache_write":   cacheWrite,
	}
	if total > 0 {
		usage["total_tokens"] = total
	}
	if reasoning > 0 {
		usage["reasoning_tokens"] = reasoning
	}
	return usage
}

// errorText is the payload form of an error: nil becomes an empty string so the
// JSON stays a string field rather than sometimes null.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// maxProviderIssueBytes caps the provider message quoted in a session error.
const maxProviderIssueBytes = 300

// providerIssues remembers the last provider-side failure kit reported for one
// session. A review that ends without a review then says what the provider did,
// rather than reading like a model that chose to stop.
type providerIssues struct {
	mu   sync.Mutex
	last string
}

func (p *providerIssues) note(event kit.Event) {
	var err error
	switch event := event.(type) {
	case kit.ErrorEvent:
		err = event.Error
	case kit.RetryEvent:
		err = event.Error
	case kit.StepResumeEvent:
		err = event.Error
	}
	if err == nil {
		return
	}
	text := errorText(err)
	if len(text) > maxProviderIssueBytes {
		text = text[:maxProviderIssueBytes] + "…"
	}
	p.mu.Lock()
	p.last = text
	p.mu.Unlock()
}

func (p *providerIssues) summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}
