package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExampleConfigIsValid keeps rumil.example.yaml honest: the committed
// example must load and validate with placeholder values filled in.
func TestExampleConfigIsValid(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatalf("writing the placeholder key file: %v", err)
	}

	t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", keyPath)
	t.Setenv("GITHUB_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-key")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("DATABASE_URL", "postgres://rumil:rumil@localhost:5432/rumil?sslmode=disable")
	t.Setenv("RUMIL_ADMIN_TOKEN", "admin-token")

	path := filepath.Join("..", "..", "rumil.example.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}

	if cfg.GitHub.InstallationID != 98765432 {
		t.Errorf("installation_id = %d, want the example value", cfg.GitHub.InstallationID)
	}

	provider, err := cfg.Models.ChatProvider()
	if err != nil {
		t.Fatalf("ChatProvider returned error: %v", err)
	}
	if provider.ID != "deepseek-v4-flash" {
		t.Errorf("selected provider = %q, want the provider named by default_chat", provider.ID)
	}
	if provider.APIKey != "deepseek-key" {
		t.Errorf("api_key = %q, want the expanded environment value", provider.APIKey)
	}
}
