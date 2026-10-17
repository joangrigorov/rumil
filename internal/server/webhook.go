// Package server hosts the HTTP surface of `rumil serve`: the GitHub webhook
// endpoint, the health check, and the worker pool that drains the job queue.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/joangrigorov/rumil/internal/ghapp"
	"github.com/joangrigorov/rumil/internal/server/web"
	"github.com/joangrigorov/rumil/internal/store"
)

// Event types Rumil handles.
const (
	eventPullRequest  = "pull_request"
	eventIssueComment = "issue_comment"
	// eventReviewComment is a reply inside a review thread. Its payload carries
	// the diff hunk the comment hangs on, which the issue comment payload does
	// not have.
	eventReviewComment = "pull_request_review_comment"
)

// The two commands the conversation supports.
const (
	// commandRemember records a rule from the comment.
	commandRemember = "remember"
	// commandReview asks for a review of the pull request the comment is on.
	// It exists because a GitHub App cannot be requested as a reviewer from
	// every surface: the API refuses one, and the people who hit that can ask
	// in the conversation instead.
	commandReview = "review"
)

// signatureHeader carries the HMAC of the raw webhook body.
const signatureHeader = "X-Hub-Signature-256"

// maxWebhookBytes bounds the body read, per §8.
const maxWebhookBytes = 10 << 20

// healthTimeout bounds the database check behind /healthz.
const healthTimeout = 2 * time.Second

// Webhook verifies and dispatches GitHub deliveries.
type Webhook struct {
	secret         []byte
	jobs           *store.DB
	botLogin       string
	installationID int64
	logger         *slog.Logger
}

// NewWebhook takes the configured webhook secret and bot handle, so it can
// tell a review request aimed at Rumil from one aimed at a person, and the
// installation Rumil serves. An App's webhook secret is account-wide, so a
// delivery signed with it can belong to any installation of the App; only the
// configured one has an installation token the engine can use.
func NewWebhook(secret, botHandle string, installationID int64, jobs *store.DB, logger *slog.Logger) *Webhook {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Webhook{
		secret:         []byte(secret),
		jobs:           jobs,
		botLogin:       ghapp.NormalizeLogin(botHandle),
		installationID: installationID,
		logger:         logger,
	}
}

// Handler builds the routes of `rumil serve`. Method patterns make the mux
// answer 405 for anything else. moderation and the observation deck may be
// nil, and their routes are then not mounted at all.
func Handler(webhook *Webhook, db *store.DB, moderation *ModerationUI, sessions ...*SessionsUI) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/github", webhook)
	mux.HandleFunc("GET /healthz", health(db, webhook.logger))
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))
	if moderation != nil {
		moderation.Routes(mux)
	}
	for _, deck := range sessions {
		if deck != nil {
			deck.Routes(mux)
		}
	}
	return mux
}

// staticHandler serves the embedded stylesheet and script tree. The assets are
// part of the binary, so a package install has nothing to copy and a missing
// file is a build error rather than a 404 at runtime.
func staticHandler() http.Handler {
	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		panic(fmt.Sprintf("the embedded static tree: %v", err))
	}
	files := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
}

// health reports whether the database answers. A failed ping is logged rather
// than returned: the response goes to whoever can reach the port, and the
// driver's error names the host and the database.
func health(db *store.DB, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			logger.Error("the database health check failed", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// ServeHTTP implements POST /webhooks/github.
func (w *Webhook) ServeHTTP(writer http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(writer, r.Body, maxWebhookBytes))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"status": "unreadable body"})
		return
	}

	if !w.validSignature(r.Header.Get(signatureHeader), body) {
		// Nothing is written to the database on an unauthenticated delivery.
		w.logger.Warn("rejected a webhook with a missing or invalid signature",
			"delivery_id", r.Header.Get("X-GitHub-Delivery"))
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"status": "invalid signature"})
		return
	}

	var delivery payload
	if err := json.Unmarshal(body, &delivery); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"status": "malformed payload"})
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if deliveryID == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"status": "missing X-GitHub-Delivery"})
		return
	}

	job, ignore := w.classify(eventType, delivery)
	if ignore != "" {
		w.logger.Info("ignored a delivery", "delivery_id", deliveryID, "event_type", eventType, "reason", ignore)
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ignored", "reason": ignore})
		return
	}
	job.DeliveryID = deliveryID
	job.Payload = body

	switch err := w.jobs.Enqueue(r.Context(), job); {
	case err == nil:
		w.logger.Info("queued a review",
			"delivery_id", deliveryID, "event_type", eventType,
			"installation_id", job.InstallationID,
			"repo", job.OrgName+"/"+job.RepoName, "pr", job.PRNumber,
		)
		writeJSON(writer, http.StatusAccepted, map[string]string{"status": "queued"})
	case errors.Is(err, store.ErrDuplicateDelivery):
		// GitHub retried a delivery Rumil already has: absorb it.
		w.logger.Info("ignored a duplicate delivery", "delivery_id", deliveryID)
		writeJSON(writer, http.StatusOK, map[string]string{"status": "duplicate ignored"})
	default:
		w.logger.Error("queueing a delivery failed", "delivery_id", deliveryID, "error", err)
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"status": "enqueue failed"})
	}
}

// validSignature compares the HMAC of the body with the header GitHub sent.
func (w *Webhook) validSignature(header string, body []byte) bool {
	if len(w.secret) == 0 || header == "" {
		return false
	}
	encoded, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	provided, err := hex.DecodeString(encoded)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, w.secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), provided)
}

