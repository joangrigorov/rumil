package server

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseCommand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		wantOK      bool
		wantVerb    string
		wantInstrct string
		wantOrg     bool
	}{
		{"the command alone", "@rumil remember we always retry", true, "remember", "we always retry", false},
		{"the verb alone", "@rumil remember", true, "remember", "", false},
		{"a mention with nothing after it", "@rumil", true, "", "", false},
		{"the verb in another case", "@rumil Remember we retry", true, "remember", "we retry", false},
		{"the mention mid-sentence", "Good catch. @rumil remember we retry here", true, "remember", "we retry here", false},
		{"the login form of the handle", "@rumil[bot] remember we retry", true, "remember", "we retry", false},
		{"the handle in another case", "@Rumil remember we retry", true, "remember", "we retry", false},
		{"a mention of another account", "@rumil2 remember we retry", false, "", "", false},
		{"no mention", "looks good to me", false, "", "", false},
		{"another command", "@rumil context: the payments flow", true, "context", "the payments flow", false},
		{"a mention inside a word", "the rumil@rumil and its use", true, "and", "its use", false},

		// A review can be asked for as a sentence, and the words after the
		// verb are the focus the reviewer spends its effort on.
		{"review with a closing period", "@rumil review. Focus on migration consistency", true, "review", "Focus on migration consistency", false},
		{"review with a colon", "@rumil review: check the retry path", true, "review", "check the retry path", false},
		{"review with a closing period alone", "@rumil review.", true, "review", "", false},
		{"review alone", "@rumil review", true, "review", "", false},
		{"a focus that opens with org", "@rumil review. org consistency matters here", true, "review", "org consistency matters here", false},

		// A question in a review thread is the same shape: the words after the
		// verb are the question, and the verb is readable as a sentence.
		{"why with a question", "@rumil why does this wrap the call", true, "why", "does this wrap the call", false},
		{"why with a closing period", "@rumil why. The retry loop is not visible here", true, "why", "The retry loop is not visible here", false},
		{"why alone", "@rumil why", true, "why", "", false},
		{"explain in another case", "@rumil EXPLAIN this with the caller in mind", true, "explain", "this with the caller in mind", false},
		{"explain with a colon", "@rumil explain: what makes this safe", true, "explain", "what makes this safe", false},

		// T-040-01: the two ways a comment asks for an organization-wide rule.
		{"the long scope marker", "@rumil remember --org all services log with slog", true, "remember", "all services log with slog", true},
		{"the short scope marker", "@rumil remember org all services log with slog", true, "remember", "all services log with slog", true},
		{"the long marker in another case", "@rumil remember --ORG all services log", true, "remember", "all services log", true},
		{"the short marker in another case", "@rumil remember Org all services log", true, "remember", "all services log", true},
		{"the short marker after the login form", "@rumil[bot] remember org all services log", true, "remember", "all services log", true},
		// The marker is the first token or it is part of the instruction, so a
		// note that happens to mention the organization is not read as one.
		{"the word org later in the note", "@rumil remember we talk to the org API here", true, "remember", "we talk to the org API here", false},
		{"a word that starts with org", "@rumil remember organise the retries", true, "remember", "organise the retries", false},
		// The cost of the short marker, named rather than left to be
		// discovered: a note that opens with the word is read as the marker.
		// That is why `--org` exists.
		{"a note that opens with the word", "@rumil remember org is the billing service here", true, "remember", "is the billing service here", true},
		// A marker with nothing behind it is the whole instruction: answering
		// it with an empty rule would record nothing at all.
		{"the short marker alone", "@rumil remember org", true, "remember", "org", false},
		{"the long marker alone", "@rumil remember --org", true, "remember", "--org", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, ok := parseCommand("rumil", tc.body)
			if ok != tc.wantOK {
				t.Fatalf("parseCommand(%q) ok = %v, want %v", tc.body, ok, tc.wantOK)
			}
			if command.Verb != tc.wantVerb || command.Instruction != tc.wantInstrct {
				t.Errorf("parseCommand(%q) = %+v, want verb %q instruction %q",
					tc.body, command, tc.wantVerb, tc.wantInstrct)
			}
			if command.Org != tc.wantOrg {
				t.Errorf("parseCommand(%q) Org = %v, want %v", tc.body, command.Org, tc.wantOrg)
			}
		})
	}
}

func TestParseCommandWithoutABotHandle(t *testing.T) {
	if _, ok := parseCommand("", "@rumil remember we retry"); ok {
		t.Error("a webhook without a configured handle parsed a command")
	}
}

func TestParseCommandKeepsAMultilineInstruction(t *testing.T) {
	body := "@rumil remember first line\nsecond line"
	command, ok := parseCommand("rumil", body)
	if !ok {
		t.Fatalf("parseCommand(%q) reported no command", body)
	}
	if command.Instruction != "first line\nsecond line" {
		t.Errorf("instruction = %q, want the whole note", command.Instruction)
	}
}

// The guidance is pasted into every turn's task message, so a long comment is
// capped at a rune boundary rather than cut through a character.
func TestFocusIsCappedAtARuneBoundary(t *testing.T) {
	long := strings.Repeat("a", maxFocusBytes-1) + "é" + strings.Repeat("b", 500)
	command, ok := parseCommand("rumil", "@rumil review. "+long)
	if !ok {
		t.Fatal("the review command did not parse")
	}
	if len(command.Instruction) > maxFocusBytes {
		t.Errorf("focus is %d bytes, want at most %d", len(command.Instruction), maxFocusBytes)
	}
	if !utf8.ValidString(command.Instruction) {
		t.Error("the cap cut through a character")
	}
	if strings.HasSuffix(command.Instruction, "b") {
		t.Error("the cap kept the tail of a long instruction")
	}
}
