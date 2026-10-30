package reviewer

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/kit/pkg/kit"

	"github.com/joangrigorov/rumil/internal/store"
)

// Bounds on the transcript a fallback carries. ContinuationBlock spends its
// own, smaller budget; these keep the in-memory capture from growing with a
// session that read large files or diffs.
const (
	transcriptCaptureEvents = 200
	transcriptCaptureBytes  = 256 << 10
	transcriptItemBytes     = 16 << 10
)

// transcriptCapture keeps the tool calls, results and notes an attempt made, in
// the shape ContinuationBlock renders. It is fed from the kit event stream, so
// a fallback inherits what the failed provider's attempt had already learned
// without reading the pull request again.
type transcriptCapture struct {
	mu    sync.Mutex
	items []store.ReviewEvent
	bytes int
}

func newTranscriptCapture() *transcriptCapture { return &transcriptCapture{} }

func (c *transcriptCapture) note(event kit.Event) {
	var eventType string
	var payload map[string]any

	switch event := event.(type) {
	case kit.ToolCallEvent:
		eventType = "tool_call"
		payload = map[string]any{
			"id": event.ToolCallID, "name": event.ToolName,
			"args": capturedText(toolArgs(event.ToolArgs, event.ParsedArgs)),
		}
	case kit.ToolResultEvent:
		eventType = "tool_result"
		payload = map[string]any{
			"id": event.ToolCallID, "name": event.ToolName,
			"args":     capturedText(toolArgs(event.ToolArgs, event.ParsedArgs)),
			"result":   capturedText(event.Result),
			"is_error": event.IsError,
		}
	case kit.MessageEndEvent:
		if strings.TrimSpace(event.Content) == "" {
			return
		}
		eventType = "message_end"
		payload = map[string]any{"content": capturedText(event.Content)}
	case kit.CompactionEvent:
		if strings.TrimSpace(event.Summary) == "" {
			return
		}
		eventType = "compaction"
		payload = map[string]any{"summary": capturedText(event.Summary)}
	case kit.ErrorEvent:
		if event.Error == nil {
			return
		}
		eventType = "session_error"
		payload = map[string]any{"error": capturedText(event.Error.Error())}
	default:
		return
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append(c.items, store.ReviewEvent{Type: eventType, At: time.Now(), Payload: encoded})
	c.bytes += len(encoded)
	for len(c.items) > 1 && (c.bytes > transcriptCaptureBytes || len(c.items) > transcriptCaptureEvents) {
		c.bytes -= len(c.items[0].Payload)
		c.items = c.items[1:]
	}
}

// events returns a copy of the captured transcript, for a Continuation.
func (c *transcriptCapture) events() []store.ReviewEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]store.ReviewEvent(nil), c.items...)
}

// capturedText clips one captured string. ContinuationBlock clips again to its
// own item budget, so this is only about the memory the capture holds.
func capturedText(value any) any {
	if text, ok := value.(string); ok {
		return clip(text, transcriptItemBytes)
	}
	return value
}