// classify decides whether a delivery becomes a job. It returns the job to
// enqueue, or a reason to ignore the delivery.
//
// Reviewing is never automatic: the only pull request action that counts is a
// review request aimed at Rumil itself.
func (w *Webhook) classify(eventType string, d payload) (store.Job, string) {
	if w.installationID > 0 && d.Installation.ID != w.installationID {
		// Dropped before anything is written: a job for an installation Rumil
		// cannot authenticate as would fail after the fact, and a remember
		// command would leave a row behind while it did.
		return store.Job{}, fmt.Sprintf("the delivery is for installation %d, and Rumil serves %d", d.Installation.ID, w.installationID)
	}

	switch eventType {
	case eventPullRequest:
		if d.Action != "review_requested" {
			return store.Job{}, fmt.Sprintf("pull_request action %q is not a review request", d.Action)
		}
		if !isBotReviewer(w.botLogin, d.RequestedReviewer) {
			return store.Job{}, "the review was requested from someone else"
		}
		if d.PullRequest == nil {
			return store.Job{}, "the payload carries no pull request"
		}
		return w.job(eventType, d, d.PullRequest.Number), ""

	case eventIssueComment, eventReviewComment:
		if d.Action != "created" {
			return store.Job{}, fmt.Sprintf("%s action %q is not a new comment", eventType, d.Action)
		}
		if d.Comment == nil {
			return store.Job{}, "the payload carries no comment"
		}
		if d.Comment.User.Type == "Bot" {
			// Rumil's own comments, and every other bot's, are dropped here:
			// otherwise a comment that quotes the command would answer itself.
			return store.Job{}, "the comment was written by a bot"
		}
		command, ok := parseCommand(w.botLogin, d.Comment.Body)
		if !ok {
			return store.Job{}, fmt.Sprintf("the comment carries no @%s command", w.botLogin)
		}
		if command.Verb != commandRemember && command.Verb != commandReview {
			return store.Job{}, fmt.Sprintf("command %q is not supported", command.Verb)
		}
		if command.Verb == commandRemember && command.Instruction == "" {
			return store.Job{}, "the command carries no instruction"
		}
		// A command costs the operator model calls, and a review command
		// spends them whether or not it records anything, so both verbs need
		// the same standing.
		if !w.mayCommand(d) {
			// Dropped here rather than in the worker, so an outsider's command
			// never reaches the database, the model, or the embedder: the point
			// is that it costs the operator nothing.
			return store.Job{}, fmt.Sprintf("a commenter with author_association %q cannot command Rumil on a public repository",
				d.Comment.AuthorAssociation)
		}

		if eventType == eventIssueComment {
			// GitHub Issues are out of scope: only a comment on a pull request
			// carries the number the queue needs.
			if d.Issue == nil || !d.Issue.isPullRequest() {
				return store.Job{}, "comment is not on a pull request"
			}
			return w.job(eventType, d, d.Issue.Number), ""
		}
		if d.PullRequest == nil {
			return store.Job{}, "the payload carries no pull request"
		}
		return w.job(eventType, d, d.PullRequest.Number), ""

	default:
		return store.Job{}, fmt.Sprintf("event %q is not handled", eventType)
	}
}

func (w *Webhook) job(eventType string, d payload, prNumber int) store.Job {
	return store.Job{
		InstallationID: d.Installation.ID,
		EventType:      eventType,
		OrgName:        d.Repository.Owner.Login,
		RepoName:       d.Repository.Name,
		PRNumber:       prNumber,
	}
}

// isBotReviewer reports whether the payload asked Rumil to review.
func isBotReviewer(botLogin string, requested *requestedReviewer) bool {
	if requested == nil || botLogin == "" {
		return false
	}
	return requested.Type == "Bot" && ghapp.NormalizeLogin(requested.Login) == botLogin
}

// mayCommand reports whether the commenter's standing is enough to command
// Rumil. A public repository is readable by everyone, so anyone can post a
// command on it; accepting it would let an outsider queue work that spends the
// operator's model and embedding credits, so a public repository accepts a
// command only from a commenter with a standing in it. A private repository
// needs no check, because everybody who can comment on one already has access
// to it.
func (w *Webhook) mayCommand(d payload) bool {
	if d.Repository.Private {
		return true
	}
	if d.Comment == nil {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(d.Comment.AuthorAssociation)) {
	case "OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR":
		return true
	default:
		return false
	}
}

type payload struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		// Private decides whether a commenter's standing has to be checked
		// before a command is accepted: everybody who can see a private
		// repository already has access to it.
		Private bool `json:"private"`
	} `json:"repository"`
	PullRequest       *pullRequest       `json:"pull_request"`
	RequestedReviewer *requestedReviewer `json:"requested_reviewer"`
	Issue             *issue             `json:"issue"`
	Comment           *comment           `json:"comment"`
}

// comment is the comment a command arrives on, which is a review-thread reply
// or a conversation comment depending on the event.
type comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	// AuthorAssociation is the commenter's standing in the repository. It is
	// what the untrusted-commenter guard reads, and what decides whether a rule
	// this comment records is exempt from moderation.
	AuthorAssociation string `json:"author_association"`
	HTMLURL           string `json:"html_url"`
	// The next three are set on a review-thread reply only.
	Path        string `json:"path"`
	DiffHunk    string `json:"diff_hunk"`
	InReplyToID int64  `json:"in_reply_to_id"`
}

type pullRequest struct {
	Number int `json:"number"`
}

type requestedReviewer struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type issue struct {
	Number      int             `json:"number"`
	PullRequest json.RawMessage `json:"pull_request"`
}

// isPullRequest reports whether the issue payload describes a pull request.
// GitHub omits the key for a plain issue and sends null in some payloads.
func (i issue) isPullRequest() bool {
	trimmed := strings.TrimSpace(string(i.PullRequest))
	return trimmed != "" && trimmed != "null"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoded, err := json.Marshal(body)
	if err != nil {
		return
	}
	_, _ = w.Write(encoded)
}
