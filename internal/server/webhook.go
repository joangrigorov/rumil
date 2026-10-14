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
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/joangrigorov/rumil/internal/store"
)

// Event types Rumil handles.
const (
	eventPullRequest  = "pull_request"
	eventIssueComment = "issue_comment"
)

// signatureHeader carries the HMAC of the raw webhook body.
const signatureHeader = "X-Hub-Signature-256"

// maxWebhookBytes bounds the body read, per §8.
const maxWebhookBytes = 10 << 20

// healthTimeout bounds the database check behind /healthz.
const healthTimeout = 2 * time.Second

// Webhook verifies and dispatches GitHub deliveries.
type Webhook struct {
	secret   []byte
	jobs     *store.DB
	botLogin string
	logger   *slog.Logger
}

// NewWebhook takes the configured webhook secret and bot handle, so it can
// tell a review request aimed at Rumil from one aimed at a person.
func NewWebhook(secret, botHandle string, jobs *store.DB, logger *slog.Logger) *Webhook {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Webhook{
		secret:   []byte(secret),
		jobs:     jobs,
		botLogin: normalizeBotLogin(botHandle),
		logger:   logger,
	}
}

// Handler builds the routes of `rumil serve`. Method patterns make the mux
// answer 405 for anything else.
func Handler(webhook *Webhook, db *store.DB) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/github", webhook)
	mux.HandleFunc("GET /healthz", health(db))
	return mux
}

func health(db *store.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": err.Error()})
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

	case eventIssueComment:
		// GitHub Issues are out of scope: only a comment on a pull request
		// carries the number the queue needs.
		if d.Issue == nil || !d.Issue.isPullRequest() {
			return store.Job{}, "comment is not on a pull request"
		}
		return w.job(eventType, d, d.Issue.Number), ""

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

// normalizeBotLogin turns the configured handle into the login GitHub reports:
// "@rumil" and "rumil[bot]" both become "rumil".
func normalizeBotLogin(handle string) string {
	login := strings.ToLower(strings.TrimSpace(handle))
	login = strings.TrimPrefix(login, "@")
	login = strings.TrimSuffix(login, "[bot]")
	return login
}

// isBotReviewer reports whether the payload asked Rumil to review.
func isBotReviewer(botLogin string, requested *requestedReviewer) bool {
	if requested == nil || botLogin == "" {
		return false
	}
	return requested.Type == "Bot" && normalizeBotLogin(requested.Login) == botLogin
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
	} `json:"repository"`
	PullRequest       *pullRequest       `json:"pull_request"`
	RequestedReviewer *requestedReviewer `json:"requested_reviewer"`
	Issue             *issue             `json:"issue"`
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

func writeJSON(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoded, err := json.Marshal(body)
	if err != nil {
		return
	}
	_, _ = w.Write(encoded)
}
