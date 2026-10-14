package testutil

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"

	"github.com/joangrigorov/rumil/internal/config"
)

// BasisVector returns a vector orthogonal to every other basis vector, so two
// of them are unrelated rules and the same one is an exact match. It is what a
// test seeds a memory with and what it scripts the embedder to answer.
func BasisVector(index int) []float32 {
	vector := make([]float32, config.EmbeddingDimensions)
	vector[index%config.EmbeddingDimensions] = 1
	return vector
}

// QueueEmbedding queues the vector the next embedding call returns. Calls
// beyond the queue answer with BasisVector of a hash of their input, so equal
// text maps to an equal vector and the values stay stable between runs.
func (f *FakeLLM) QueueEmbedding(vector []float32) *FakeLLM {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.embeddings = append(f.embeddings, vector)
	return f
}

// FailEmbeddings answers every embedding call with status, which is how a
// provider outage is simulated.
func (f *FakeLLM) FailEmbeddings(status int) *FakeLLM {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.embeddingFail = status
	return f
}

// EmbeddedTexts returns the input of every embedding call, in order, which is
// what shows the text a selection was made from.
func (f *FakeLLM) EmbeddedTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.embedded...)
}

func (f *FakeLLM) handleEmbeddings(w http.ResponseWriter, raw []byte) {
	var body struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	f.embedded = append(f.embedded, body.Input)
	fail := f.embeddingFail
	vector := BasisVector(embeddingIndex(body.Input))
	if len(f.embeddings) > 0 {
		vector, f.embeddings = f.embeddings[0], f.embeddings[1:]
	}
	f.mu.Unlock()

	if fail != 0 {
		w.WriteHeader(fail)
		_, _ = fmt.Fprint(w, `{"error":{"message":"the embedding provider is unavailable"}}`)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"model":  body.Model,
		"data":   []map[string]any{{"object": "embedding", "index": 0, "embedding": vector}},
		"usage":  map[string]int{"prompt_tokens": 3, "total_tokens": 3},
	})
}

// embeddingIndex spreads a text over the dimensions deterministically, so the
// same text always lands on the same vector.
func embeddingIndex(text string) int {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(text))
	return int(hash.Sum32() % config.EmbeddingDimensions)
}
