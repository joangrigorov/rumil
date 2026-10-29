package continuation

import (
	"errors"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/store"
)

func turnLimitedParent() store.ReviewSession {
	return store.ReviewSession{
		ID:               "11111111-1111-1111-1111-111111111111",
		InstallationID:   98765432,
		OrgName:          "acme",
		RepoName:         "payments",
		PRNumber:         42,
		HeadSHA:          "headsha",
		Status:           store.ReviewFailed,
		TurnLimitReached: true,
	}
}

func TestNewRequestRefusesASessionThatCannotContinue(t *testing.T) {
	cases := []struct {
		name    string
		session store.ReviewSession
		want    error
	}{
		{
			name: "a submitted review",
			session: store.ReviewSession{
				Status: store.ReviewSucceeded, ReviewEvent: "COMMENT",
			},
			want: store.ErrReviewAlreadySubmitted,
		},
		{
			name:    "a running session",
			session: store.ReviewSession{Status: store.ReviewRunning},
			want:    store.ErrReviewNotFailed,
		},
		{
			name: "a failure with another cause",
			session: store.ReviewSession{
				Status: store.ReviewFailed, Error: "the GitHub App is not installed",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRequest(tc.session, 5, "cli"); !errors.Is(err, tc.want) {
				t.Errorf("NewRequest() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewRequestValidatesItsInput(t *testing.T) {
	parent := turnLimitedParent()

	if _, err := NewRequest(parent, -1, "cli"); err == nil || !strings.Contains(err.Error(), "turns") {
		t.Errorf("negative turns = %v, want an error naming turns", err)
	}
	if _, err := NewRequest(parent, 5, "  "); err == nil || !strings.Contains(err.Error(), "requested_by") {
		t.Errorf("empty requester = %v, want an error naming requested_by", err)
	}
}

func TestJobCarriesTheRequestAndTheParentsCoordinates(t *testing.T) {
	parent := turnLimitedParent()

	request, err := NewRequest(parent, 7, " alice ")
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	if request.RequestedBy != "alice" {
		t.Errorf("requested_by = %q, want the trimmed alice", request.RequestedBy)
	}
	if request.RequestedAt.IsZero() {
		t.Error("requested_at is zero")
	}

	job, err := NewJob(parent, request)
	if err != nil {
		t.Fatalf("NewJob returned error: %v", err)
	}
	if job.DeliveryID != DeliveryID(parent.ID) {
		t.Errorf("delivery_id = %q, want %q", job.DeliveryID, DeliveryID(parent.ID))
	}
	if job.EventType != EventType {
		t.Errorf("event_type = %q, want %q", job.EventType, EventType)
	}
	if job.InstallationID != parent.InstallationID || job.OrgName != parent.OrgName ||
		job.RepoName != parent.RepoName || job.PRNumber != parent.PRNumber {
		t.Errorf("job coordinates = %+v, want the parent's", job)
	}

	parsed, err := Parse(job.Payload)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if parsed.SessionID != parent.ID || parsed.Turns != 7 || parsed.RequestedBy != "alice" {
		t.Errorf("parsed request = %+v, want the queued one", parsed)
	}
	if !parsed.RequestedAt.Equal(request.RequestedAt) {
		t.Errorf("requested_at = %v, want %v", parsed.RequestedAt, request.RequestedAt)
	}
}

func TestParseRefusesAMalformedRequest(t *testing.T) {
	cases := map[string]string{
		"not JSON":       `{`,
		"no session":     `{"turns":5,"requested_by":"cli"}`,
		"negative turns": `{"session_id":"s","turns":-1,"requested_by":"cli"}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(payload)); err == nil {
				t.Errorf("Parse(%s) succeeded, want an error", payload)
			}
		})
	}
}

// A payload written by an older or hand-built surface still names somebody, so
// the session record is never blank.
func TestParseDefaultsTheRequesterAndTime(t *testing.T) {
	request, err := Parse([]byte(`{"session_id":"s","turns":0}`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if request.RequestedBy != "unknown" {
		t.Errorf("requested_by = %q, want unknown", request.RequestedBy)
	}
	if request.RequestedAt.IsZero() {
		t.Error("requested_at is zero")
	}
}
