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
	"github.com/joangrigorov/rumil/internal/metrics"
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
	// eventReaction is a reaction on a review comment, which is the cheapest
	// signal that a finding landed.
	eventReaction = "reaction"
	// eventPullRequestReview carries a dismissed review, which says its
	// findings were not accepted.
	eventPullRequestReview = "pull_request_review"
)

// The commands the conversation supports.
const (
	// commandRemember records a rule from the comment.
	commandRemember = "remember"
	// commandReview asks for a review of the pull request the comment is on.
	// It exists because a GitHub App cannot be requested as a reviewer from
	// every surface: the API refuses one, and the people who hit that can ask
	// in the conversation instead.
	commandReview = "review"
	// commandWhy and commandExplain ask Rumil to explain a finding it posted,
	// as a reply inside the review thread the finding opened.
	commandWhy     = "why"
	commandExplain = "explain"
)

// signatureHeader carries the HMAC of the raw webhook body.
const signatureHeader = "X-Hub-Signature-256"

// maxWebhookBytes bounds the body read, per §8.
const maxWebhookBytes = 10 << 20

// healthTimeout bounds the database check behind /healthz.
const healthTimeout = 2 * time.Second

// Webhook verifies and dispatches GitHub deliveries.
type Webhook struct {
	secret []byte
	// secretSource is read on each delivery, so a reloaded webhook secret
	// takes effect without rebuilding the server.
	secretSource   func() string
	jobs           *store.DB
	botLogin       string
	installationID int64
	installations  map[int64]bool
	logger         *slog.Logger
	meter          *metrics.Metrics
}

// WebhookOption tunes a webhook at construction.
type WebhookOption func(*Webhook)

// WithWebhookMetrics counts deliveries and the jobs they queue.
func WithWebhookMetrics(meter *metrics.Metrics) WebhookOption {
	return func(w *Webhook) { w.meter = meter }
}

// WithWebhookSecretSource reads the webhook secret on each delivery, so a
// reloaded secret takes effect without rebuilding the server. The constructor
// secret is the initial value used when no source is set.
func WithWebhookSecretSource(source func() string) WebhookOption {
	return func(w *Webhook) { w.secretSource = source }
}

// WithInstallations is the set of installations this instance serves. Empty
// keeps the single installation from the constructor, and a delivery for any
// other account is ignored before it reaches the queue.
func WithInstallations(ids []int64) WebhookOption {
	return func(w *Webhook) {
		w.installations = make(map[int64]bool, len(ids))
		for _, id := range ids {
			w.installations[id] = true
		}
	}
}

// allows reports whether this instance serves an installation. A webhook built
// without an installation id serves any, which is only the case in a test that
// does not care about tenancy.
func (w *Webhook) allows(installationID int64) bool {
	if len(w.installations) == 0 {
		return w.installationID <= 0 || installationID == w.installationID
	}
	return w.installations[installationID]
}

