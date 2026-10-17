package server

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joangrigorov/rumil/internal/store"
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
