package config

import (
	"strconv"
	"strings"
	"testing"
)

const installationModels = `
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
    - id: "other"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "other"
      api_key: "k"
`

func multiInstallationDoc(github string) string {
	return "github:\n" + github + installationModels
}

func TestInstallationSetKeepsTheSingleForm(t *testing.T) {
	cfg := mustParse(t, `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
`+installationModels)

	ids := cfg.GitHub.InstallationIDs()
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("InstallationIDs = %v, want [2]", ids)
	}
	if got := cfg.GitHub.PrimaryInstallationID(); got != 2 {
		t.Errorf("PrimaryInstallationID = %d, want 2", got)
	}
	if !cfg.GitHub.HasInstallation(2) || cfg.GitHub.HasInstallation(3) {
		t.Error("HasInstallation does not describe the configured set")
	}
	if settings := cfg.GitHub.InstallationByID(2); settings != nil {
		t.Errorf("InstallationByID(2) = %+v, want nil for the legacy form", settings)
	}
}

func TestInstallationSetListsTheLegacyInstallationFirst(t *testing.T) {
	cfg := mustParse(t, multiInstallationDoc(`  app_id: 1
  installation_id: 2
  private_key: "key"
  installations:
    - id: 3
      name: "second"
      chat_provider: "other"
      worker_concurrency: 2
      max_total_tokens: 100000
      max_cost_usd: 1.5
`))

	if got, want := cfg.GitHub.InstallationIDs(), []int64{2, 3}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("InstallationIDs = %v, want %v", got, want)
	}
	if got := cfg.GitHub.PrimaryInstallationID(); got != 2 {
		t.Errorf("PrimaryInstallationID = %d, want the legacy installation 2", got)
	}
	if settings := cfg.GitHub.InstallationByID(3); settings == nil {
		t.Fatal("InstallationByID(3) = nil, want the listed settings")
	} else {
		if settings.Name != "second" || settings.ChatProvider != "other" {
			t.Errorf("settings = %+v, want the configured name and provider", settings)
		}
		if settings.WorkerConcurrency != 2 || settings.MaxTotalTokens != 100000 || settings.MaxCostUSD != 1.5 {
			t.Errorf("settings = %+v, want the configured limits", settings)
		}
	}
	if settings := cfg.GitHub.InstallationByID(2); settings != nil {
		t.Errorf("InstallationByID(2) = %+v, want nil for the unnamed legacy installation", settings)
	}
}

func TestInstallationSetWithoutTheLegacyID(t *testing.T) {
	cfg := mustParse(t, multiInstallationDoc(`  app_id: 1
  private_key: "key"
  installations:
    - id: 7
      name: "only"
`))

	if got := cfg.GitHub.PrimaryInstallationID(); got != 7 {
		t.Errorf("PrimaryInstallationID = %d, want the first listed installation 7", got)
	}
}

func TestInstallationsAreValidated(t *testing.T) {
	tests := []struct {
		name    string
		github  string
		wantSub string
	}{
		{
			name:    "no installation at all",
			github:  "  app_id: 1\n  private_key: \"key\"\n",
			wantSub: "github.installation_id or github.installations",
		},
		{
			name: "a non-positive id",
			github: `  app_id: 1
  private_key: "key"
  installations:
    - id: 0
      name: "zero"
`,
			wantSub: "github.installations[0].id",
		},
		{
			name: "a duplicate of the legacy id",
			github: `  app_id: 1
  installation_id: 2
  private_key: "key"
  installations:
    - id: 2
      name: "again"
`,
			wantSub: "repeats installation 2",
		},
		{
			name: "a duplicate among the listed installations",
			github: `  app_id: 1
  private_key: "key"
  installations:
    - id: 3
      name: "one"
    - id: 3
      name: "two"
`,
			wantSub: "repeats installation 3",
		},
		{
			name: "a missing name",
			github: `  app_id: 1
  private_key: "key"
  installations:
    - id: 3
      name: " "
`,
			wantSub: "name is required",
		},
		{
			name: "a name that is not a handle",
			github: `  app_id: 1
  private_key: "key"
  installations:
    - id: 3
      name: "two accounts"
`,
			wantSub: "must be 1-64",
		},
		{
			name: "a duplicate name",
			github: `  app_id: 1
  private_key: "key"
  installations:
    - id: 3
      name: "same"
    - id: 4
      name: "same"
`,
			wantSub: `two installations named "same"`,
		},
		{
			name: "a negative worker concurrency",
			github: `  app_id: 1
  private_key: "key"
  installations:
    - id: 3
      name: "one"
      worker_concurrency: -1
`,
			wantSub: "worker_concurrency",
		},
		{
			name: "a negative token budget",
			github: `  app_id: 1
  private_key: "key"
  installations:
    - id: 3
      name: "one"
      max_total_tokens: -5
`,
			wantSub: "max_total_tokens",
		},
		{
			name: "a negative cost budget",
			github: `  app_id: 1
  private_key: "key"
  installations:
    - id: 3
      name: "one"
      max_cost_usd: -1
`,
			wantSub: "max_cost_usd",
		},
		{
			name: "an unknown chat provider",
			github: `  app_id: 1
  private_key: "key"
  installations:
    - id: 3
      name: "one"
      chat_provider: "nope"
`,
			wantSub: "github.installations[0].chat_provider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(multiInstallationDoc(tt.github)), "test.yaml")
			if err == nil {
				t.Fatal("Parse succeeded, want the configuration refused")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantSub)
			}
		})
	}
}

func TestInstallationsAreCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("github:\n  app_id: 1\n  installation_id: 2\n  private_key: \"key\"\n  installations:\n")
	for i := 1; i <= maxInstallations; i++ {
		b.WriteString("    - id: ")
		b.WriteString(strconv.Itoa(i + 100))
		b.WriteString("\n      name: \"account-")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\"\n")
	}
	b.WriteString(installationModels)

	if _, err := Parse([]byte(b.String()), "test.yaml"); err == nil {
		t.Fatal("Parse succeeded, want more installations than the cap refused")
	} else if !strings.Contains(err.Error(), "want at most") {
		t.Errorf("error = %v, want it to name the cap", err)
	}
}
