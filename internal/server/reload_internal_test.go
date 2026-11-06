package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/config"
	"github.com/joangrigorov/rumil/internal/reviewer"
)

// TestAdminAuthReadsUsersOnEachRequest proves that a reloaded user list takes
// effect without rebuilding the surface.
func TestAdminAuthReadsUsersOnEachRequest(t *testing.T) {
	users := []config.AdminUser{{Name: "first", PasswordHash: "$2a$10$HEbCK2/.V0nBjO/13L3Mu.OnAoDOLDZdUZEg0irk5uO0jTZhw0Rba"}}
	auth := NewAdminAuthSource(func() []config.AdminUser { return users }, nil)
	handler := auth.authorize(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	request := func(username, password string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
		recorder := httptest.NewRecorder()
		handler(recorder, r)
		return recorder
	}

	if got := request("first", "test-admin-password").Code; got != http.StatusNoContent {
		t.Fatalf("the first user was refused with %d", got)
	}

	users = []config.AdminUser{{Name: "second", PasswordHash: "$2a$10$HEbCK2/.V0nBjO/13L3Mu.OnAoDOLDZdUZEg0irk5uO0jTZhw0Rba"}}

	if got := request("first", "test-admin-password").Code; got != http.StatusUnauthorized {
		t.Errorf("the removed user still authenticated with %d", got)
	}
	if got := request("second", "test-admin-password").Code; got != http.StatusNoContent {
		t.Errorf("the added user was refused with %d", got)
	}
}

// TestWebhookReadsSecretOnEachDelivery proves that a reloaded webhook secret
// applies to the next delivery.
func TestWebhookReadsSecretOnEachDelivery(t *testing.T) {
	secret := "first-secret"
	webhook := NewWebhook(secret, "@rumil", 1, nil, nil,
		WithWebhookSecretSource(func() string { return secret }))

	body := []byte(`{"action":"created"}`)
	first := webhookSignature("first-secret", body)
	if !webhook.validSignature(first, body) {
		t.Fatal("the first secret did not verify its own signature")
	}

	secret = "second-secret"
	if webhook.validSignature(first, body) {
		t.Error("the replaced secret still verified the old signature")
	}
	if !webhook.validSignature(webhookSignature("second-secret", body), body) {
		t.Error("the replacement secret did not verify its own signature")
	}
}

// TestPoolTakesOneEngineAndTimeoutPerJob proves that a job keeps the engine and
// budget it started with when a reload lands between jobs.
func TestPoolTakesOneEngineAndTimeoutPerJob(t *testing.T) {
	first := &reviewer.Engine{}
	second := &reviewer.Engine{}
	engine := first
	cfg := &config.Config{Worker: config.Worker{JobTimeout: "5m"}}

	pool := NewPool(nil, first, "@rumil", 1, nil,
		WithEngineSource(func() *reviewer.Engine { return engine }),
		WithConfigSource(func() *config.Config { return cfg }))

	run := pool.newRunner()
	if run.engine != first {
		t.Error("the first job did not take the first engine")
	}
	if run.timeout != 5*time.Minute {
		t.Errorf("the first job timeout = %s, want 5m", run.timeout)
	}

	engine = second
	cfg = &config.Config{Worker: config.Worker{JobTimeout: "30m"}}

	run = pool.newRunner()
	if run.engine != second {
		t.Error("the next job did not take the replacement engine")
	}
	if run.timeout != 30*time.Minute {
		t.Errorf("the next job timeout = %s, want 30m", run.timeout)
	}
}

// webhookSignature is the X-Hub-Signature-256 value GitHub would send.
func webhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
