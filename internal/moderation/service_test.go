package moderation

import (
	"errors"
	"testing"
)

// The status filter is normalized in one place, which is where both the CLI and
// the HTTP surface pass through, so neither can hand the statement a value the
// enum will refuse.
func TestNormalizeStatusAcceptsTheThreeAndNothingElse(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   string
	}{
		{"nothing asked for", "", ""},
		{"as the enum spells it", "PENDING", "PENDING"},
		{"as a moderator types it", "approved", "APPROVED"},
		{"with padding", "  Rejected  ", "REJECTED"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeStatus(tc.status)
			if err != nil {
				t.Fatalf("normalizeStatus(%q) returned error: %v", tc.status, err)
			}
			if got != tc.want {
				t.Errorf("normalizeStatus(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}

	for _, status := range []string{"approve", "pending2", "any", "PENDING,APPROVED"} {
		t.Run("refused: "+status, func(t *testing.T) {
			got, err := normalizeStatus(status)
			if !errors.Is(err, ErrUnknownStatus) {
				t.Fatalf("normalizeStatus(%q) returned %v, want %v", status, err, ErrUnknownStatus)
			}
			if got != "" {
				t.Errorf("a refused status came back as %q", got)
			}
		})
	}
}

// The authority a decision carries is normalized in the same one place the
// status and the scope are, so a surface cannot hand the statement a value the
// enum will refuse.
func TestNormalizeAuthorityAcceptsTheTwoAndNothingElse(t *testing.T) {
	cases := []struct {
		name      string
		authority string
		want      string
	}{
		{"nothing asked for", "", ""},
		{"as the enum spells it", "CONTEXT", "CONTEXT"},
		{"as a moderator types it", "directive", "DIRECTIVE"},
		{"with padding", "  Context  ", "CONTEXT"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeAuthority(tc.authority)
			if err != nil {
				t.Fatalf("normalizeAuthority(%q) returned error: %v", tc.authority, err)
			}
			if got != tc.want {
				t.Errorf("normalizeAuthority(%q) = %q, want %q", tc.authority, got, tc.want)
			}
		})
	}

	for _, authority := range []string{"absolute", "directives", "CONTEXT,DIRECTIVE", "repo"} {
		t.Run("refused: "+authority, func(t *testing.T) {
			got, err := normalizeAuthority(authority)
			if !errors.Is(err, ErrUnknownAuthority) {
				t.Fatalf("normalizeAuthority(%q) returned %v, want %v", authority, err, ErrUnknownAuthority)
			}
			if got != "" {
				t.Errorf("a refused authority came back as %q", got)
			}
		})
	}
}
