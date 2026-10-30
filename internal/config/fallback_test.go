package config

import (
	"strings"
	"testing"
)

// fallbackModels builds a model list with n chat providers plus one embedding
// provider, so the fallback list can be held against a real registry.
func fallbackModels(chats int) Models {
	models := Models{DefaultChat: "chat-0"}
	for i := 0; i < chats; i++ {
		models.Providers = append(models.Providers, Provider{
			ID:        "chat-" + string(rune('0'+i)),
			Provider:  "openai-compatible",
			BaseURL:   "https://chat.example.com/v1",
			ModelName: "model-" + string(rune('0'+i)),
			APIKey:    "key",
		})
	}
	models.Providers = append(models.Providers, Provider{
		ID: "embed", Provider: "openai-compatible", BaseURL: "https://embed.example.com/v1",
		ModelName: "embed", APIKey: "key", Dimensions: 1536,
	})
	return models
}

func TestFallbackAcceptsConfiguredChatProviders(t *testing.T) {
	models := fallbackModels(3)
	models.Fallback = []string{"chat-1", "chat-2"}
	if err := models.validate(); err != nil {
		t.Fatalf("validate returned error: %v", err)
	}
}

func TestFallbackRefusesUnknownAndEmbeddingProviders(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"unknown", "chat-z"},
		{"embedding", "embed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := fallbackModels(2)
			models.Fallback = []string{tc.id}
			if err := models.validate(); err == nil || !strings.Contains(err.Error(), tc.id) {
				t.Fatalf("validate error = %v, want it to name %q", err, tc.id)
			}
		})
	}
}

func TestFallbackRefusesDuplicates(t *testing.T) {
	models := fallbackModels(3)
	models.Fallback = []string{"chat-1", "chat-1"}
	if err := models.validate(); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("validate error = %v, want a duplicate refusal", err)
	}
}

func TestFallbackIsBounded(t *testing.T) {
	models := fallbackModels(6)
	models.Fallback = []string{"chat-0", "chat-1", "chat-2", "chat-3", "chat-4", "chat-5"}
	if err := models.validate(); err == nil || !strings.Contains(err.Error(), "at most 5") {
		t.Fatalf("validate error = %v, want the fallback cap", err)
	}
}
