package server

import "strings"

// Command is one instruction parsed out of a comment that mentions Rumil.
//
// The pull request conversation supports two commands. `remember <instruction>`
// records a rule (project_plan.md §6 Phase 4), in the two scopes of §6 Phase 6:
// a `--org` marker before the instruction or the word `org` after it records a
// rule for the whole organization rather than for the repository the comment is
// on. `review` asks for a review of the pull request the comment is on, which
// is the trigger that works where the App cannot be requested as a reviewer:
// the REST API refuses a GitHub App in its reviewers list, and the UI does not
// offer one everywhere. Parsing happens in the webhook so a comment that asks
// for nothing never takes a worker.
type Command struct {
	// Verb is the command word, lowercased: "remember" or "review".
	Verb string
	// Instruction is what the person wrote after the verb, with the scope marker
	// taken out. It is the raw material of the synthesis step, not a rule yet,
	// and it is empty for a command that carries none.
	Instruction string
	// Org is true when the command asked for an organization-wide rule, which is
	// the only thing that lets the synthesis session record one.
	Org bool
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

// splitVerb reads the verb after a mention, the scope it asks for, and the
// instruction after that.
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
	command := Command{Verb: strings.ToLower(verb)}
	command.Org, command.Instruction = splitScope(instruction)
	return command
}

// splitScope reads the scope marker at the head of an instruction: `--org`, or
// the shorter `org` the plan names as its equivalent (§6 Phase 6 writes the
// shorthand as `[org]`, its notation for an optional token, so the token itself
// is `org`). Both are separated from the instruction by whitespace, and `--org`
// is the unambiguous form: an instruction that happens to open with the word
// "org" is read as a scope marker, which is why the longer form exists.
func splitScope(instruction string) (org bool, rest string) {
	token, after := instruction, ""
	if i := strings.IndexAny(instruction, " \t\r\n"); i >= 0 {
		token, after = instruction[:i], strings.TrimSpace(instruction[i+1:])
	}

	switch strings.ToLower(token) {
	case "--org", "org":
		// A marker with nothing behind it is not a scope, it is the whole
		// instruction: "@rumil remember org" asks to remember the word "org",
		// and answering that with an empty rule would be worse than useless.
		if after == "" {
			return false, instruction
		}
		return true, after
	default:
		return false, instruction
	}
}

// isHandleByte reports whether a byte continues a GitHub login, so a match
// followed by one is a longer handle rather than this one.
func isHandleByte(c byte) bool {
	return c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
