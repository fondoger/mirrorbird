package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"mirrorbird/internal/session"
)

type newThreadFake struct {
	*fakeBackend
	calls atomic.Int32
	read  func(context.Context) (session.NewThreadContext, error)
}

func (b *newThreadFake) NewThread(ctx context.Context) (session.NewThreadContext, error) {
	b.calls.Add(1)
	if b.read != nil {
		return b.read(ctx)
	}
	return session.NewThreadContext{
		Projects: []session.Project{{ID: "project", Name: "Workspace"}},
		Models:   []session.Model{{ID: "model-id", Model: "model", Name: "Model"}},
		Defaults: session.NewThreadDefaults{Model: "model", Effort: "high"},
	}, nil
}

func newThreadTestServer(t *testing.T) (*Server, *newThreadFake) {
	t.Helper()
	b := &newThreadFake{fakeBackend: newFakeBackend()}
	s := New(b, fstest.MapFS{"index.html": {Data: []byte("preview")}})
	s.SetSharing(true)
	t.Cleanup(func() { s.SetSharing(false) })
	return s, b
}

func TestNewThreadRouteReturnsOneCompleteContext(t *testing.T) {
	s, b := newThreadTestServer(t)
	w := request(s, "", "GET", "/api/new-thread", "")
	var result session.NewThreadContext
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil ||
		len(result.Projects) != 1 || result.Projects[0].ID != "project" || len(result.Models) != 1 ||
		result.Defaults != (session.NewThreadDefaults{Model: "model", Effort: "high"}) || b.calls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), b.calls.Load())
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &fields) != nil || len(fields) != 3 || fields["projects"] == nil || fields["models"] == nil || fields["defaults"] == nil {
		t.Fatal("unexpected aggregate shape", w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("composer metadata must not be cached")
	}
}

func TestNewThreadRouteRejectsInvalidQueriesAndAccess(t *testing.T) {
	s, b := newThreadTestServer(t)
	for _, path := range []string{"/api/new-thread?kind=projects", "/api/new-thread?cursor=next", "/api/new-thread?projectId=p", "/api/new-thread?bad=%"} {
		if w := request(s, "", "GET", path, ""); w.Code != http.StatusBadRequest {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "http://localhost/api/new-thread", nil)
	r.Header.Set("Origin", "https://different.example")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Code, w.Body.String())
	}
	s.SetSharing(false)
	w = request(s, "", "GET", "/api/new-thread", "")
	if w.Code != http.StatusForbidden || b.calls.Load() != 0 {
		t.Fatal(w.Code, w.Body.String(), b.calls.Load())
	}
}

func TestNewThreadRouteFailureDoesNotExposePartialContext(t *testing.T) {
	s, b := newThreadTestServer(t)
	for _, tc := range []struct {
		failure error
		status  int
		code    string
	}{
		{errors.New("temporarily unavailable"), 502, "backend_unavailable"},
		{session.ErrAttachmentOutdated, 501, "attachment_outdated"},
		{session.ErrNoAttachment, 503, "disconnected"},
	} {
		b.read = func(context.Context) (session.NewThreadContext, error) {
			return session.NewThreadContext{Projects: []session.Project{{ID: "must-not-escape"}}}, tc.failure
		}
		w := request(s, "", "GET", "/api/new-thread", "")
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) ||
			strings.Contains(w.Body.String(), "projects") || strings.Contains(w.Body.String(), "must-not-escape") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestNewThreadRouteSuppressesContextAfterSharingLifetimeChange(t *testing.T) {
	s, b := newThreadTestServer(t)
	started := make(chan context.Context, 1)
	resume := make(chan struct{})
	b.read = func(ctx context.Context) (session.NewThreadContext, error) {
		started <- ctx
		<-resume
		return session.NewThreadContext{Projects: []session.Project{{ID: "stale-project"}}}, nil
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(s, "", "GET", "/api/new-thread", "") }()
	ctx := <-started
	s.SetSharing(false)
	s.SetSharing(true)
	if ctx.Err() == nil {
		t.Error("old sharing lifetime did not cancel the backend context")
	}
	close(resume)
	w := <-done
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "access_changed") || strings.Contains(w.Body.String(), "stale-project") {
		t.Fatal(w.Code, w.Body.String())
	}
}
