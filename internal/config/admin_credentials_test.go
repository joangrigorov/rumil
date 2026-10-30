package config

import (
	"fmt"
	"strings"
	"testing"
)

// A lone admin_token keeps working, and it is one named entry rather than a
// separate code path.
func TestAdminTokenIsALegacyCredential(t *testing.T) {
	cfg := mustParse(t, twoChatOneEmbedding+`
moderation:
  admin_token: "one-token"
`)
	credentials := cfg.Moderation.AdminCredentials()
	if len(credentials) != 1 {
		t.Fatalf("credentials = %+v, want one", credentials)
	}
	if credentials[0].Name != "legacy" || credentials[0].Token != "one-token" {
		t.Errorf("credential = %+v, want legacy/one-token", credentials[0])
	}
}

// Named credentials are listed as configured, and the legacy token keeps its
// place in front when both forms are present.
func TestNamedAdminCredentials(t *testing.T) {
	cfg := mustParse(t, twoChatOneEmbedding+`
moderation:
  admin_token: "one-token"
  admin_tokens:
    - name: "alice"
      token: "alice-token"
    - name: "ci.bot-2"
      token: "bot-token"
`)
	credentials := cfg.Moderation.AdminCredentials()
	want := []AdminCredential{
		{Name: "legacy", Token: "one-token"},
		{Name: "alice", Token: "alice-token"},
		{Name: "ci.bot-2", Token: "bot-token"},
	}
	if len(credentials) != len(want) {
		t.Fatalf("credentials = %+v, want %+v", credentials, want)
	}
	for i := range want {
		if credentials[i] != want[i] {
			t.Errorf("credential %d = %+v, want %+v", i, credentials[i], want[i])
		}
	}
}

// An admin_tokens-only configuration is valid: the singular field is the
// backward-compatible form, not the required one.
func TestAdminTokensWithoutTheLegacyToken(t *testing.T) {
	cfg := mustParse(t, twoChatOneEmbedding+`
moderation:
  admin_tokens:
    - name: "alice"
      token: "alice-token"
`)
	if got := cfg.Moderation.AdminCredentials(); len(got) != 1 || got[0].Name != "alice" {
		t.Errorf("credentials = %+v, want alice only", got)
	}
}

// A set that cannot identify exactly one holder is refused at load, so an
// ambiguous audit trail never reaches a request.
func TestAdminCredentialValidation(t *testing.T) {
	cases := []struct {
		name       string
		moderation string
	}{
		{"a missing name", "  admin_tokens:\n    - token: \"a-token\"\n"},
		{"an empty name", "  admin_tokens:\n    - name: \"  \"\n      token: \"a-token\"\n"},
		{"a name that is not a handle", "  admin_tokens:\n    - name: \"alice smith\"\n      token: \"a-token\"\n"},
		{"a name over the length", "  admin_tokens:\n    - name: \"" + strings.Repeat("a", 65) + "\"\n      token: \"a-token\"\n"},
		{"a missing token", "  admin_tokens:\n    - name: \"alice\"\n"},
		{"a duplicate name", "  admin_tokens:\n    - name: \"alice\"\n      token: \"a-token\"\n    - name: \"alice\"\n      token: \"b-token\"\n"},
		{"a duplicate token", "  admin_tokens:\n    - name: \"alice\"\n      token: \"same\"\n    - name: \"bob\"\n      token: \"same\"\n"},
		{"a named duplicate of legacy", "  admin_token: \"one-token\"\n  admin_tokens:\n    - name: \"legacy\"\n      token: \"another\"\n"},
		{"a named token equal to legacy", "  admin_token: \"one-token\"\n  admin_tokens:\n    - name: \"alice\"\n      token: \"one-token\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(twoChatOneEmbedding+"\nmoderation:\n"+tc.moderation), "test.yaml"); err == nil {
				t.Fatal("Parse accepted an ambiguous credential set")
			}
		})
	}

	// One more than the cap is refused, and the cap itself is accepted.
	var atCap strings.Builder
	for i := 0; i < maxAdminCredentials; i++ {
		fmt.Fprintf(&atCap, "    - name: \"holder-%d\"\n      token: \"token-%d\"\n", i, i)
	}
	if _, err := Parse([]byte(twoChatOneEmbedding+"\nmoderation:\n  admin_tokens:\n"+atCap.String()), "test.yaml"); err != nil {
		t.Errorf("the cap of %d credentials was refused: %v", maxAdminCredentials, err)
	}
	tooMany := atCap.String() + "    - name: \"one-too-many\"\n      token: \"one-too-many-token\"\n"
	if _, err := Parse([]byte(twoChatOneEmbedding+"\nmoderation:\n  admin_tokens:\n"+tooMany), "test.yaml"); err == nil {
		t.Errorf("Parse accepted %d credentials, want at most %d", maxAdminCredentials+1, maxAdminCredentials)
	}
}
