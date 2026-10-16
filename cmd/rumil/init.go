package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/joangrigorov/rumil/internal/config"
)

// secretBytes is the length of the webhook secret `rumil init` offers. The
// secret is a shared string between GitHub and Rumil, so it only has to be
// long and random; GitHub's own suggestion is 32 hex characters.
const secretBytes = 16

// minSecretLength is what a hand-typed webhook secret has to reach. A short
// secret is a weak HMAC key, and the generated one sets the example to follow.
const minSecretLength = 16

func newInitCmd(configPath *string) *cobra.Command {
	var (
		force     bool
		printOnly bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a configuration file, one question at a time",
		Long: "Ask for the GitHub App credentials, the model endpoint and the optional pieces, then write a\n" +
			"configuration file the runtime accepts. Every screen says what it is asking for, so the README is\n" +
			"not needed while it runs, and nothing is written until the whole file has been parsed.\n\n" +
			"The file is written where Rumil looks for one when --config is not given: ~/.config/rumil/config.yaml,\n" +
			"or ./rumil.yaml when there is no home directory. An answer that is the name of an environment\n" +
			"variable is written as ${THAT_VARIABLE}, so the file can be copied around without the secrets.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, *configPath, force, printOnly)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing configuration file")
	cmd.Flags().BoolVar(&printOnly, "print", false, "write the configuration to stdout instead of a file")

	return cmd
}

func runInit(cmd *cobra.Command, configFlag string, force, printOnly bool) error {
	out := cmd.OutOrStdout()

	target, err := config.Env(configFlag).WriteTarget()
	if err != nil {
		return err
	}
	// Before the first question, so that a long conversation does not end in a
	// refusal.
	if !printOnly && !force {
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite it, or --print to see what would be written", target)
		}
	}

	given := answers{
		Answers: config.Answers{
			PrivateKeyPath: "GITHUB_APP_PRIVATE_KEY_PATH",
			WebhookSecret:  generatedSecret(),
			ChatBaseURL:    "https://api.openai.com/v1",
			ChatModel:      "gpt-4o-mini",
			ChatAPIKey:     "OPENAI_API_KEY",
			DatabaseDSN:    "DATABASE_URL",
		},
		confirm: true,
	}

	in := cmd.InOrStdin()
	interactive := isTerminal(in)

	form := initForm(&given, target, interactive, printOnly).
		WithOutput(cmd.ErrOrStderr()).
		WithInput(in)
	if !interactive {
		// Without a terminal huh asks its questions a line at a time, which is
		// also how the answers in a file reach it.
		form = form.WithInput(oneLineAtATime(in)).WithAccessible(true)
	}
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return errors.New("stopped; nothing was written")
		}
		return err
	}
	if !printOnly && !given.confirm {
		_, _ = fmt.Fprintln(out, "nothing was written")
		return nil
	}

	// The file is rendered and parsed before anything reaches the disk, so a
	// file that would stop Rumil at startup is never written at all.
	raw, err := config.Render(given.Answers)
	if err != nil {
		return err
	}
	if printOnly {
		_, err := out.Write(raw)
		return err
	}

	if dir := filepath.Dir(target); dir != "" && dir != "." {
		// 0700: the file can hold a webhook secret and an admin token.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}

	_, _ = fmt.Fprintf(out, "wrote %s\n\n", target)
	_, _ = fmt.Fprintf(out, "next: export the environment variables it names, then\n")
	_, _ = fmt.Fprintf(out, "  rumil review --repo owner/name --pr 1\n")
	if given.DatabaseDSN != "" {
		_, _ = fmt.Fprintf(out, "with the database configured, the server and its queue are\n  rumil serve\n")
	}
	return nil
}

// answers carries the configuration file's answers along with the one decision
// that is not part of the file.
type answers struct {
	config.Answers
	confirm bool
}