// NewWebhook takes the configured webhook secret and bot handle, so it can
// tell a review request aimed at Rumil from one aimed at a person, and the
// primary installation Rumil serves. An App's webhook secret is account-wide,
// so a delivery signed with it can belong to any installation of the App; only
// a configured one has an installation token the engine can use. WithInstallations
// replaces the constructor's single id with the whole set.
func NewWebhook(secret, botHandle string, installationID int64, jobs *store.DB, logger *slog.Logger, opts ...WebhookOption) *Webhook {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	installations := map[int64]bool{}
	if installationID > 0 {
		installations[installationID] = true
	}
	webhook := &Webhook{
		secret:         []byte(secret),
		jobs:           jobs,
		botLogin:       ghapp.NormalizeLogin(botHandle),
		installationID: installationID,
		installations:  installations,
		logger:         logger,
	}
	for _, opt := range opts {
		opt(webhook)
	}
	return webhook
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
	// An authenticated delivery is received whether or not it becomes a job;
	// the outcome labels tell the two apart.
	w.meter.Job(metricEvent(eventType), "received")

	// Feedback on an earlier finding is recorded before the delivery is
	// classified: a reply that is not a command, a reaction and a dismissal are
	// all dropped as jobs, and their outcome is the whole point of reading them.
	w.recordFindingFeedback(r.Context(), eventType, delivery)

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
		w.meter.Job(metricEvent(eventType), "queued")
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

// recordFindingFeedback attaches what later deliveries say about a finding. It
// is best effort: a delivery that cannot be recorded is still answered, because
// GitHub retries a non-2xx and the finding is not more important than the
// webhook staying healthy.
func (w *Webhook) recordFindingFeedback(ctx context.Context, eventType string, d payload) {
	if !w.allows(d.Installation.ID) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var err error
	switch eventType {
	case eventReviewComment:
		if d.Action == "created" && d.Comment != nil {
			err = w.jobs.MarkFindingReplied(ctx, d.Comment.InReplyToID)
		}
	case eventReaction:
		if d.Comment != nil && d.Reaction != nil {
			err = w.jobs.MarkFindingReaction(ctx, d.Comment.ID, d.Reaction.Content)
		}
	case eventPullRequestReview:
		if d.Action == "dismissed" && d.Review != nil {
			err = w.jobs.MarkReviewFindingsDismissed(ctx, d.Review.ID)
		}
	case eventPullRequest:
		if d.Action == "synchronize" && d.PullRequest != nil {
			err = w.jobs.MarkPRFindingsHeadMoved(ctx, d.Repository.Owner.Login, d.Repository.Name, d.PullRequest.Number)
		}
	}
	if err != nil {
		w.logger.Warn("recording finding feedback failed",
			"event_type", eventType, "action", d.Action, "error", err)
	}
}

// validSignature compares the HMAC of the body with the header GitHub sent.
func (w *Webhook) validSignature(header string, body []byte) bool {
	secret := w.secret
	if w.secretSource != nil {
		secret = []byte(w.secretSource())
	}
	if len(secret) == 0 || header == "" {
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

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), provided)
}

// classify decides whether a delivery becomes a job. It returns the job to
// enqueue, or a reason to ignore the delivery.
//
// Reviewing is never automatic: the only pull request action that counts is a
// review request aimed at Rumil itself.
func (w *Webhook) classify(eventType string, d payload) (store.Job, string) {
	if !w.allows(d.Installation.ID) {
		// Dropped before anything is written: a job for an installation Rumil
		// cannot authenticate as would fail after the fact, and a remember
		// command would leave a row behind while it did.
		return store.Job{}, fmt.Sprintf("the delivery is for installation %d, which this Rumil does not serve", d.Installation.ID)
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
		if command.Verb != commandRemember && command.Verb != commandReview &&
			!isAnswerVerb(command.Verb) {
			return store.Job{}, fmt.Sprintf("command %q is not supported", command.Verb)
		}
		if command.Verb == commandRemember && command.Instruction == "" {
			return store.Job{}, "the command carries no instruction"
		}
		if isAnswerVerb(command.Verb) && eventType != eventReviewComment {
			// An answer needs the hunk and the thread. A conversation comment
			// has neither, so the person is asked to reply where the finding is.
			return store.Job{}, "why and explain are answered inside a review thread only"
		}
		// A command costs the operator model calls, and every verb spends them
		// whether or not it records anything, so all of them need the same
		// standing.
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
	Reaction          *reaction          `json:"reaction"`
	Review            *review            `json:"review"`
}

type reaction struct {
	Content string `json:"content"`
}

type review struct {
	ID int64 `json:"id"`
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
	// The next four are set on a review-thread reply only. Line is the line the
	// comment hangs on in the reviewed diff; OriginalLine is the line it was
	// written on, which is what remains when the comment is outdated.
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"original_line"`
	DiffHunk     string `json:"diff_hunk"`
	InReplyToID  int64  `json:"in_reply_to_id"`
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
