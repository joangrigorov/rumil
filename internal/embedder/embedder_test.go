package embedder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/config"
)

// testDimensions is the width these tests serve and expect. The width is the
// provider's, so the client checks against whatever its provider declares;
// TestEmbedRunsAtTheProviderWidth shows a second one.
const (
	testKey        = "test-key"
	testDimensions = 1536
)

// vector returns values that are exact in binary floating point, so a JSON
// round trip preserves them.
func vector(width int) []float32 {
	out := make([]float32, width)
	for i := range out {
		out[i] = float32(i%7) / 4
	}
	return out
}

func newProvider(t *testing.T, handler http.HandlerFunc) *config.Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &config.Provider{
		ID:         "embed",
		Provider:   "openai-compatible",
		BaseURL:    server.URL,
		ModelName:  "text-embedding-3-small",
		APIKey:     testKey,
		Dimensions: testDimensions,
	}
}

func writeEmbedding(t *testing.T, w http.ResponseWriter, values []float32) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]any{"data": []map[string]any{{"embedding": values}}}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("encoding the response: %v", err)
	}
}

func TestEmbedSendsTheConfiguredRequestAndReturnsTheVector(t *testing.T) {
	var (
		method, path, auth, contentType string
		body                            struct{ Model, Input string }
	)
	provider := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		writeEmbedding(t, w, vector(testDimensions))
	})

	client, err := New(provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := client.Embed(context.Background(), "we allow 60s of clock skew here")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/embeddings" {
		t.Errorf("path = %q, want /embeddings", path)
	}
	if auth != "Bearer "+testKey {
		t.Errorf("authorization header = %q, want the bearer token", auth)
	}
	if contentType != "application/json" {
		t.Errorf("content type = %q, want application/json", contentType)
	}
	if body.Model != provider.ModelName {
		t.Errorf("model = %q, want %q", body.Model, provider.ModelName)
	}
	if body.Input != "we allow 60s of clock skew here" {
		t.Errorf("input = %q, want the text that was passed in", body.Input)
	}
	if client.Dimension() != testDimensions {
		t.Errorf("Dimension() = %d, want %d", client.Dimension(), testDimensions)
	}

	want := vector(testDimensions)
	if len(got) != len(want) {
		t.Fatalf("vector width = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("vector[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// The width belongs to the provider, not to the runtime: a 768-dimension model
// is served by a provider that declares 768 (project_plan.md §3 rule 5), which
// is what lets memory run on a local model whose vectors are narrower than
// OpenAI's.
func TestEmbedRunsAtTheProviderWidth(t *testing.T) {
	const localWidth = 768

	provider := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeEmbedding(t, w, vector(localWidth))
	})
	provider.Dimensions = localWidth

	client, err := New(provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.Dimension() != localWidth {
		t.Errorf("Dimension() = %d, want %d", client.Dimension(), localWidth)
	}

	got, err := client.Embed(context.Background(), "we allow 60s of clock skew here")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != localWidth {
		t.Fatalf("vector width = %d, want %d", len(got), localWidth)
	}
}

func TestEmbedRejectsAWidthTheColumnCannotStore(t *testing.T) {
	provider := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeEmbedding(t, w, []float32{0.5, 0.25, 0.125})
	})

	client, err := New(provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Embed(context.Background(), "text")
	if err == nil {
		t.Fatal("Embed accepted a 3-dimension vector, want an error")
	}
	if !strings.Contains(err.Error(), "want 1536") {
		t.Errorf("error = %q, want it to name the expected width", err)
	}
}

func TestEmbedReportsTheProviderMessageWithoutTheAPIKey(t *testing.T) {
	provider := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		if _, err := w.Write([]byte(`{"error":{"message":"rate limit reached"}}`)); err != nil {
			t.Errorf("writing the response: %v", err)
		}
	})

	client, err := New(provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Embed(context.Background(), "text")
	if err == nil {
		t.Fatal("Embed accepted a 429, want an error")
	}
	if !strings.Contains(err.Error(), "rate limit reached") {
		t.Errorf("error = %q, want the provider's message", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error = %q, want no credentials in it", err)
	}
}

func TestEmbedTruncatesAnOversizedInput(t *testing.T) {
	var received int
	provider := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		received = len(body.Input)
		writeEmbedding(t, w, vector(testDimensions))
	})

	client, err := New(provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Embed(context.Background(), strings.Repeat("a", 100<<10)); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if received != maxInputBytes {
		t.Errorf("input length = %d, want it capped at %d", received, maxInputBytes)
	}
}

func TestEmbedRejectsAnEmptyDataArray(t *testing.T) {
	provider := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"data":[]}`)); err != nil {
			t.Errorf("writing the response: %v", err)
		}
	})

	client, err := New(provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Embed(context.Background(), "text"); err == nil {
		t.Fatal("Embed accepted an empty data array, want an error")
	}
}

func TestNewRejectsAProviderThatIsNotAnEmbedder(t *testing.T) {
	_, err := New(&config.Provider{ID: "chat", BaseURL: "http://example.invalid", ModelName: "chat-model"})
	if err == nil {
		t.Fatal("New accepted a provider without dimensions, want an error")
	}
	if !strings.Contains(err.Error(), "no dimensions") {
		t.Errorf("error = %q, want it to name the missing dimensions", err)
	}
}

func TestNewRejectsAnIncompleteProvider(t *testing.T) {
	_, err := New(&config.Provider{ID: "embed", Dimensions: testDimensions})
	if err == nil {
		t.Fatal("New accepted a provider without a base URL, want an error")
	}
}

func TestNewRejectsNil(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) succeeded, want an error")
	}
}
