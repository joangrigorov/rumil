package server

import "strings"

// Command is one instruction parsed out of a comment that mentions Rumil.
//
// The pull request conversation supports exactly one command, `@rumil remember
// <instruction>` (project_plan.md §6 Phase 4). Parsing happens in the webhook so
// a comment that asks for nothing never takes a worker.
type Command struct {
	// Verb is the command word, lowercased: "remember".
	Verb string
	// Instruction is what the person wrote after the verb. It is the raw
	// material of the synthesis step, not a rule yet.
	Instruction string
}

// parseCommand reads the first mention of the bot in a comment body and the one
// word that follows it. It reports false when the body mentions Rumil without a
// command, and an empty verb when the mention carries nothing after it.
func parseCommand(botLogin, body string) (Command, bool) {
	if botLogin == "" {
		return Command{}, false
	}

	mention := "@" + botLogin
	for i := 0; i+len(mention) <= len(body); i++ {
		if !strings.EqualFold(body[i:i+len(mention)], mention) {
			continue
		}
		end := i + len(mention)
		// A mention is a whole handle: "@rumil2" mentions another account.
		if end == len(body) || !isHandleByte(body[end]) {
			return splitVerb(body[end:]), true
		}
	}
	return Command{}, false
}

// splitVerb reads the verb after a mention and the instruction after that.
func splitVerb(rest string) Command {
	rest = strings.TrimLeft(rest, " \t\r\n")
	// The timeline shows an App as "rumil[bot]", so a handle copied out of it
	// carries the suffix even though the mention people type does not.
	if len(rest) >= 5 && strings.EqualFold(rest[:5], "[bot]") {
		rest = strings.TrimLeft(rest[5:], " \t\r\n")
	}

	verb, instruction := rest, ""
	if i := strings.IndexAny(rest, " \t\r\n"); i >= 0 {
		verb, instruction = rest[:i], strings.TrimSpace(rest[i+1:])
	}
	return Command{Verb: strings.ToLower(verb), Instruction: instruction}
}

// isHandleByte reports whether a byte continues a GitHub login, so a match
// followed by one is a longer handle rather than this one.
func isHandleByte(c byte) bool {
	return c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