// initForm builds the whole questionnaire.
func initForm(a *answers, target string, interactive, printOnly bool) *huh.Form {
	groups := []*huh.Group{
		huh.NewGroup(
			huh.NewNote().
				Title("The GitHub App").
				Description("Rumil ships no App of its own: it authenticates as yours, so nothing here is shared\n"+
					"with anyone else. Create one at https://github.com/settings/apps/new — or in your\n"+
					"organization's settings — with these repository permissions:\n\n"+
					"  Contents        read            to read RUMIL.md and the files behind a diff\n"+
					"  Issues          read and write  to read the conversation and answer a remembered rule\n"+
					"  Pull requests   read and write  to read the pull request and post the review\n"+
					"  Metadata        read            always required by GitHub\n\n"+
					"When it exists, generate a private key, download the .pem, note the App ID, and install the\n"+
					"App on the repositories Rumil should review."),
			huh.NewInput().
				Title("The App's slug").
				Description("How people address Rumil in comments: an App named \"Acme Rumil\" answers to acme-rumil.").
				Placeholder("acme-rumil").
				Value(&a.BotHandle).
				Validate(required("the App's slug")),
			huh.NewInput().
				Title("App ID").
				Description("On the App's settings page, under \"About\".").
				Placeholder("123456").
				Value(&a.AppID).
				Validate(positiveNumber("the App ID")),
			huh.NewInput().
				Title("Installation ID").
				Description("The number at the end of the URL after installing the App, github.com/settings/installations/<id>.").
				Placeholder("98765432").
				Value(&a.InstallationID).
				Validate(positiveNumber("the installation ID")),
			huh.NewInput().
				Title("Private key").
				Description("Path to the .pem, or the name of an environment variable that holds the path.").
				Placeholder("GITHUB_APP_PRIVATE_KEY_PATH").
				Value(&a.PrivateKeyPath).
				Validate(required("the App's private key")),
		).Title("GitHub App"),

		huh.NewGroup(
			huh.NewNote().
				Title("The model").
				Description("Any OpenAI-compatible chat endpoint works, and it has to support tool calling: a review\n"+
					"is an agent session that calls tools, and an endpoint that answers without them returns no\n"+
					"review. The key is found in your provider's dashboard, and it is billed by that provider."),
			huh.NewInput().
				Title("Chat endpoint").
				Description("The base URL, usually ending in /v1.").
				Placeholder("https://api.openai.com/v1").
				Value(&a.ChatBaseURL).
				Validate(startURL("the chat endpoint")),
			huh.NewInput().
				Title("Chat model").
				Description("The model that reviews pull requests. A larger one reviews better and costs more.").
				Placeholder("gpt-4o-mini").
				Value(&a.ChatModel).
				Validate(required("the chat model")),
			secretInput(interactive).
				Title("Chat API key").
				Description("The key itself, or the name of an environment variable that holds it.").
				Placeholder("OPENAI_API_KEY").
				Value(&a.ChatAPIKey).
				Validate(required("the chat API key")),
		).Title("Model"),

		huh.NewGroup(
			huh.NewNote().
				Title("Memory (optional)").
				Description("With an embedding provider configured, Rumil records the engineering rules your team\n"+
					"states in review and applies the ones a maintainer approved, so a decision made once is not\n"+
					"re-litigated on the next pull request. It also needs a database, which the next screen asks\n"+
					"about.\n\n"+
					"Leave the embedding model empty if you do not want this yet: reviews work without it.\n\n"+
					"The model is an embeddings model of its own, and it may sit at another provider than the\n"+
					"chat model: the endpoint below is empty to reuse the chat one. It has to return\n"+
					"1536-dimension embeddings, which is what text-embedding-3-small does."),
			huh.NewInput().
				Title("Embedding model").
				Description("An embeddings model, not the chat model above; empty turns memory off.").
				Placeholder("text-embedding-3-small").
				Value(&a.EmbeddingModel).
				Validate(differentFromChat(&a.ChatModel)),
			huh.NewInput().
				Title("Embedding endpoint").
				Description("Empty reuses the chat endpoint.").
				Placeholder("https://api.openai.com/v1").
				Value(&a.EmbeddingBaseURL).
				Validate(startURLEmpty("the embedding endpoint")),
			secretInput(interactive).
				Title("Embedding API key").
				Description("Empty reuses the chat key.").
				Placeholder("OPENAI_API_KEY").
				Value(&a.EmbeddingAPIKey),
		).Title("Memory"),

		huh.NewGroup(
			huh.NewNote().
				Title("The webhook server (optional)").
				Description("Only `rumil serve` needs these. It listens for GitHub's webhooks and reviews a pull\n"+
					"request when someone requests the App as a reviewer; `rumil review` above does not.\n\n"+
					"To run it, put this webhook secret in the App's settings as the Webhook secret, set the\n"+
					"Webhook URL to https://<your-rumil-host>/webhooks/github, and subscribe to the events\n"+
					"Pull request, Issue comment and Pull request review comment.\n\n"+
					"Leave the database empty to configure only the command line for now; the server and the\n"+
					"recorded rules need Postgres 16 with the pgvector extension."),
			huh.NewInput().
				Title("Webhook secret").
				Description("Generated for you. Paste it into the App, or clear it to leave the server out.").
				Value(&a.WebhookSecret).
				Validate(minLength("the webhook secret", minSecretLength)),
			huh.NewInput().
				Title("Database URL").
				Description("postgres://user:pass@host:5432/rumil?sslmode=disable, or the name of an environment variable holding it.").
				Placeholder("DATABASE_URL").
				Value(&a.DatabaseDSN).
				Validate(databaseURL),
			secretInput(interactive).
				Title("Moderation token").
				Description("Optional, and needs the database above: the password for the moderation page and API.").
				Value(&a.AdminToken),
		).Title("Webhook server"),
	}

	if !printOnly {
		groups = append(groups, huh.NewGroup(
			huh.NewNote().
				Title("Write it").
				Description(fmt.Sprintf("The file goes to %s, where Rumil looks for one when --config is not given.\n"+
					"Nothing has been checked against GitHub: the App ID and the key are only read at runtime.", target)),
			huh.NewConfirm().
				Title("Write "+target+"?").
				Affirmative("Write it").
				Negative("Cancel").
				Value(&a.confirm),
		).Title("Write"))
	}

	return huh.NewForm(groups...)
}

