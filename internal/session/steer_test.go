package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestSteerSendsDistinctClientMessageIdentity(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	openThread(t, s, "a")
	for range 2 {
		if _, err := s.Send(context.Background(), "a", "same deliberate input", "steer", "turn-1"); err != nil {
			t.Fatal(err)
		}
	}

	f.mu.Lock()
	calls := append([]fakeCall(nil), f.calls...)
	f.mu.Unlock()
	seen := make(map[string]bool)
	for _, call := range calls {
		if call.method != "turn/steer" {
			continue
		}
		var params struct {
			ThreadID            string `json:"threadId"`
			ExpectedTurnID      string `json:"expectedTurnId"`
			ClientUserMessageID string `json:"clientUserMessageId"`
			Input               []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		}
		if err := json.Unmarshal(call.params, &params); err != nil {
			t.Fatal(err)
		}
		id := params.ClientUserMessageID
		if len(id) < 26 || seen[id] || id == params.ThreadID || id == params.ExpectedTurnID {
			t.Fatal("steer requires a distinct opaque client message identity")
		}
		seen[id] = true
		if params.ThreadID != "a" || params.ExpectedTurnID != "turn-1" || len(params.Input) != 1 ||
			params.Input[0].Type != "text" || params.Input[0].Text != "same deliberate input" {
			t.Fatalf("steer changed the target or input: %+v", params)
		}
	}
	if len(seen) != 2 {
		t.Fatal("each deliberate steer must dispatch exactly once")
	}
}

func TestSteerUnknownOutcomeDoesNotGenerateRetry(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	openThread(t, s, "a")
	f.setHook(func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error, bool) {
		if method != "turn/steer" {
			return nil, nil, false
		}
		var input map[string]json.RawMessage
		if err := json.Unmarshal(params, &input); err != nil || len(input["clientUserMessageId"]) < 3 {
			t.Fatal("dispatched steer omitted its client message identity")
		}
		return nil, errors.New("lost acknowledgement"), true
	})
	if _, err := s.Send(context.Background(), "a", "only once", "steer", "turn-1"); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if f.count("turn/steer") != 1 {
		t.Fatal("unknown steering outcome was replayed")
	}
}
