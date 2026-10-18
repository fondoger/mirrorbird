package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"mirrorbird/internal/session"
)

type companionFake struct {
	*fakeBackend
	calls   atomic.Int32
	input   session.MessageInput
	action  session.ThreadAction
	failure error
}

func (b *companionFake) Options(context.Context, string, string, string) (session.ComposerOptions, error) {
	b.calls.Add(1)
	return session.ComposerOptions{Models: []session.Model{{ID: "m", Model: "m", Name: "Model"}}}, b.failure
}
func (b *companionFake) History(_ context.Context, id, cursor string, version uint64) (session.HistoryPage, error) {
	b.calls.Add(1)
	return session.HistoryPage{Turns: []session.Turn{{ID: id + "-" + cursor, Status: "completed", Items: []session.Item{}}}, HistoryVersion: version}, b.failure
}
func (b *companionFake) CreateThread(_ context.Context, _ string, input session.MessageInput) (json.RawMessage, error) {
	b.calls.Add(1)
	b.input = input
	return json.RawMessage(`{"threadId":"created","secret":"must not escape"}`), b.failure
}
func (b *companionFake) Enqueue(_ context.Context, _, _ string, input session.MessageInput) (json.RawMessage, error) {
	b.calls.Add(1)
	b.input = input
	return json.RawMessage(`{}`), b.failure
}
func (b *companionFake) Act(_ context.Context, _ string, action session.ThreadAction) (json.RawMessage, error) {
	b.calls.Add(1)
	b.action = action
	return json.RawMessage(`{}`), b.failure
}

func companionTestServer(t *testing.T) (*Server, *companionFake, string) {
	t.Helper()
	b := &companionFake{fakeBackend: newFakeBackend()}
	s := New(b, fstest.MapFS{"index.html": {Data: []byte("preview")}})
	s.SetSharing(true)
	t.Cleanup(func() { s.SetSharing(false) })
	return s, b, requestCSRF(t, s)
}

func TestCompanionRoutesEnforceSharingProofAndProjection(t *testing.T) {
	for _, path := range []string{"/api/threads", "/api/threads/thread/queue", "/api/threads/thread/actions"} {
		t.Run(path, func(t *testing.T) {
			s, b, csrf := companionTestServer(t)
			if w := request(s, "", "POST", path, `{}`); w.Code != http.StatusForbidden {
				t.Fatal(w.Code, w.Body.String())
			}
			if b.calls.Load() != 0 {
				t.Fatal("mutation reached backend without proof")
			}
			s.SetSharing(false)
			if w := request(s, csrf, "POST", path, `{}`); w.Code != http.StatusForbidden {
				t.Fatal(w.Code)
			}
			if b.calls.Load() != 0 {
				t.Fatal("stopped service accepted mutation")
			}
		})
	}
	s, b, csrf := companionTestServer(t)
	if w := request(s, csrf, "POST", "/api/threads/unloaded/queue", `{"requestId":"1234567890123456","text":"hello"}`); w.Code != http.StatusConflict {
		t.Fatal(w.Code, w.Body.String())
	}
	if b.calls.Load() != 0 {
		t.Fatal("queue hydrated an unloaded thread")
	}
}

func TestCompanionCreateIsDeduplicatedAndOnlyReturnsNewID(t *testing.T) {
	s, b, csrf := companionTestServer(t)
	body := `{"requestId":"1234567890123456","projectId":"project","text":"hello","model":"m","effort":"high"}`
	for range 2 {
		w := request(s, csrf, "POST", "/api/threads", body)
		if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"threadId":"created"`) || strings.Contains(w.Body.String(), "secret") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if b.calls.Load() != 1 || b.input.Text != "hello" || b.input.Model != "m" || b.input.Effort != "high" {
		t.Fatal(b.calls.Load(), b.input)
	}
	if w := request(s, csrf, "POST", "/api/threads", strings.Replace(body, "hello", "different", 1)); w.Code != http.StatusConflict {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCompanionOptionsHistoryAndActionsValidateBoundary(t *testing.T) {
	s, b, csrf := companionTestServer(t)
	for _, path := range []string{"/api/options?kind=models", "/api/threads/thread/history?cursor=older&version=12"} {
		if w := request(s, "", "GET", path, ""); w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/options?kind=models&kind=skills", "/api/options?unexpected=value", "/api/threads/thread/history?cursor=older&version=no"} {
		if w := request(s, "", "GET", path, ""); w.Code != http.StatusBadRequest {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if b.calls.Load() != 2 {
		t.Fatal("invalid query reached backend")
	}
	w := request(s, csrf, "POST", "/api/threads/thread/actions", `{"requestId":"1234567890123456","kind":"queue-guide","queueId":"head"}`)
	if w.Code != http.StatusAccepted || b.action.Kind != "queue-guide" || b.action.QueueID != "head" {
		t.Fatal(w.Code, b.action)
	}
	b.failure = session.ErrUnsupported
	if w := request(s, "", "GET", "/api/options?kind=models", ""); w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), `"code":"unsupported"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	b.failure = session.ErrAttachmentOutdated
	if w := request(s, "", "GET", "/api/options?kind=projects", ""); w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), `"code":"attachment_outdated"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