// isTerminal reports whether the reader is a terminal. huh draws a full-screen
// form on one, and asks line by line without one.
func isTerminal(in io.Reader) bool {
	f, ok := in.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(f.Fd())
}

// secretInput returns an input that hides the keystrokes when there is a
// terminal to hide them from: huh's line-by-line prompts cannot mask input
// without one, and a prompt that refuses to run is worse than a visible answer.
func secretInput(interactive bool) *huh.Input {
	in := huh.NewInput()
	if interactive {
		return in.EchoMode(huh.EchoModePassword)
	}
	return in
}

// lineReader returns one line per read. Each of huh's prompts reads through a
// scanner of its own, and a scanner takes whatever the reader offers: without
// this, a file of answers piped into `rumil init` would be swallowed by the
// first question and every later one would be answered with the default.
type lineReader struct {
	r *bufio.Reader
}

func oneLineAtATime(r io.Reader) io.Reader { return &lineReader{r: bufio.NewReader(r)} }

func (l *lineReader) Read(p []byte) (int, error) {
	line, err := l.r.ReadBytes('\n')
	switch {
	case len(line) > 0 && len(line) <= len(p):
		copy(p, line)
		return len(line), nil
	case len(line) > len(p):
		return 0, fmt.Errorf("a line of input is longer than the %d bytes a prompt offered", len(p))
	default:
		return 0, err
	}
}

// generatedSecret returns the webhook secret offered by default.
func generatedSecret() string {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		// The generator is the kernel's; a failure here means the answer is
		// asked for by hand instead.
		return ""
	}
	return hex.EncodeToString(buf)
}

func required(what string) func(string) error {
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", what)
		}
		return nil
	}
}

// differentFromChat refuses an embedding model that repeats the chat model. A
// provider is either the chat model or the embedding model, and its id is the
// name of its model, so the chat model here asks for a file the runtime
// refuses. It reads the chat answer when the question is answered rather than
// when the form is built, and config.Render refuses the same pair, which covers
// an answer file and an answer that was changed after this screen was left.
func differentFromChat(chat *string) func(string) error {
	return func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" || value != strings.TrimSpace(*chat) {
			return nil
		}
		return fmt.Errorf("the chat model is already %q: the embedding model is a model of its own, or empty to run without memory", value)
	}
}

func positiveNumber(what string) func(string) error {
	return func(value string) error {
		number, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || number <= 0 {
			return fmt.Errorf("%s is a positive number", what)
		}
		return nil
	}
}

// startURL accepts an http or https URL.
func startURL(what string) func(string) error {
	return func(value string) error {
		parsed, err := url.Parse(strings.TrimSpace(value))
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("%s is a URL like https://api.openai.com/v1", what)
		}
		return nil
	}
}

// startURLEmpty accepts an http or https URL, or nothing at all.
func startURLEmpty(what string) func(string) error {
	start := startURL(what)
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return start(value)
	}
}

func minLength(what string, length int) func(string) error {
	return func(value string) error {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil
		}
		if len(trimmed) < length {
			return fmt.Errorf("%s is at least %d characters, or empty to leave the server out", what, length)
		}
		return nil
	}
}

// databaseURL accepts a Postgres URL, the name of an environment variable that
// holds one, or nothing at all.
func databaseURL(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || config.IsEnvName(trimmed) {
		return nil
	}
	if !strings.HasPrefix(trimmed, "postgres://") && !strings.HasPrefix(trimmed, "postgresql://") {
		return errors.New("the database URL starts with postgres://, or name the environment variable that holds it")
	}
	return nil
}
