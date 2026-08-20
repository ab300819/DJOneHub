package main

import (
	"encoding/json"
	"sync"
	"testing"
)

func decodeResponse(t *testing.T, raw []byte) Response {
	t.Helper()
	var response Response
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	return response
}

// A transport has no way to report a request it could not even parse, so Call
// has to answer with a frame rather than failing.
func TestCallReportsMalformedRequestAsAResponse(t *testing.T) {
	response := decodeResponse(t, Call(newDemoApp(), []byte("not json")))

	if response.OK {
		t.Fatal("malformed request reported success")
	}
	if response.ID != 0 {
		t.Errorf("ID = %d, want 0 for a request whose ID could not be read", response.ID)
	}
	if response.Kind != "invalid" {
		t.Errorf("Kind = %q, want %q", response.Kind, "invalid")
	}
}

// Kind is what lets a client branch without matching on message text, so the
// classification has to survive the trip through Call.
func TestCallClassifiesFailures(t *testing.T) {
	instance := newDemoApp()
	for _, testCase := range []struct {
		name    string
		request string
		want    string
	}{
		{"unknown method is a client error", `{"id":1,"method":"nope"}`, "invalid"},
		{"rejected AT command", `{"id":2,"method":"at","params":{"command":"HELLO"}}`, "invalid"},
		{"missing SMS fields", `{"id":3,"method":"sms.send","params":{}}`, "invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := decodeResponse(t, Call(instance, []byte(testCase.request)))
			if response.OK {
				t.Fatal("expected a failure")
			}
			if response.Kind != testCase.want {
				t.Errorf("Kind = %q, want %q", response.Kind, testCase.want)
			}
		})
	}
}

func TestCallAnswersWithTheRequestID(t *testing.T) {
	response := decodeResponse(t, Call(newDemoApp(), []byte(`{"id":77,"method":"health"}`)))

	if !response.OK {
		t.Fatalf("health failed: %s", response.Error)
	}
	if response.ID != 77 {
		t.Errorf("ID = %d, want 77", response.ID)
	}
}

type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *recordingSink) Emit(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

// Transports that cannot push never install a sink, and emitting has to stay
// harmless rather than panicking on the nil.
func TestEmitWithoutASinkIsHarmless(t *testing.T) {
	newDemoApp().emit(Event{Event: EventESIMDownloadProgress, Percent: 10})
}

func TestEmitReachesTheInstalledSink(t *testing.T) {
	instance := newDemoApp()
	sink := &recordingSink{}
	instance.SetEventSink(sink)

	instance.emit(Event{Event: EventESIMDownloadProgress, Percent: 42, Message: "downloading"})

	if len(sink.events) != 1 {
		t.Fatalf("sink saw %d events, want 1", len(sink.events))
	}
	if got := sink.events[0]; got.Percent != 42 || got.Event != EventESIMDownloadProgress {
		t.Errorf("event = %+v", got)
	}
}

// Events share the stream with responses, so a client tells them apart by
// shape: an event carries "event" and no "id".
func TestEventFrameCarriesNoID(t *testing.T) {
	encoded, err := json.Marshal(Event{Event: EventESIMDownloadProgress, Percent: 5})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := fields["id"]; present {
		t.Error("event frame carries an id, which would be read as a response")
	}
	if fields["event"] != EventESIMDownloadProgress {
		t.Errorf("event = %v", fields["event"])
	}
}
