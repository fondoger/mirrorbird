package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"mirrorbird/internal/server"
	"mirrorbird/internal/session"
)

func TestSyntheticPreviewRejectsForwardedHostsAndCrossOrigin(t *testing.T) {
	for _, test := range []struct {
		name, host, origin, fetchSite string
		want                          int
	}{
		{name: "localhost", host: "localhost:43124", want: 204},
		{name: "loopback", host: "127.0.0.1:43124", origin: "http://127.0.0.1:43124", want: 204},
		{name: "forwarded tunnel", host: "fixture-43124.use.devtunnels.ms", want: 403},
		{name: "rebinding", host: "attacker.example:43124", want: 403},
		{name: "wrong port", host: "localhost:43123", want: 403},
		{name: "cross origin", host: "localhost:43124", origin: "https://attacker.example", want: 403},
		{name: "origin path", host: "localhost:43124", origin: "http://localhost:43124/path", want: 403},
		{name: "origin userinfo", host: "localhost:43124", origin: "http://attacker@localhost:43124", want: 403},
		{name: "cross site", host: "localhost:43124", fetchSite: "cross-site", want: 403},
		{name: "same site not origin", host: "localhost:43124", fetchSite: "same-site", want: 403},
		{name: "same origin", host: "localhost:43124", fetchSite: "same-origin", want: 204},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://localhost:43124/__preview/reset", nil)
			r.Host = test.host
			if test.origin != "" {
				r.Header.Set("Origin", test.origin)
			}
			r.Header.Set("Sec-Fetch-Site", test.fetchSite)
			w := httptest.NewRecorder()
			localPreview(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), 43124).ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatal(w.Code)
			}
		})
	}
}

