package reviewer

import (
	"context"
	"errors"
	"testing"

	kit "github.com/mark3labs/kit/pkg/kit"
)

type captureObserver struct {
	events []Event
}

func (c *captureObserver) Observe(_ context.Context, event Event) {
	c.events = append(c.events, event)
}

func TestObserveKitEventMapsTheSession(t *testing.T) {
	engine := &Engine{}
	observer := &captureObserver{}
	ctx := context.Background()

	engine.observeKitEvent(ctx, observer, kit.StepStartEvent{StepNumber: 2})
	engine.observeKitEvent(ctx, observer, kit.ReasoningDeltaEvent{Delta: "weigh the options"})
	engine.observeKitEvent(ctx, observer, kit.ToolCallStartEvent{ToolCallID: "call_1", ToolName: "read_diff", ToolKind: "read"})
	engine.observeKitEvent(ctx, observer, kit.ToolCallEvent{
		ToolCallID: "call_1", ToolName: "read_diff", ToolKind: "read",
		ToolArgs: `{"path":"a.go"}`, ParsedArgs: map[string]any{"path": "a.go"},
	})
	engine.observeKitEvent(ctx, observer, kit.ToolResultEvent{
		ToolCallID: "call_1", ToolName: "read_diff", ToolKind: "read",
		ParsedArgs: map[string]any{"path": "a.go"}, Result: "a patch", IsError: false,
	})
	engine.observeKitEvent(ctx, observer, kit.StepFinishEvent{
		StepNumber: 2, HasToolCalls: true, FinishReason: "tool_calls",
		Usage: kit.LLMUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	engine.observeKitEvent(ctx, observer, kit.TurnEndEvent{Error: errors.New("the provider closed the stream")})
	engine.observeKitEvent(ctx, observer, kit.ResponseEvent{Content: "unmapped"})

	if len(observer.events) != 7 {
		t.Fatalf("events = %d, want 7 (ResponseEvent is not part of the deck)", len(observer.events))
	}

	wantTypes := []string{"step_start", "reasoning_delta", "tool_call_start", "tool_call", "tool_result", "step_finish", "turn_end"}
	for i, want := range wantTypes {
		if observer.events[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, observer.events[i].Type, want)
		}
	}

	if step, ok := observer.events[0].Payload["step"].(int); !ok || step != 2 {
		t.Errorf("step_start payload = %v, want step 2", observer.events[0].Payload)
	}
	if delta := observer.events[1].Payload["delta"]; delta != "weigh the options" {
		t.Errorf("reasoning delta = %v", delta)
	}

	call := observer.events[3].Payload
	args, ok := call["args"].(map[string]any)
	if !ok || args["path"] != "a.go" {
		t.Errorf("tool call args = %v, want the parsed map", call["args"])
	}

	result := observer.events[4].Payload
	if result["result"] != "a patch" || result["is_error"] != false {
		t.Errorf("tool result payload = %v", result)
	}

	usage, ok := observer.events[5].Payload["usage"].(map[string]any)
	if !ok {
		t.Fatalf("step_finish usage = %v, want a map", observer.events[5].Payload["usage"])
	}
	if usage["input_tokens"] != int64(10) || usage["output_tokens"] != int64(5) || usage["total_tokens"] != int64(15) {
		t.Errorf("usage = %v, want 10 in, 5 out, 15 total", usage)
	}

	if got := observer.events[6].Payload["error"]; got != "the provider closed the stream" {
		t.Errorf("turn_end error = %v", got)
	}
}

func TestToolArgsPrefersTheParsedMap(t *testing.T) {
	parsed := map[string]any{"path": "a.go"}
	if got := toolArgs(`{"path":"a.go"}`, parsed); got == nil {
		t.Fatal("toolArgs returned nil")
	} else if args, ok := got.(map[string]any); !ok || args["path"] != "a.go" {
		t.Errorf("toolArgs(parsed) = %v, want the map", got)
	}

	if got := toolArgs(`{"path":"a.go"}`, nil); got != `{"path":"a.go"}` {
		t.Errorf("toolArgs(raw) = %v, want the wire text", got)
	}
}
