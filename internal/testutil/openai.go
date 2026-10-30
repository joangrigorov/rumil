package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ToolCall is one tool invocation the fake model asks for.
type ToolCall struct {
	Name string
	Args map[string]any
}

// ScriptedTurn is one model reply: either tool calls or final text.
type ScriptedTurn struct {
	ToolCalls []ToolCall
	Text      string
	// FinishReason overrides the reason the reply reports. Empty means "stop"
	// for text and "tool_calls" for a call, which is what a normal step
	// reports. A test sets "length" to model a reply cut off at the output cap.
	FinishReason string
	// PromptTokens overrides the usage this reply reports for its input. A
	// test sets it to move the session's estimated context past a compaction
	// trigger; zero keeps the default.
	PromptTokens int
}

// Request is a captured model request.
type Request struct {
	Method string
	Path   string
	Stream bool
	Model  string
	Tools  []string
	Raw    []byte
	// SystemPrompt is the concatenated system message, which shows what the
	// reviewer prompt carried into the session.
	SystemPrompt string
	// UserPrompt is the concatenated user message, which is the task the
	// session was asked to do.
	UserPrompt string
	// ToolCallNames lists the tool calls requested by the assistant messages in
	// this request, which shows whether tool results were fed back.
	ToolCallNames []string
}

// FakeLLM is an OpenAI-compatible chat completions endpoint driven by a script.
type FakeLLM struct {
	Server *httptest.Server

	mu        sync.Mutex
	requests  []Request
	turns     []ScriptedTurn
	defaultTx string
	unknown   []string
	rejected  []string

	// embeddings are the vectors queued for the next embedding calls, and
	// embedded is every input those calls carried.
	embeddings    [][]float32
	embedded      []string
	embeddingFail int
	// failures are error messages answered for the next chat requests, oldest
	// first, so a test can drive kit's retry path.
	failures []string
}

// QueueFailure makes the next chat request answer 503 with message.
func (f *FakeLLM) QueueFailure(message string) *FakeLLM {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, message)
	return f
}

// NewFakeLLM starts the fake endpoint. When the script runs out it replies with
// a neutral final message.
func NewFakeLLM(t *testing.T) *FakeLLM {
	t.Helper()

	f := &FakeLLM{defaultTx: "No further changes requested."}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Server.Close)
	return f
}

// URL is the provider base URL to configure.
func (f *FakeLLM) URL() string { return f.Server.URL }

// QueueToolCall appends a reply that calls one tool.
func (f *FakeLLM) QueueToolCall(name string, args map[string]any) *FakeLLM {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, ScriptedTurn{ToolCalls: []ToolCall{{Name: name, Args: args}}})
	return f
}

// QueueText appends a reply with final text.
func (f *FakeLLM) QueueText(text string) *FakeLLM {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, ScriptedTurn{Text: text})
	return f
}

// QueueTurn appends a fully specified reply, including the usage it reports.
func (f *FakeLLM) QueueTurn(turn ScriptedTurn) *FakeLLM {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, turn)
	return f
}

// Requests returns a copy of every request seen.
func (f *FakeLLM) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// Count returns how many model requests were made.
func (f *FakeLLM) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// UnknownPaths returns paths that no handler matched, which surfaces a wrong
// provider wire protocol immediately.
func (f *FakeLLM) UnknownPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unknown...)
}

// RejectedSchemas returns the tool schemas this endpoint refused, in order. An
// empty result means every request carried a schema a validating provider
// accepts; a session that did not run because of one has its cause here.
func (f *FakeLLM) RejectedSchemas() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rejected...)
}

// ToolSchemaOf returns the schema the provider was shown for one tool of the
// most recent request, which is what a test reads when it cares about the shape
// rather than that a call happened.
func (f *FakeLLM) ToolSchemaOf(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	var body map[string]any
	if err := json.Unmarshal(f.requests[len(f.requests)-1].Raw, &body); err != nil {
		return nil
	}
	return ToolSchemas(body)[name]
}

// ToolNames returns the tools offered on the most recent request.
func (f *FakeLLM) ToolNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1].Tools
}

// ToolResults returns the tool messages the model received on its most recent
// request, in the order they were answered, which is what a test asserts on when
// it cares about what a tool said rather than that it was called.
func (f *FakeLLM) ToolResults() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return toolResultsFrom(f.requests[len(f.requests)-1].Raw)
}

func toolResultsFrom(raw []byte) []string {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}

	messages, _ := body["messages"].([]any)
	var results []string
	for _, m := range messages {
		message, _ := m.(map[string]any)
		if role, _ := message["role"].(string); role != "tool" {
			continue
		}
		if content, ok := message["content"].(string); ok {
			results = append(results, content)
		}
	}
	return results
}

