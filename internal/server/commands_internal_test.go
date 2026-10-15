package server

import "testing"

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
		{"another command", "@rumil context: the payments flow", true, "context:", "the payments flow", false},
		{"a mention inside a word", "the rumil@rumil and its use", true, "and", "its use", false},

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
