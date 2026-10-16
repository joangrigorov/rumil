// Package embedder turns text into the vectors that `memories.embedding`
// stores, through the OpenAI-compatible embeddings endpoint of one configured
// provider. Memory is opt-in, so nothing builds a Client unless
// models.default_embedding names an embedding provider (project_plan.md §3
// rule 6).
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joangrigorov/rumil/internal/config"
)

// maxInputBytes caps one request. The endpoint accepts more; the cap keeps a
// pathological input from becoming an expensive call.
const maxInputBytes = 24 << 10

// maxErrorBytes bounds how much of a failed response is quoted back.
const maxErrorBytes = 4 << 10

// maxResponseBytes bounds a successful response. A wide vector, 1536
// dimensions or more, is tens of KB of JSON.
const maxResponseBytes = 1 << 20

// requestTimeout bounds one embeddings call.
const requestTimeout = 30 * time.Second

// Client embeds text through one provider.
type Client struct {
	http      *http.Client
	id        string
	baseURL   string
	model     string
	apiKey    string
	dimension int
}

// New builds a client for an embedding provider. It fails when the provider
// declares no dimensions, because that is the width the column memory searches
// is sized to.
func New(provider *config.Provider) (*Client, error) {
	if provider == nil {
		return nil, errors.New("embedder: no provider")
	}
	if !provider.IsEmbedding() {
		return nil, fmt.Errorf("embedder: provider %q declares no dimensions", provider.ID)
	}
	if provider.BaseURL == "" || provider.ModelName == "" {
		return nil, fmt.Errorf("embedder: provider %q needs base_url and model_name", provider.ID)
	}
	return &Client{
		http:      &http.Client{Timeout: requestTimeout},
		id:        provider.ID,
		baseURL:   strings.TrimRight(provider.BaseURL, "/"),
		model:     provider.ModelName,
		apiKey:    provider.APIKey,
		dimension: provider.Dimensions,
	}, nil
}

// Dimension is the vector width this client produces.
func (c *Client) Dimension() int { return c.dimension }

// Embed returns the vector for text. A response whose width differs from the
// configured dimension is an error rather than an INSERT that fails later.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embeddingRequest{Model: c.model, Input: truncate(text)})
	if err != nil {
		return nil, fmt.Errorf("embedder: encoding the request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedder: building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedder: calling %s: %w", c.id, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("embedder: %s answered %s: %s", c.id, resp.Status, errorText(resp.Body))
	}

	var payload embeddingResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("embedder: decoding the response from %s: %w", c.id, err)
	}
	if len(payload.Data) == 0 {
		return nil, fmt.Errorf("embedder: %s returned no embedding", c.id)
	}

	vector := payload.Data[0].Embedding
	if len(vector) != c.dimension {
		return nil, fmt.Errorf("embedder: %s returned %d dimensions, want %d", c.id, len(vector), c.dimension)
	}
	return vector, nil
}

type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// errorText reports a failed response in one line, preferring the provider's
// own message.
func errorText(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBytes))
	if err != nil {
		return "unreadable response body"
	}

	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Error.Message != "" {
		return payload.Error.Message
	}

	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "empty response body"
	}
	return text
}

// truncate caps an input on a UTF-8 boundary so the request carries no partial
// rune.
func truncate(text string) string {
	if len(text) <= maxInputBytes {
		return text
	}
	cut := text[:maxInputBytes]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut
}