// OfferedToolNames returns the union of tools offered across all requests.
func (f *FakeLLM) OfferedToolNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	var names []string
	for _, r := range f.requests {
		for _, name := range r.Tools {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}

func (f *FakeLLM) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))

	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	if strings.HasSuffix(r.URL.Path, "/embeddings") {
		f.handleEmbeddings(w, raw)
		return
	}

	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		f.mu.Lock()
		f.unknown = append(f.unknown, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		http.Error(w, `{"error":{"message":"unsupported endpoint"}}`, http.StatusNotFound)
		return
	}

	f.mu.Lock()
	failure := ""
	if len(f.failures) > 0 {
		failure, f.failures = f.failures[0], f.failures[1:]
	}
	f.mu.Unlock()
	if failure != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": failure}})
		return
	}

	// A provider that validates tool schemas answers this before it looks at
	// the conversation, so the double refuses the same requests (see
	// ValidateToolSchemas).
	if err := ValidateToolSchemas(body); err != nil {
		f.mu.Lock()
		f.rejected = append(f.rejected, err.Error())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}

	req := Request{Method: r.Method, Path: r.URL.Path, Raw: raw}
	req.Model, _ = body["model"].(string)
	if stream, ok := body["stream"].(bool); ok {
		req.Stream = stream
	}
	req.Tools = toolNamesFrom(body)
	req.ToolCallNames = toolCallNamesFrom(body)
	req.SystemPrompt = systemPromptFrom(body)
	req.UserPrompt = userPromptFrom(body)

	f.mu.Lock()
	f.requests = append(f.requests, req)
	var turn ScriptedTurn
	if isSummarizationRequest(req.SystemPrompt) {
		// Compaction asks the same endpoint for a summary. Answer it directly
		// so a test's script stays about the review itself.
		turn = ScriptedTurn{Text: "## Goal\n- summarized the review context"}
	} else {
		turn = f.nextTurnLocked()
	}
	f.mu.Unlock()

	if req.Stream {
		writeStream(w, turn)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(completion(turn))
}

// isSummarizationRequest reports whether a request is kit's compaction
// summary prompt rather than a step of the review.
func isSummarizationRequest(systemPrompt string) bool {
	return strings.Contains(systemPrompt, "You are a context summarization assistant.")
}

func (f *FakeLLM) nextTurnLocked() ScriptedTurn {
	if len(f.turns) == 0 {
		return ScriptedTurn{Text: f.defaultTx}
	}
	turn := f.turns[0]
	f.turns = f.turns[1:]
	return turn
}

func toolNamesFrom(body map[string]any) []string {
	tools, _ := body["tools"].([]any)
	var names []string
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if name, ok := fn["name"].(string); ok {
			names = append(names, name)
		}
	}
	return names
}

func toolCallNamesFrom(body map[string]any) []string {
	messages, _ := body["messages"].([]any)
	var names []string
	for _, m := range messages {
		msg, _ := m.(map[string]any)
		calls, _ := msg["tool_calls"].([]any)
		for _, c := range calls {
			call, _ := c.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			if name, ok := fn["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

func systemPromptFrom(body map[string]any) string {
	return messageContentFrom(body, "system")
}

func userPromptFrom(body map[string]any) string {
	return messageContentFrom(body, "user")
}

func messageContentFrom(body map[string]any, role string) string {
	messages, _ := body["messages"].([]any)
	var parts []string
	for _, m := range messages {
		msg, _ := m.(map[string]any)
		if got, _ := msg["role"].(string); got != role {
			continue
		}
		if content, ok := msg["content"].(string); ok {
			parts = append(parts, content)
		}
	}
	return strings.Join(parts, "\n")
}

func completion(turn ScriptedTurn) map[string]any {
	promptTokens := turn.PromptTokens
	if promptTokens == 0 {
		promptTokens = 10
	}

	message := map[string]any{"role": "assistant"}
	finish := "stop"

	if len(turn.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(turn.ToolCalls))
		for i, tc := range turn.ToolCalls {
			args, _ := json.Marshal(tc.Args)
			calls = append(calls, map[string]any{
				"id":   fmt.Sprintf("call_%d", i+1),
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(args),
				},
			})
		}
		message["tool_calls"] = calls
		message["content"] = nil
		finish = "tool_calls"
	} else {
		message["content"] = turn.Text
	}
	if turn.FinishReason != "" {
		finish = turn.FinishReason
	}

	return map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "test-model",
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": map[string]int{"prompt_tokens": promptTokens, "completion_tokens": 5, "total_tokens": promptTokens + 5},
	}
}

// finishOf is the reason a scripted turn reports, which is its own override
// when it has one and the normal reason for its shape otherwise.
func finishOf(turn ScriptedTurn, fallback string) string {
	if turn.FinishReason != "" {
		return turn.FinishReason
	}
	return fallback
}

func writeStream(w http.ResponseWriter, turn ScriptedTurn) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	send := func(payload map[string]any) {
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "test-model",
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}

	send(chunk(map[string]any{"role": "assistant"}, nil))
	if len(turn.ToolCalls) > 0 {
		for i, tc := range turn.ToolCalls {
			args, _ := json.Marshal(tc.Args)
			send(chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": i,
				"id":    fmt.Sprintf("call_%d", i+1),
				"type":  "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(args),
				},
			}}}, nil))
		}
		send(chunk(map[string]any{}, finishOf(turn, "tool_calls")))
	} else {
		send(chunk(map[string]any{"content": turn.Text}, nil))
		send(chunk(map[string]any{}, finishOf(turn, "stop")))
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