func previewFixture(t *testing.T) (http.Handler, *previewBackend) {
	t.Helper()
	b := newPreviewBackend()
	api := server.New(b, fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Synthetic workspace</title>")}})
	api.SetSharing(true)
	t.Cleanup(func() { api.SetSharing(false) })
	return previewHandler(api, b, 43124), b
}

func previewRequest(h http.Handler, csrf, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost:43124"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		r.Header.Set("X-MirrorBird-CSRF", csrf)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func previewCSRF(t *testing.T, h http.Handler) string {
	t.Helper()
	w := previewRequest(h, "", "GET", "/api/connection", "")
	var state struct {
		Sharing   bool   `json:"sharing"`
		Connected bool   `json:"connected"`
		CSRF      string `json:"csrf"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &state) != nil || !state.Sharing || !state.Connected || state.CSRF == "" || len(w.Result().Cookies()) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	return state.CSRF
}

func TestPreviewDirectBootWithoutPairingOrCookies(t *testing.T) {
	h, b := previewFixture(t)
	w := previewRequest(h, "", "GET", "/", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Synthetic workspace") || w.Header().Get("Set-Cookie") != "" {
		t.Fatal(w.Code, w.Body.String())
	}
	csrf := previewCSRF(t, h)
	if other := previewCSRF(t, h); other != csrf {
		t.Fatal("ordinary page boot rotated the lifetime proof")
	}
	w = previewRequest(h, "", "GET", "/api/threads", "")
	var page session.ThreadPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Threads) != 50 || page.NextCursor == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(b.snapshots) != 0 {
		t.Fatal("boot or metadata list eagerly loaded transcripts")
	}
	for _, removed := range []struct{ method, path string }{
		{"GET", "/__preview/pair"}, {"POST", "/__preview/revoke"}, {"POST", "/api/pair"}, {"GET", "/api/session"},
	} {
		w = previewRequest(h, "", removed.method, removed.path, `{}`)
		if w.Code == 200 || w.Code == 204 || w.Code/100 == 3 || w.Header().Get("Set-Cookie") != "" {
			t.Fatal("removed helper is still active", removed, w.Code)
		}
	}
}

func TestPreviewCataloguePagesSearchAndArchiveAreMetadataOnly(t *testing.T) {
	b := newPreviewBackend()
	options := session.ListOptions{Limit: 37}
	seen := map[string]bool{}
	for {
		page, err := b.ListThreads(context.Background(), options)
		if err != nil || len(page.Threads) > options.Limit {
			t.Fatal(err, len(page.Threads))
		}
		for _, item := range page.Threads {
			if item.Archived || seen[item.ID] {
				t.Fatal("duplicate or archived default result", item)
			}
			if item.CanAcceptDirectInput != nil {
				t.Fatal("unloaded capability lost unknown state")
			}
			seen[item.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		options.Cursor = page.NextCursor
	}
	if len(seen) != 180 || len(b.snapshots) != 0 || len(b.generations) != 0 {
		t.Fatal(len(seen), len(b.snapshots), len(b.generations))
	}
	filtered, err := b.ListThreads(context.Background(), session.ListOptions{Search: "ALPHA", Limit: 100})
	if err != nil || len(filtered.Threads) < 50 {
		t.Fatal(err, len(filtered.Threads))
	}
	for _, item := range filtered.Threads {
		if !strings.Contains(item.CWD, "alpha") {
			t.Fatal("search did not filter project metadata", item)
		}
	}
	archive, err := b.ListThreads(context.Background(), session.ListOptions{Archived: true, Limit: 100})
	if err != nil || len(archive.Threads) != 48 {
		t.Fatal(err, len(archive.Threads))
	}
	for _, item := range archive.Threads {
		if !item.Archived || seen[item.ID] {
			t.Fatal("archive is not separate", item)
		}
	}
	if len(b.snapshots) != 0 {
		t.Fatal("search or archive listed full transcripts")
	}
}

func TestPreviewCursorIsBoundToFilterAndInvalidInputRejected(t *testing.T) {
	b := newPreviewBackend()
	page, err := b.ListThreads(context.Background(), session.ListOptions{Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatal(err)
	}
	for _, options := range []session.ListOptions{
		{Cursor: page.NextCursor, Search: "alpha", Limit: 1},
		{Cursor: page.NextCursor, Archived: true, Limit: 1},
		{Cursor: "malformed", Limit: 1}, {Limit: -1}, {Limit: 101},
	} {
		if _, err := b.ListThreads(context.Background(), options); !errors.Is(err, session.ErrInvalidListOptions) {
			t.Fatal(options, err)
		}
	}
}

func TestPreviewLazyReadExplicitResyncAndReadOnlyArchive(t *testing.T) {
	h, b := previewFixture(t)
	csrf := previewCSRF(t, h)
	if b.ProjectionGeneration("history-178") != 0 {
		t.Fatal("history already loaded")
	}
	w := previewRequest(h, "", "GET", "/api/threads/history-178", "")
	var snapshot session.Snapshot
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &snapshot) != nil || snapshot.Thread.ID != "history-178" || snapshot.Thread.CanAcceptDirectInput == nil || !*snapshot.Thread.CanAcceptDirectInput {
		t.Fatal(w.Code, w.Body.String())
	}
	before := b.ProjectionGeneration("history-178")
	w = previewRequest(h, "", "GET", "/api/threads/history-178", "")
	if w.Code != 200 || before == 0 || b.ProjectionGeneration("history-178") != before {
		t.Fatal("normal read rebuilt cached history")
	}
	w = previewRequest(h, "", "GET", "/api/threads/history-178?resync=true", "")
	if w.Code != 200 || b.ProjectionGeneration("history-178") == before {
		t.Fatal("explicit resync did not rebuild cache")
	}
	w = previewRequest(h, "", "GET", "/api/threads/archive-001?archived=true", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"readOnly":true`) || !strings.Contains(w.Body.String(), `"canAcceptDirectInput":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = previewRequest(h, csrf, "POST", "/api/threads/archive-001/messages", `{"text":"no write","mode":"start","requestId":"1234567890123456"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "state_changed") {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := b.Read(context.Background(), "archive-001", session.ReadOptions{}); !errors.Is(err, session.ErrInvalidID) {
		t.Fatal("ordinary read silently unarchived history", err)
	}
	// The synthetic data can exercise repeated selection well beyond 32 watches.
	for i := 1; i <= 64; i++ {
		id := b.catalogue[i+1].ID
		if _, err := b.Read(context.Background(), id, session.ReadOptions{}); err != nil {
			t.Fatal(i, err)
		}
	}
}

func TestPreviewStopResetAndResyncHelpersHaveNoIdentityState(t *testing.T) {
	h, b := previewFixture(t)
	first := previewCSRF(t, h)
	if w := previewRequest(h, "", "POST", "/__preview/resync", ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	w := previewRequest(h, "", "GET", "/api/threads/demo-thread", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"resyncRequired":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = previewRequest(h, "", "GET", "/api/threads/demo-thread?resync=true", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"resyncRequired":true`) || previewCSRF(t, h) != first {
		t.Fatal("snapshot resync changed global lifetime or did not clear stale state", w.Code, w.Body.String())
	}
	before := b.ProjectionGeneration("demo-thread")
	if w := previewRequest(h, "", "POST", "/__preview/stop", ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	w = previewRequest(h, "", "GET", "/api/connection", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sharing":false`) || strings.Contains(w.Body.String(), "csrf") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := previewRequest(h, "", "POST", "/__preview/reset", ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if previewCSRF(t, h) == first || len(b.snapshots) != 0 {
		t.Fatal("reset retained old proof or eager projections")
	}
	if _, err := b.Read(context.Background(), "demo-thread", session.ReadOptions{}); err != nil || b.ProjectionGeneration("demo-thread") <= before {
		t.Fatal("reset reused a projection generation", err)
	}
}

func TestPreviewSyntheticUnknownOutcomeIsNotRetried(t *testing.T) {
	h, b := previewFixture(t)
	csrf := previewCSRF(t, h)
	if w := previewRequest(h, "", "GET", "/api/threads/approval-thread", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := previewRequest(h, "", "POST", "/__preview/unknown", ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	body := `{"text":"synthetic once","mode":"steer","expectedTurnId":"approval-turn","requestId":"1234567890123456"}`
	for range 2 {
		w := previewRequest(h, csrf, "POST", "/api/threads/approval-thread/messages", body)
		if w.Code != 502 || !strings.Contains(w.Body.String(), "operation_failed_or_outcome_unknown") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	value, err := b.Read(context.Background(), "approval-thread", session.ReadOptions{})
	if err != nil || len(value.Thread.Turns[0].Items) != 3 {
		t.Fatal("synthetic command replayed", err, value)
	}
}

func TestPreviewContentAndCatalogueHintsStaySeparate(t *testing.T) {
	b := newPreviewBackend()
	changes, cancel := b.Subscribe()
	defer cancel()
	if _, err := b.Read(context.Background(), "demo-thread", session.ReadOptions{}); err != nil {
		t.Fatal(err)
	}
	structural := <-changes
	if structural.ThreadID != "demo-thread" || structural.ContentOnly || structural.CatalogueChanged {
		t.Fatal(structural)
	}
	b.mu.Lock()
	b.publishLocked("demo-thread", true)
	b.publishCatalogueLocked()
	b.mu.Unlock()
	content, catalogue := <-changes, <-changes
	if !content.ContentOnly || content.CatalogueChanged || content.Revision <= structural.Revision {
		t.Fatal(content)
	}
	if !catalogue.CatalogueChanged || catalogue.ThreadID != "" || catalogue.ContentOnly {
		t.Fatal(catalogue)
	}
}
