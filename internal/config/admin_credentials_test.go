package config

import (
	"fmt"
	"strings"
	"testing"
)

// testPasswordHash is the bcrypt digest of "test-admin-password". A hash starts
// with $2a$, which the loader must leave intact while expanding ${VAR}
// placeholders.
const testPasswordHash = "$2a$10$HEbCK2/.V0nBjO/13L3Mu.OnAoDOLDZdUZEg0irk5uO0jTZhw0Rba"

// The deck users are named, and each carries a bcrypt hash rather than a
// password.
func TestAdminUsersAreNamed(t *testing.T) {
	cfg := mustParse(t, twoChatOneEmbedding+`
moderation:
  admin_users:
    - name: "alice"
      password_hash: "`+testPasswordHash+`"
    - name: "ci.bot-2"
      password_hash: "`+testPasswordHash+`"
`)
	users := cfg.Moderation.Users()
	if len(users) != 2 {
		t.Fatalf("users = %+v, want two", users)
	}
	if users[0].Name != "alice" || users[1].Name != "ci.bot-2" {
		t.Errorf("users = %+v, want alice and ci.bot-2", users)
	}
	if users[0].PasswordHash != testPasswordHash {
		t.Errorf("password hash = %q, want the configured hash", users[0].PasswordHash)
	}
}

// The old token fields are gone. A file that still carries one is refused at
// load rather than quietly running without a login.
func TestLegacyTokenFieldsAreRefused(t *testing.T) {
	for _, field := range []string{"admin_token", "admin_tokens"} {
		t.Run(field, func(t *testing.T) {
			doc := twoChatOneEmbedding + "\nmoderation:\n  " + field + ":\n    - name: alice\n"
			_, err := Parse([]byte(doc), "test.yaml")
			if err == nil {
				t.Fatalf("Parse accepted moderation.%s", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("error does not name %s: %v", field, err)
			}
		})
	}
}

// A set that cannot identify exactly one holder is refused at load, so an
// ambiguous audit trail never reaches a request.
func TestAdminUserValidation(t *testing.T) {
	cases := []struct {
		name       string
		moderation string
	}{
		{"a missing name", "  admin_users:\n    - password_hash: \"" + testPasswordHash + "\"\n"},
		{"an empty name", "  admin_users:\n    - name: \"  \"\n      password_hash: \"" + testPasswordHash + "\"\n"},
		{"a name that is not a handle", "  admin_users:\n    - name: \"alice smith\"\n      password_hash: \"" + testPasswordHash + "\"\n"},
		{"a name over the length", "  admin_users:\n    - name: \"" + strings.Repeat("a", 65) + "\"\n      password_hash: \"" + testPasswordHash + "\"\n"},
		{"a missing password hash", "  admin_users:\n    - name: \"alice\"\n"},
		{"a password instead of a hash", "  admin_users:\n    - name: \"alice\"\n      password: \"plaintext\"\n"},
		{"a hash that is not bcrypt", "  admin_users:\n    - name: \"alice\"\n      password_hash: \"not-a-hash\"\n"},
		{"a duplicate name", "  admin_users:\n    - name: \"alice\"\n      password_hash: \"" + testPasswordHash + "\"\n    - name: \"alice\"\n      password_hash: \"" + testPasswordHash + "\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(twoChatOneEmbedding+"\nmoderation:\n"+tc.moderation), "test.yaml"); err == nil {
				t.Fatal("Parse accepted an invalid user set")
			}
		})
	}

	// One more than the cap is refused, and the cap itself is accepted.
	var atCap strings.Builder
	for i := 0; i < maxAdminUsers; i++ {
		fmt.Fprintf(&atCap, "    - name: \"holder-%d\"\n      password_hash: %q\n", i, testPasswordHash)
	}
	if _, err := Parse([]byte(twoChatOneEmbedding+"\nmoderation:\n  admin_users:\n"+atCap.String()), "test.yaml"); err != nil {
		t.Errorf("the cap of %d users was refused: %v", maxAdminUsers, err)
	}
	tooMany := atCap.String() + "    - name: \"one-too-many\"\n      password_hash: \"" + testPasswordHash + "\"\n"
	if _, err := Parse([]byte(twoChatOneEmbedding+"\nmoderation:\n  admin_users:\n"+tooMany), "test.yaml"); err == nil {
		t.Errorf("Parse accepted %d users, want at most %d", maxAdminUsers+1, maxAdminUsers)
	}
}

// Only the brace form is an environment placeholder, so a bcrypt hash and a
// bare dollar sign reach the parser as written.
func TestOnlyBraceEnvironmentPlaceholdersExpand(t *testing.T) {
	t.Setenv("RUMIL_TEST_ADMIN_HASH", testPasswordHash)
	cfg := mustParse(t, twoChatOneEmbedding+`
moderation:
  admin_users:
    - name: "alice"
      password_hash: "${RUMIL_TEST_ADMIN_HASH}"
`)
	if got := cfg.Moderation.Users()[0].PasswordHash; got != testPasswordHash {
		t.Errorf("password_hash = %q, want the environment value", got)
	}
}
