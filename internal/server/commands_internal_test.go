package server

import "testing"

func TestParseCommand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		wantOK      bool
		wantVerb    string
		wantInstrct string
	}{
		{"the command alone", "@rumil remember we always retry", true, "remember", "we always retry"},
		{"the verb alone", "@rumil remember", true, "remember", ""},
		{"a mention with nothing after it", "@rumil", true, "", ""},
		{"the verb in another case", "@rumil Remember we retry", true, "remember", "we retry"},
		{"the mention mid-sentence", "Good catch. @rumil remember we retry here", true, "remember", "we retry here"},
		{"the login form of the handle", "@rumil[bot] remember we retry", true, "remember", "we retry"},
		{"the handle in another case", "@Rumil remember we retry", true, "remember", "we retry"},
		{"a mention of another account", "@rumil2 remember we retry", false, "", ""},
		{"no mention", "looks good to me", false, "", ""},
		{"another command", "@rumil context: the payments flow", true, "context:", "the payments flow"},
		{"a mention inside a word", "the rumil@rumil and its use", true, "and", "its use"},
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
