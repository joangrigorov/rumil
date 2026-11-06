package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/reviewer"
	"github.com/joangrigorov/rumil/internal/store"
	"github.com/joangrigorov/rumil/internal/testutil"
)

func TestBrokerFansOutAndUnsubscribes(t *testing.T) {
	deck := newBroker()
	first, stopFirst := deck.subscribe("session-1")
	defer stopFirst()
	second, stopSecond := deck.subscribe("session-1")

	event := store.ReviewEvent{Seq: 1, At: time.Now(), Type: "step_start", Payload: []byte(`{"step":1}`)}
	deck.publish("session-1", event)

	for name, channel := range map[string]<-chan store.ReviewEvent{"first": first, "second": second} {
		select {
		case received := <-channel:
			if received.Seq != event.Seq || received.Type != event.Type {
				t.Errorf("%s subscriber received %+v, want %+v", name, received, event)
			}
		case <-time.After(time.Second):
			t.Errorf("%s subscriber received nothing", name)
		}
	}

	stopSecond()
	deck.publish("session-1", store.ReviewEvent{Seq: 2, Type: "tool_call"})

	select {
	case received := <-first:
		if received.Seq != 2 {
			t.Errorf("first subscriber received seq %d, want 2", received.Seq)
		}
	case <-time.After(time.Second):
		t.Error("first subscriber received nothing after the second left")
	}

	if _, ok := <-second; ok {
		t.Error("the unsubscribed channel is still open")
	}

	if len(deck.subscribers["session-1"]) != 1 {
		t.Errorf("subscribers = %d, want 1", len(deck.subscribers["session-1"]))
	}
}

func TestBrokerDropsForASlowSubscriber(t *testing.T) {
	deck := newBroker()
	channel, stop := deck.subscribe("session-1")
	defer stop()

	for i := 0; i < 300; i++ {
		deck.publish("session-1", store.ReviewEvent{Seq: i + 1, Type: "reasoning_delta"})
	}

	if got := len(channel); got != 256 {
		t.Errorf("buffered events = %d, want the channel's 256", got)
	}
}

func TestIsDeltaEvent(t *testing.T) {
	for _, eventType := range []string{"reasoning_delta", "message_delta", "tool_output"} {
		if !isDeltaEvent(eventType) {
			t.Errorf("isDeltaEvent(%q) = false, want true", eventType)
		}
	}
	for _, eventType := range []string{"session_metadata", "tool_call", "tool_result", "session_end"} {
		if isDeltaEvent(eventType) {
			t.Errorf("isDeltaEvent(%q) = true, want false", eventType)
		}
	}
}

// A running session's count is written as tool_start events arrive, so the
// list and Home show progress instead of zero until the review ends.
func TestObserverRecordsRunningToolCalls(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, testutil.TestDatabaseURL(t, "observer_tool_calls"), true, 0)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(db.Close)

	recorder := NewSessionRecorder(db, newBroker(), nil, nil)
	observer, err := recorder.Start(ctx, store.NewReviewSession{
		InstallationID: 98765432,
		OrgName:        "acme",
		RepoName:       "payments",
		PRNumber:       42,
	})
	if err != nil {
		t.Fatalf("starting the recorder: %v", err)
	}

	for i := 0; i < 2; i++ {
		observer.Observe(ctx, reviewer.Event{Type: "tool_start", Payload: map[string]any{"id": i}})
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		session, err := db.ReviewSessionByID(ctx, observer.sessionID)
		if err != nil {
			t.Fatalf("reading the session: %v", err)
		}
		if session.ToolCalls == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("running tool_calls = %d, want 2", session.ToolCalls)
		}
		time.Sleep(20 * time.Millisecond)
	}

	observer.Finish(ctx, store.ReviewOutcome{Status: store.ReviewSucceeded, ToolCalls: 2})
}

func TestLastEventIDPrefersTheHeader(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/sessions/1/events?after=3", nil)
	request.Header.Set("Last-Event-ID", "7")

	if got := lastEventID(request); got != 7 {
		t.Errorf("lastEventID = %d, want the header's 7", got)
	}

	request.Header.Del("Last-Event-ID")
	if got := lastEventID(request); got != 3 {
		t.Errorf("lastEventID = %d, want the query's 3", got)
	}

	request = httptest.NewRequest("GET", "/api/sessions/1/events?after=nonsense", nil)
	if got := lastEventID(request); got != 0 {
		t.Errorf("lastEventID = %d, want 0 for a malformed cursor", got)
	}
}
