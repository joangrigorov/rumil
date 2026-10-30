package server_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/joangrigorov/rumil/internal/server"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

// An App's webhook secret is account-wide, and a multi-installation deployment
// serves every account the operator listed. A delivery through one of them is
// queued with its own installation id, which is what the per-installation pool
// claims by.
func TestDeliveryThroughASecondInstallationIsQueued(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})

	webhook := server.NewWebhook(h.cfg.GitHub.WebhookSecret, h.cfg.App.BotHandle, h.cfg.GitHub.InstallationID,
		h.db, h.logger, server.WithInstallations([]int64{98765432, 55555555}))
	handler := server.Handler(webhook, h.db, nil)

	body := bytes.Replace(
		reviewRequestedPayload("review_requested", "rumil[bot]", "Bot"),
		[]byte(`"installation":{"id":98765432}`),
		[]byte(`"installation":{"id":55555555}`),
		1,
	)
	if !bytes.Contains(body, []byte("55555555")) {
		t.Fatalf("the fixture does not carry the second installation: %s", body)
	}

	response := h.deliver(t, handler, "pull_request", "d-second-installation", body, sign(webhookSecret, body))
	if response.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 for a configured installation", response.Code)
	}

	job, err := h.db.JobByDelivery(context.Background(), "d-second-installation")
	if err != nil {
		t.Fatalf("JobByDelivery returned error: %v", err)
	}
	if job == nil {
		t.Fatal("the delivery queued no job")
	}
	if job.InstallationID != 55555555 {
		t.Errorf("installation_id = %d, want the delivery's own installation", job.InstallationID)
	}
}

func TestDeliveryForAnUnlistedInstallationIsIgnoredWhenSeveralAreServed(t *testing.T) {
	gh := testutil.NewFakeGitHub(t)
	llm := testutil.NewFakeLLM(t)
	h := newHarness(t, gh, llm, harnessOpts{})

	webhook := server.NewWebhook(h.cfg.GitHub.WebhookSecret, h.cfg.App.BotHandle, h.cfg.GitHub.InstallationID,
		h.db, h.logger, server.WithInstallations([]int64{98765432, 55555555}))
	handler := server.Handler(webhook, h.db, nil)

	body := bytes.Replace(
		reviewRequestedPayload("review_requested", "rumil[bot]", "Bot"),
		[]byte(`"installation":{"id":98765432}`),
		[]byte(`"installation":{"id":44444444}`),
		1,
	)

	response := h.deliver(t, handler, "pull_request", "d-unlisted-installation", body, sign(webhookSecret, body))
	if response.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with an ignored notice", response.Code)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("ignored")) {
		t.Errorf("body = %s, want an ignored notice", response.Body.String())
	}
	if queued, err := h.db.CountJobs(context.Background(), store.StatusQueued); err != nil {
		t.Fatalf("CountJobs returned error: %v", err)
	} else if queued != 0 {
		t.Errorf("queued jobs = %d, want the unlisted installation to enqueue nothing", queued)
	}
}
