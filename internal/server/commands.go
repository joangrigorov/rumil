package server

import (
	"strings"
	"unicode/utf8"
)

// Command is one instruction parsed out of a comment that mentions Rumil.
//
// The pull request conversation supports four commands. `remember <instruction>`
// records a rule (project_plan.md §6 Phase 4), in the two scopes of §6 Phase 6:
// a `--org` marker before the instruction or the word `org` after it records a
// rule for the whole organization rather than for the repository the comment is
// on. `review` asks for a review of the pull request the comment is on, which
// is the trigger that works where the App cannot be requested as a reviewer:
// the REST API refuses a GitHub App in its reviewers list, and the UI does not
// offer one everywhere. `why` and `explain` ask Rumil to say more about a
// finding inside a review thread. Parsing happens in the webhook so a comment
// that asks for nothing never takes a worker.
type Command struct {
	// Verb is the command word, lowercased: "remember", "review", "why" or
	// "explain".
	Verb string
	// Instruction is what the person wrote after the verb, with the scope marker
	// taken out. It is the raw material of the synthesis step, not a rule yet,
	// and it is empty for a command that carries none. For `review` it is the
	// focus guidance, and for `why` and `explain` it is the question itself;
	// both are capped so a pasted log cannot fill every task message.
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
	command := Command{Verb: commandVerb(verb)}
	switch command.Verb {
	case commandRemember:
		command.Org, command.Instruction = splitScope(instruction)
	case commandReview, commandWhy, commandExplain:
		command.Instruction = focusOf(instruction)
	default:
		command.Instruction = instruction
	}
	return command
}

// commandVerb reads the verb out of the first token. A person writes a command
// as a sentence — "review. Focus on the migration" — so a closing period or
// colon belongs to the prose rather than the verb.
func commandVerb(token string) string {
	return strings.ToLower(strings.TrimRight(token, ".:"))
}

// maxFocusBytes bounds the guidance a review command carries. A comment body
// can be tens of kilobytes, and the guidance goes into the task message of
// every turn, so a pasted log would crowd out the work.
const maxFocusBytes = 2000

// focusOf trims the guidance and caps it at a rune boundary.
func focusOf(instruction string) string {
	instruction = strings.TrimSpace(instruction)
	if len(instruction) <= maxFocusBytes {
		return instruction
	}
	cut := maxFocusBytes
	for cut > 0 && !utf8.RuneStart(instruction[cut]) {
		cut--
	}
	return strings.TrimSpace(instruction[:cut])
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

// isAnswerVerb reports whether the command asks Rumil to explain a finding.
func isAnswerVerb(verb string) bool {
	return verb == commandWhy || verb == commandExplain
}

// isHandleByte reports whether a byte continues a GitHub login, so a match
// followed by one is a longer handle rather than this one.
func isHandleByte(c byte) bool {
	return c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
