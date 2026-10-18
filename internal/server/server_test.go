package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"mirrorbird/internal/session"
)

type fakeBackend struct {
	mu          sync.Mutex
	epochs      map[string]uint64
	nextEpoch   uint64
	readOnly    map[string]bool
	connected   bool
	value       session.Snapshot
	changes     chan session.Change
	sends       atomic.Int32
	interrupts  atomic.Int32
	responses   atomic.Int32
	reads       atomic.Int32
	lists       atomic.Int32
	onConnected func()
	onList      func(context.Context, session.ListOptions) (session.ThreadPage, error)
	onRead      func(context.Context, string, session.ReadOptions) (session.Snapshot, error)
	onSend      func(context.Context) (json.RawMessage, error)
	onRespond   func(json.RawMessage, string)
	onSubscribe func()
}

func boolPointer(value bool) *bool { return &value }

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		epochs: map[string]uint64{"thread": 1}, nextEpoch: 1, readOnly: map[string]bool{}, connected: true,
		value: session.Snapshot{Thread: session.Thread{
			ID: "thread", Name: "Thread", CanAcceptDirectInput: boolPointer(true), Turns: []session.Turn{},
		}, Revision: 7, Pending: []session.PendingRequest{}},
		changes: make(chan session.Change, 8),
	}
}

func (b *fakeBackend) Connected() bool {
	if b.onConnected != nil {
		b.onConnected()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected
}
func (b *fakeBackend) ListThreads(ctx context.Context, options session.ListOptions) (session.ThreadPage, error) {
	b.lists.Add(1)
	if b.onList != nil {
		return b.onList(ctx, options)
	}
	return session.ThreadPage{Threads: []session.Summary{{ID: "thread", Name: "Thread"}, {ID: "unloaded", Name: "History"}}}, nil
}
func (b *fakeBackend) ProjectionGeneration(id string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.epochs[id]
}
func (b *fakeBackend) setEpoch(id string, epoch uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.epochs[id] = epoch
	if epoch > b.nextEpoch {
		b.nextEpoch = epoch
	}
}
func (b *fakeBackend) Read(ctx context.Context, id string, options session.ReadOptions) (session.Snapshot, error) {
	b.reads.Add(1)
	if b.onRead != nil {
		return b.onRead(ctx, id, options)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return session.Snapshot{}, err
	}
	if !b.connected {
		return session.Snapshot{}, session.ErrNoAttachment
	}
	if id == "missing" {
		return session.Snapshot{}, session.ErrInvalidID
	}
	if b.epochs[id] == 0 || options.Resync || b.readOnly[id] != options.Archived {
		b.nextEpoch++
		b.epochs[id] = b.nextEpoch
	}
	b.readOnly[id] = options.Archived
	value := b.value
	value.Thread.ID = id
	value.ReadOnly = options.Archived
	if options.Archived {
		value.Thread.CanAcceptDirectInput = boolPointer(false)
	}
	return value, nil
}
func (b *fakeBackend) Send(ctx context.Context, id, _, _, _ string) (json.RawMessage, error) {
	b.sends.Add(1)
	if b.onSend != nil {
		return b.onSend(ctx)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.readOnly[id] {
		return nil, session.ErrStateChanged
	}
	return json.RawMessage(`{"secretInternalField":"must not reach browser"}`), nil
}
func (b *fakeBackend) Interrupt(context.Context, string, string) (json.RawMessage, error) {
	b.interrupts.Add(1)
	return json.RawMessage(`{}`), nil
}
func (b *fakeBackend) Respond(_ context.Context, _ string, id json.RawMessage, decision string) (json.RawMessage, error) {
	b.responses.Add(1)
	if b.onRespond != nil {
		b.onRespond(id, decision)
	}
	return json.RawMessage(`{}`), nil
}
func (b *fakeBackend) Subscribe() (<-chan session.Change, func()) {
	if b.onSubscribe != nil {
		b.onSubscribe()
	}
	return b.changes, func() {}
}

func testServer(t *testing.T) (*Server, *fakeBackend, string) {
	t.Helper()
	backend := newFakeBackend()
	s := New(backend, fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>MirrorBird</title>")}})
	s.SetSharing(true)
	t.Cleanup(func() { s.SetSharing(false) })
	return s, backend, requestCSRF(t, s)
}

func newRequest(s *Server, csrf, method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "localhost:43123"
	s.mu.Lock()
	if s.origin != "" {
		r.Host = strings.TrimPrefix(s.origin, "https://")
	}
	s.mu.Unlock()
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if csrf != "" {
		r.Header.Set("X-MirrorBird-CSRF", csrf)
	}
	return r
}

func request(s *Server, csrf, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newRequest(s, csrf, method, path, body))
	return w
}

func requestCSRF(t *testing.T, s *Server) string {
	t.Helper()
	w := request(s, "", "GET", "/api/connection", "")
	var state struct {
		Sharing bool   `json:"sharing"`
		CSRF    string `json:"csrf"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &state) != nil || !state.Sharing || state.CSRF == "" || len(w.Result().Cookies()) != 0 {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
	return state.CSRF
}

const messageBody = `{"text":"hello","mode":"start","requestId":"1234567890123456"}`

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not complete")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestConnectionDirectBootStableCSRFAndNoCookies(t *testing.T) {
	s, _, csrf := testServer(t)
	for range 4 {
		s.SetSharing(true)
		if got := requestCSRF(t, s); got != csrf {
			t.Fatal("ordinary boot or repeated enable rotated the proof")
		}
	}
	w := request(s, "", "GET", "/api/connection", "")
	for _, forbidden := range []string{"authenticated", "device", "expiresAt", "pair"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatal("connection contains removed identity state", w.Body.String())
		}
	}
	if !strings.Contains(w.Body.String(), `"connected":true`) || w.Header().Get("Set-Cookie") != "" {
		t.Fatal(w.Body.String(), w.Header())
	}
	w = request(s, "", "GET", "/api/threads", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"unloaded"`) || strings.Contains(w.Body.String(), `"shared"`) || w.Header().Get("Set-Cookie") != "" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, route := range []struct{ method, path string }{{"POST", "/api/pair"}, {"GET", "/api/session"}, {"POST", "/api/revoke"}} {
		if got := request(s, "", route.method, route.path, `{}`); got.Code != 404 || got.Header().Get("Set-Cookie") != "" {
			t.Fatal("removed route is still active", route, got.Code)
		}
	}
	s.SetSharing(false)
	w = request(s, "", "GET", "/api/connection", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sharing":false`) || strings.Contains(w.Body.String(), "csrf") || s.Sharing() {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := request(s, csrf, "POST", "/api/threads/thread/messages", messageBody); got.Code != 403 {
		t.Fatal(got.Code)
	}
	s.SetSharing(true)
	fresh := requestCSRF(t, s)
	if fresh == csrf {
		t.Fatal("Stop/Start reused a proof")
	}
	if got := request(s, csrf, "POST", "/api/threads/thread/messages", messageBody); got.Code != 403 || !strings.Contains(got.Body.String(), "csrf_failed") {
		t.Fatal(got.Code, got.Body.String())
	}
	if got := request(s, fresh, "POST", "/api/threads/thread/messages", messageBody); got.Code != 202 {
		t.Fatal(got.Code, got.Body.String())
	}
}

func TestVerifiedOriginLifetimeAndStoppedHostRestriction(t *testing.T) {
	s, _, first := testServer(t)
	const origin = "https://fixture-43123.use.devtunnels.ms"
	if err := s.EnableSharing(origin); err != nil {
		t.Fatal(err)
	}
	proof := requestCSRF(t, s)
	if proof == first {
		t.Fatal("origin change reused proof")
	}
	if err := s.EnableSharing(origin + ":443/"); err != nil || requestCSRF(t, s) != proof {
		t.Fatal("equivalent origin changed the lifetime", err)
	}
	s.SetSharing(false)
	r := httptest.NewRequest("GET", "http://127.0.0.1:43123/api/connection", nil)
	r.Header.Set("X-Forwarded-Host", "fixture-43123.use.devtunnels.ms")
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "invalid_host") {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := s.EnableSharing(origin); err != nil || requestCSRF(t, s) == proof {
		t.Fatal("verified Stop/Start reused proof", err)
	}
	for _, invalid := range []string{
		"http://localhost", "https://example.test/path", "https://u:p@example.test", "https://example.test?token=x",
		"https://example.test?", "https://example.test#fragment", "https://example.test:444", "https://example.test:", "https://example.test\\attacker",
	} {
		if s.EnableSharing(invalid) == nil {
			t.Fatalf("accepted invalid origin %q", invalid)
		}
	}
}

func TestRequestOriginAndFetchMetadataProtectReadsProofAndMutations(t *testing.T) {
	s, b, _ := testServer(t)
	const origin = "https://fixture-43123.use.devtunnels.ms"
	if err := s.EnableSharing(origin); err != nil {
		t.Fatal(err)
	}
	csrf := requestCSRF(t, s)
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/connection"}, {"GET", "/api/threads"}, {"GET", "/api/threads/thread"},
		{"GET", "/api/events"}, {"POST", "/api/threads/thread/messages"},
	} {
		for _, headers := range []map[string]string{
			{"Origin": "https://attacker.example"}, {"Origin": "null"}, {"Origin": ""},
			{"Origin": origin, "Sec-Fetch-Site": "cross-site"}, {"Sec-Fetch-Site": "same-site"},
			{"Sec-Fetch-Site": "unrecognized"}, {"Origin": origin + "/"},
		} {
			r := newRequest(s, csrf, route.method, route.path, messageBody)
			for key, value := range headers {
				r.Header.Set(key, value)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 403 || strings.Contains(w.Body.String(), csrf) || w.Header().Get("Set-Cookie") != "" {
				t.Fatal(route, headers, w.Code, w.Body.String())
			}
		}
	}
	for _, header := range []string{"Origin", "Sec-Fetch-Site", "X-MirrorBird-CSRF"} {
		r := newRequest(s, csrf, "POST", "/api/threads/thread/messages", messageBody)
		r.Header.Del(header)
		r.Header.Add(header, "same-origin")
		r.Header.Add(header, "same-origin")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(header, w.Code)
		}
	}
	for _, site := range []string{"", "same-origin", "none"} {
		r := newRequest(s, "", "GET", "/api/connection", "")
		r.Header.Set("Origin", origin)
		r.Header.Set("Sec-Fetch-Site", site)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(site, w.Code, w.Body.String())
		}
	}
	if b.reads.Load() != 0 || b.lists.Load() != 0 || b.sends.Load() != 0 {
		t.Fatal("cross-origin request reached backend")
	}
}

func TestHostCannotBeOverriddenByForwardingHeaders(t *testing.T) {
	for _, production := range []bool{false, true} {
		s, _, _ := testServer(t)
		if production {
			if err := s.EnableSharing("https://fixture-43123.use.devtunnels.ms"); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range []string{"/api/connection", "/api/threads", "/", "/healthz"} {
			r := httptest.NewRequest("GET", "http://attacker.example:43123"+path, nil)
			r.Header.Set("X-Forwarded-Host", "fixture-43123.use.devtunnels.ms")
			r.Header.Set("X-Forwarded-Proto", "https")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatal(production, path, w.Code)
			}
		}
	}
}

func TestConnectionProofIsWithheldIfLifetimeChangesDuringResponse(t *testing.T) {
	s, b, csrf := testServer(t)
	b.onConnected = func() {
		if err := s.EnableSharing("https://replacement.use.devtunnels.ms"); err != nil {
			t.Error(err)
		}
	}
	w := request(s, "", "GET", "/api/connection", "")
	if w.Code != 403 || strings.Contains(w.Body.String(), csrf) || strings.Contains(w.Body.String(), "csrf") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestInterruptRequiresExplicitTurn(t *testing.T) {
	s, b, csrf := testServer(t)
	w := request(s, csrf, "POST", "/api/threads/thread/interrupt", `{"turnId":"active","requestId":"1234567890123456"}`)
	if w.Code != 202 || b.interrupts.Load() != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(s, csrf, "POST", "/api/threads/thread/interrupt", `{"turnId":"","requestId":"abcdefghijklmnop"}`)
	if w.Code != 400 || b.interrupts.Load() != 1 {
		t.Fatal(w.Code, b.interrupts.Load())
	}
}

func TestRequestCSRFAndInputLimits(t *testing.T) {
	s, b, csrf := testServer(t)
	for _, proof := range []string{"", "wrong"} {
		if got := request(s, proof, "POST", "/api/threads/thread/messages", messageBody); got.Code != 403 {
			t.Fatal(got.Code)
		}
	}
	if got := request(s, csrf, "POST", "/api/threads/unloaded/messages", messageBody); got.Code != 409 || !strings.Contains(got.Body.String(), "state_changed") {
		t.Fatal(got.Code, got.Body.String())
	}
	for _, invalid := range []string{
		`{"text":"","mode":"start","requestId":"1234567890123456"}`,
		`{"text":"hello","mode":"steer","requestId":"1234567890123456"}`,
		`{"text":"hello","mode":"start","expectedTurnId":"active","requestId":"1234567890123456"}`,
		`{"text":"hello","mode":"start","requestId":"short"}`,
		`{"text":"hello","mode":"start","requestId":"1234567890123456","unknown":true}`,
		messageBody + `{}`,
		`{"text":"` + strings.Repeat("x", 70*1024) + `","mode":"start","requestId":"1234567890123456"}`,
	} {
		if got := request(s, csrf, "POST", "/api/threads/thread/messages", invalid); got.Code != 400 {
			t.Fatal(got.Code, got.Body.String())
		}
	}
	r := newRequest(s, csrf, "POST", "/api/threads/thread/messages", messageBody)
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 415 || b.sends.Load() != 0 {
		t.Fatal("invalid request reached backend", w.Code)
	}
}

func TestCatalogueQueryIsBoundedAndPassedThrough(t *testing.T) {
	s, b, _ := testServer(t)
	b.onList = func(ctx context.Context, options session.ListOptions) (session.ThreadPage, error) {
		if !s.Sharing() || ctx.Err() != nil { // The server must not hold its lock across backend I/O.
			t.Error("invalid catalogue context")
		}
		if options != (session.ListOptions{Cursor: "opaque", Search: "other project", Limit: 17}) {
			t.Error(options)
		}
		return session.ThreadPage{Threads: []session.Summary{{ID: "history"}}, NextCursor: "next-opaque"}, nil
	}
	w := request(s, "", "GET", "/api/threads?cursor=opaque&search=other+project&limit=17", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"nextCursor":"next-opaque"`) || !strings.Contains(w.Body.String(), `"connected":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, query := range []string{
		"limit=0", "limit=101", "limit=-1", "limit=no", "limit=", "limit=1&limit=2", "archived=true", "archived=false", "archived=1", "archived=", "archived=yes",
		"search=a&search=b", "unknown=value", "search=%ZZ", "search=%FF", "cursor=%00", "cursor=%0A",
		"search=" + strings.Repeat("x", maxSearchBytes+1), "cursor=" + strings.Repeat("x", maxCursorBytes+1),
		"search=" + strings.Repeat("x", maxQueryBytes+1),
	} {
		if got := request(s, "", "GET", "/api/threads?"+query, ""); got.Code != 400 {
			t.Fatal(query[:min(len(query), 80)], got.Code, got.Body.String())
		}
	}
	if b.lists.Load() != 1 || b.reads.Load() != 0 {
		t.Fatal("invalid query or catalogue request initialized a projection", b.lists.Load(), b.reads.Load())
	}
	b.onList = func(_ context.Context, options session.ListOptions) (session.ThreadPage, error) {
		if options != (session.ListOptions{Limit: 50}) {
			t.Error("unexpected default query", options)
		}
		return session.ThreadPage{}, nil
	}
	w = request(s, "", "GET", "/api/threads", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"threads":[]`) || strings.Contains(w.Body.String(), "nextCursor") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCataloguePaginationHasNoVisibilityCapOrProjectionWhitelist(t *testing.T) {
	s, b, _ := testServer(t)
	const total = 237
	b.onList = func(_ context.Context, options session.ListOptions) (session.ThreadPage, error) {
		start, _ := strconv.Atoi(options.Cursor)
		end := min(start+options.Limit, total)
		page := session.ThreadPage{Threads: []session.Summary{}}
		for i := start; i < end; i++ {
			page.Threads = append(page.Threads, session.Summary{ID: fmt.Sprintf("history-%d", i)})
		}
		if end < total {
			page.NextCursor = strconv.Itoa(end)
		}
		return page, nil
	}
	count, cursor := 0, ""
	for {
		w := request(s, "", "GET", "/api/threads?limit=50&cursor="+url.QueryEscape(cursor), "")
		var page session.ThreadPage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		count += len(page.Threads)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if count != total || b.reads.Load() != 0 || len(b.epochs) != 1 {
		t.Fatal(count, b.reads.Load(), b.epochs)
	}
	w := request(s, "", "GET", "/api/threads/not-in-any-page", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"not-in-any-page"`) || b.reads.Load() != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestOversizedBackendPageIsRejectedNotSilentlyTruncated(t *testing.T) {
	s, b, _ := testServer(t)
	b.onList = func(context.Context, session.ListOptions) (session.ThreadPage, error) {
		return session.ThreadPage{Threads: make([]session.Summary, 51)}, nil
	}
	w := request(s, "", "GET", "/api/threads", "")
	if w.Code != 502 || strings.Contains(w.Body.String(), `"threads"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSnapshotLazyReadAndExplicitResyncOptions(t *testing.T) {
	s, b, csrf := testServer(t)
	if b.ProjectionGeneration("off-page") != 0 {
		t.Fatal("projection already loaded")
	}
	w := request(s, "", "GET", "/api/threads/off-page", "")
	if w.Code != 200 || b.ProjectionGeneration("off-page") == 0 || b.lists.Load() != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	before := b.ProjectionGeneration("off-page")
	w = request(s, "", "GET", "/api/threads/off-page?resync=false", "")
	if w.Code != 200 || b.ProjectionGeneration("off-page") != before {
		t.Fatal("ordinary read replaced the projection")
	}
	w = request(s, "", "GET", "/api/threads/off-page?resync=true", "")
	if w.Code != 200 || b.ProjectionGeneration("off-page") == before {
		t.Fatal("explicit resync did not reach backend")
	}
	if got := request(s, csrf, "POST", "/api/threads/off-page/messages", messageBody); got.Code != 202 {
		t.Fatal(got.Code, got.Body.String())
	}
	b.setEpoch("off-page", 0)
	if got := request(s, csrf, "POST", "/api/threads/off-page/messages", messageBody); got.Code != 409 || !strings.Contains(got.Body.String(), "state_changed") {
		t.Fatal(got.Code, got.Body.String())
	}
	reads := b.reads.Load()
	for _, query := range []string{"resync", "resync=1", "resync=yes", "resync=true&resync=false", "archived=", "limit=1"} {
		if got := request(s, "", "GET", "/api/threads/thread?"+query, ""); got.Code != 400 {
			t.Fatal(query, got.Code)
		}
	}
	if b.reads.Load() != reads {
		t.Fatal("invalid snapshot query reached backend")
	}
}

func TestArchivedBrowsingIsNotExposed(t *testing.T) {
	s, b, _ := testServer(t)
	w := request(s, "", "GET", "/api/threads/history?archived=true&resync=true", "")
	if w.Code != 400 || b.reads.Load() != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(s, "", "GET", "/api/threads?archived=true", "")
	if w.Code != 400 || b.lists.Load() != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestLazySnapshotHasLongerDeadlineThanCatalogue(t *testing.T) {
	s, b, _ := testServer(t)
	checkDeadline := func(ctx context.Context, budget time.Duration) {
		t.Helper()
		deadline, ok := ctx.Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining < budget-time.Second || remaining > budget {
			t.Errorf("backend deadline remaining = %v, want about %v", remaining, budget)
		}
	}
	b.onRead = func(ctx context.Context, id string, _ session.ReadOptions) (session.Snapshot, error) {
		checkDeadline(ctx, 45*time.Second)
		return session.Snapshot{Thread: session.Thread{ID: id}}, nil
	}
	b.onList = func(ctx context.Context, _ session.ListOptions) (session.ThreadPage, error) {
		checkDeadline(ctx, 15*time.Second)
		return session.ThreadPage{}, nil
	}
	for _, path := range []string{"/api/threads", "/api/threads/dormant", "/api/threads/dormant?resync=true"} {
		if w := request(s, "", "GET", path, ""); w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
}

func TestStopCancelsBackendReadAndCatalogueOutsideServerLock(t *testing.T) {
	for _, mode := range []string{"snapshot", "catalogue"} {
		t.Run(mode, func(t *testing.T) {
			s, b, _ := testServer(t)
			started := make(chan context.Context, 1)
			wait := func(ctx context.Context) {
				_ = s.Sharing()
				started <- ctx
				<-ctx.Done()
			}
			b.onRead = func(ctx context.Context, _ string, _ session.ReadOptions) (session.Snapshot, error) {
				wait(ctx)
				return session.Snapshot{Thread: session.Thread{Name: "late-secret"}}, nil
			}
			b.onList = func(ctx context.Context, _ session.ListOptions) (session.ThreadPage, error) {
				wait(ctx)
				return session.ThreadPage{Threads: []session.Summary{{Name: "late-secret"}}}, nil
			}
			path := "/api/threads"
			if mode == "snapshot" {
				path += "/off-page"
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- request(s, "", "GET", path, "") }()
			ctx := <-started
			s.SetSharing(false)
			if ctx.Err() == nil {
				t.Fatal("Stop did not synchronously cancel backend context")
			}
			s.SetSharing(true)
			select {
			case w := <-done:
				if w.Code != 403 || strings.Contains(w.Body.String(), "late-secret") {
					t.Fatal(w.Code, w.Body.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("read was not cancelled by Stop")
			}
		})
	}
}

func TestOperationDeduplicationAndPayloadBindingAreServiceWide(t *testing.T) {
	s, b, csrf := testServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	b.onSend = func(context.Context) (json.RawMessage, error) {
		close(started)
		<-release
		return json.RawMessage(`{"internal":"private"}`), nil
	}
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- request(s, csrf, "POST", "/api/threads/thread/messages", messageBody) }()
	<-started
	// Another visitor obtains the same lifetime proof without registration.
	otherProof := requestCSRF(t, s)
	go func() { results <- request(s, otherProof, "POST", "/api/threads/thread/messages", messageBody) }()
	conflict := request(s, csrf, "POST", "/api/threads/thread/messages", strings.Replace(messageBody, "hello", "different", 1))
	if conflict.Code != 409 {
		t.Fatal(conflict.Code, conflict.Body.String())
	}
	close(release)
	for range 2 {
		select {
		case result := <-results:
			if result.Code != 202 || strings.Contains(result.Body.String(), "private") || result.Header().Get("Set-Cookie") != "" {
				t.Fatal(result.Code, result.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("duplicate operation did not finish")
		}
	}
	if b.sends.Load() != 1 {
		t.Fatal("duplicate mutation", b.sends.Load())
	}
	// The route is part of the binding, so an explicit interrupt is independent.
	w := request(s, csrf, "POST", "/api/threads/thread/interrupt", `{"turnId":"active","requestId":"1234567890123456"}`)
	if w.Code != 202 || b.interrupts.Load() != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestDuplicateResponseIsWithheldAfterStop(t *testing.T) {
	s, b, csrf := testServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	b.onSend = func(context.Context) (json.RawMessage, error) {
		close(started)
		<-release
		return nil, nil
	}
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- request(s, csrf, "POST", "/api/threads/thread/messages", messageBody) }()
	<-started
	go func() { results <- request(s, csrf, "POST", "/api/threads/thread/messages", messageBody) }()
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, op := range s.requests {
			if op.waiters == 1 {
				return true
			}
		}
		return false
	})
	s.SetSharing(false)
	close(release)
	for range 2 {
		select {
		case result := <-results:
			if result.Code != 403 || !strings.Contains(result.Body.String(), "operation_failed_or_outcome_unknown") {
				t.Fatal(result.Code, result.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("stopped operation did not finish")
		}
	}
	if b.sends.Load() != 1 {
		t.Fatal(b.sends.Load())
	}
}

func TestLostResponseDoesNotCancelAdmittedOperation(t *testing.T) {
	s, b, csrf := testServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	b.onSend = func(ctx context.Context) (json.RawMessage, error) {
		close(started)
		<-release
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := newRequest(s, csrf, "POST", "/api/threads/thread/messages", messageBody).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.Handler().ServeHTTP(w, r); close(done) }()
	<-started
	cancel()
	close(release)
	<-done
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	if duplicate := request(s, csrf, "POST", "/api/threads/thread/messages", messageBody); duplicate.Code != 202 || b.sends.Load() != 1 {
		t.Fatal("lost browser response caused replay", duplicate.Code, b.sends.Load())
	}
}

type callbackBody struct {
	io.Reader
	onRead func()
	once   sync.Once
}

func (b *callbackBody) Read(p []byte) (int, error) { b.once.Do(b.onRead); return b.Reader.Read(p) }
func (*callbackBody) Close() error                 { return nil }

func TestLifetimeChangeDuringDecodeCannotAdmitMutation(t *testing.T) {
	for _, mode := range []string{"sharing", "origin", "projection", "evicted"} {
		t.Run(mode, func(t *testing.T) {
			s, b, csrf := testServer(t)
			r := newRequest(s, csrf, "POST", "/api/threads/thread/messages", "")
			r.Body = &callbackBody{Reader: strings.NewReader(messageBody), onRead: func() {
				switch mode {
				case "sharing":
					s.SetSharing(false)
					s.SetSharing(true)
				case "origin":
					_ = s.EnableSharing("https://replacement.use.devtunnels.ms")
				case "projection":
					b.setEpoch("thread", 2)
				case "evicted":
					b.setEpoch("thread", 0)
				}
			}}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			want := 403
			if mode == "projection" || mode == "evicted" {
				want = 409
			}
			if w.Code != want || b.sends.Load() != 0 || strings.Contains(w.Body.String(), "outcome_unknown") {
				t.Fatal(w.Code, w.Body.String(), b.sends.Load())
			}
		})
	}
}

func TestBackendErrorsExposeOnlyStableSafeOutcomes(t *testing.T) {
	for _, test := range []struct {
		err      error
		mutation bool
		status   int
		code     string
	}{
		{errors.Join(session.ErrOutcomeUnknown, session.ErrStateChanged), true, 502, "operation_failed_or_outcome_unknown"},
		{session.ErrInvalidID, false, 400, "invalid_input"},
		{session.ErrInvalidListOptions, false, 400, "invalid_input"},
		{session.ErrProjectionCapacity, false, 429, "capacity_limited"},
		{session.ErrNoAttachment, false, 503, "disconnected"},
		{session.ErrStateChanged, true, 409, "state_changed"},
		{session.ErrResyncRequired, true, 409, "resync_required"},
		{context.DeadlineExceeded, false, 504, "timeout"},
		{context.DeadlineExceeded, true, 502, "operation_failed_or_outcome_unknown"},
		{errors.New("private backend details"), false, 502, "backend_unavailable"},
	} {
		status, message, code := backendError(test.err, test.mutation)
		if status != test.status || code != test.code || strings.Contains(message, "private backend details") {
			t.Fatal(status, message, code)
		}
	}
	if got := PortAddress(43123); got != "127.0.0.1:43123" {
		t.Fatal(got)
	}
}

func TestUnknownOutcomeIsRetainedAndInternalErrorsAreRedacted(t *testing.T) {
	s, b, csrf := testServer(t)
	b.onSend = func(context.Context) (json.RawMessage, error) {
		return nil, errors.New("capability=secret-local-value")
	}
	for range 2 {
		w := request(s, csrf, "POST", "/api/threads/thread/messages", messageBody)
		if w.Code != 502 || !strings.Contains(w.Body.String(), "operation_failed_or_outcome_unknown") || strings.Contains(w.Body.String(), "secret-local-value") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if b.sends.Load() != 1 {
		t.Fatal("unknown outcome was replayed")
	}
}

func TestApprovalIDRoundTripPreservesJSONTypeAndPrecision(t *testing.T) {
	for _, raw := range []string{`9007199254740993`, `"9007199254740993"`, `"approval-3"`} {
		t.Run(raw, func(t *testing.T) {
			s, b, csrf := testServer(t)
			b.value.Pending = []session.PendingRequest{{ID: json.RawMessage(raw), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread","command":"go test ./...","internalCredential":"never-forward"}`)}}
			w := request(s, "", "GET", "/api/threads/thread", "")
			var snap struct {
				Pending []struct {
					ID string `json:"id"`
				} `json:"pending"`
			}
			if json.Unmarshal(w.Body.Bytes(), &snap) != nil || len(snap.Pending) != 1 || strings.Contains(w.Body.String(), "never-forward") {
				t.Fatal(w.Body.String())
			}
			b.onRespond = func(id json.RawMessage, decision string) {
				if string(id) != raw || decision != "accept" {
					t.Errorf("ID changed: %s", id)
				}
			}
			body, _ := json.Marshal(map[string]string{"id": snap.Pending[0].ID, "decision": "accept", "requestId": "1234567890123456"})
			w = request(s, csrf, "POST", "/api/threads/thread/respond", string(body))
			if w.Code != 202 || b.responses.Load() != 1 {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	for _, raw := range []string{`null`, `true`, `{}`, `[1]`, `1 2`, ""} {
		if _, err := decodeApprovalID(base64.RawURLEncoding.EncodeToString([]byte(raw))); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}

func TestStopCancelsAdmittedWaitWithoutReplaying(t *testing.T) {
	s, b, csrf := testServer(t)
	started, done := make(chan context.Context, 1), make(chan *httptest.ResponseRecorder, 1)
	b.onSend = func(ctx context.Context) (json.RawMessage, error) {
		started <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() { done <- request(s, csrf, "POST", "/api/threads/thread/messages", messageBody) }()
	ctx := <-started
	s.SetSharing(false)
	if ctx.Err() == nil {
		t.Fatal("Stop did not synchronously cancel admitted context")
	}
	select {
	case w := <-done:
		if w.Code != 403 || b.sends.Load() != 1 || !strings.Contains(w.Body.String(), "operation_failed_or_outcome_unknown") {
			t.Fatal(w.Code, b.sends.Load(), w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not cancel admitted backend wait")
	}
}

func TestSuccessfulMutationWithLateLifetimeLossIsUnknownNotRejected(t *testing.T) {
	for _, scope := range []string{"projection", "evicted", "sharing", "origin"} {
		t.Run(scope, func(t *testing.T) {
			s, b, csrf := testServer(t)
			b.onSend = func(context.Context) (json.RawMessage, error) {
				switch scope {
				case "projection":
					b.setEpoch("thread", 2)
				case "evicted":
					b.setEpoch("thread", 0)
				case "sharing":
					s.SetSharing(false)
				case "origin":
					_ = s.EnableSharing("https://replacement.use.devtunnels.ms")
				}
				return json.RawMessage(`{"executed":true}`), nil
			}
			w := request(s, csrf, "POST", "/api/threads/thread/messages", messageBody)
			if w.Code != 403 || !strings.Contains(w.Body.String(), "operation_failed_or_outcome_unknown") || strings.Contains(w.Body.String(), `"code":"access_changed"`) || b.sends.Load() != 1 {
				t.Fatal(w.Code, w.Body.String(), b.sends.Load())
			}
		})
	}
}

func TestDeduplicationConflictsAcrossProjectionLifetime(t *testing.T) {
	s, b, csrf := testServer(t)
	if w := request(s, csrf, "POST", "/api/threads/thread/messages", messageBody); w.Code != 202 {
		t.Fatal(w.Code)
	}
	b.setEpoch("thread", 2)
	w := request(s, csrf, "POST", "/api/threads/thread/messages", messageBody)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "request_id_reused") || b.sends.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), b.sends.Load())
	}
	s.SetSharing(false)
	s.SetSharing(true)
	w = request(s, requestCSRF(t, s), "POST", "/api/threads/thread/messages", messageBody)
	if w.Code != 202 || b.sends.Load() != 2 {
		t.Fatal("dedup key did not include the service lifetime", w.Code, b.sends.Load())
	}
}

func TestDuplicateOperationWaitersAreBounded(t *testing.T) {
	s, b, csrf := testServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	b.onSend = func(context.Context) (json.RawMessage, error) {
		close(started)
		<-release
		return nil, nil
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(s, csrf, "POST", "/api/threads/thread/messages", messageBody) }()
	<-started
	s.mu.Lock()
	for _, op := range s.requests {
		op.waiters = maxOperationWaiters
	}
	s.mu.Unlock()
	w := request(s, csrf, "POST", "/api/threads/thread/messages", messageBody)
	close(release)
	<-done
	if w.Code != 429 || b.sends.Load() != 1 {
		t.Fatal(w.Code, b.sends.Load())
	}
}

func TestOutstandingOperationsRemainCountedAcrossStopStart(t *testing.T) {
	s, b, csrf := testServer(t)
	started, release := make(chan struct{}, maxActiveOperations), make(chan struct{})
	b.onSend = func(context.Context) (json.RawMessage, error) {
		started <- struct{}{}
		<-release // Simulate already-transmitted RPCs that ignore cancellation.
		return nil, nil
	}
	results := make(chan *httptest.ResponseRecorder, maxActiveOperations)
	for i := range maxActiveOperations {
		body := strings.Replace(messageBody, "1234567890123456", fmt.Sprintf("operation-%016d", i), 1)
		go func() { results <- request(s, csrf, "POST", "/api/threads/thread/messages", body) }()
	}
	for range maxActiveOperations {
		<-started
	}
	s.SetSharing(false)
	s.SetSharing(true)
	fresh := requestCSRF(t, s)
	w := request(s, fresh, "POST", "/api/threads/thread/messages", messageBody)
	if w.Code != 429 || b.sends.Load() != maxActiveOperations {
		t.Fatal("old commands lost their concurrency slots", w.Code, b.sends.Load())
	}
	close(release)
	for range maxActiveOperations {
		if result := <-results; result.Code != 403 || !strings.Contains(result.Body.String(), "outcome_unknown") {
			t.Fatal(result.Code, result.Body.String())
		}
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active != 0 {
		t.Fatal("late command cleanup corrupted outstanding count", active)
	}
	w = request(s, fresh, "POST", "/api/threads/thread/messages", messageBody)
	if w.Code != 202 || b.sends.Load() != maxActiveOperations+1 {
		t.Fatal(w.Code, b.sends.Load())
	}
}

func TestRetainedOperationLimitAndExpiry(t *testing.T) {
	s, _, csrf := testServer(t)
	done := make(chan struct{})
	close(done)
	s.mu.Lock()
	for i := range maxOperations {
		s.requests[strconv.Itoa(i)] = &operation{done: done, expires: time.Now().Add(operationRetention)}
	}
	s.mu.Unlock()
	w := request(s, csrf, "POST", "/api/threads/thread/messages", messageBody)
	if w.Code != 429 {
		t.Fatal(w.Code, w.Body.String())
	}
	s.mu.Lock()
	s.requests["0"].expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	w = request(s, csrf, "POST", "/api/threads/thread/messages", messageBody)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != maxOperations || s.active != 0 {
		t.Fatal(len(s.requests), s.active)
	}
	for key, op := range s.requests {
		if strings.Contains(key, "\x00/api/") && time.Until(op.expires) < operationRetention-time.Second {
			t.Fatal("retention was not anchored to completion")
		}
	}
}

func TestResyncRequiredRejectsMutationWithoutAutomaticRetry(t *testing.T) {
	s, b, csrf := testServer(t)
	b.onSend = func(context.Context) (json.RawMessage, error) { return nil, session.ErrResyncRequired }
	w := request(s, csrf, "POST", "/api/threads/thread/messages", messageBody)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "resync_required") || b.sends.Load() != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestRateLimitIsGlobalAndSurvivesStopStart(t *testing.T) {
	s, b, csrf := testServer(t)
	for i := 0; i < operationsPerMinute+1; i++ {
		body := strings.Replace(messageBody, "1234567890123456", fmt.Sprintf("operation-%016d", i), 1)
		w := request(s, csrf, "POST", "/api/threads/thread/messages", body)
		want := 202
		if i == operationsPerMinute {
			want = 429
		}
		if w.Code != want {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	s.SetSharing(false)
	s.SetSharing(true)
	fresh := requestCSRF(t, s)
	if w := request(s, fresh, "POST", "/api/threads/thread/messages", messageBody); w.Code != 429 {
		t.Fatal("Stop/Start replenished the global rate budget", w.Code)
	}
	s.mu.Lock()
	s.rate.start = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	if w := request(s, fresh, "POST", "/api/threads/thread/messages", messageBody); w.Code != 202 || b.sends.Load() != operationsPerMinute+1 {
		t.Fatal(w.Code, b.sends.Load())
	}
}

func TestStaticSecurityHeadersAndNoCORS(t *testing.T) {
	s, _, _ := testServer(t)
	w := request(s, "", "GET", "/", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "MirrorBird") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header())
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("unexpected CORS or application cookie")
	}
}

func TestSSEAdmissionDoesNotCrossStopGeneration(t *testing.T) {
	s, b, _ := testServer(t)
	b.onSubscribe = func() { s.SetSharing(false); s.SetSharing(true) }
	w := request(s, "", "GET", "/api/events", "")
	if w.Code != 403 || strings.Contains(w.Body.String(), "event: ready") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestLiveSSEStopsOnServiceStop(t *testing.T) {
	s, _, _ := testServer(t)
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get(httpServer.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
	s.SetSharing(false)
	data, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(data), "event: ready") {
		t.Fatal(err, string(data))
	}
}

func TestSSEPublishesCatalogueAndThreadChangesWithoutMembershipFilter(t *testing.T) {
	s, b, _ := testServer(t)
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get(httpServer.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	readEvent := func() string {
		t.Helper()
		var value strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			value.WriteString(line)
			if line == "\n" {
				return value.String()
			}
		}
	}
	if ready := readEvent(); !strings.Contains(ready, "event: ready") {
		t.Fatal(ready)
	}
	b.changes <- session.Change{CatalogueChanged: true}
	if change := readEvent(); !strings.Contains(change, `"catalogueChanged":true`) || strings.Contains(change, "threadId") {
		t.Fatal(change)
	}
	b.changes <- session.Change{ThreadID: "off-page-history", Revision: 2}
	if change := readEvent(); !strings.Contains(change, "event: change") || !strings.Contains(change, `"threadId":"off-page-history"`) || strings.Contains(change, "contentOnly") {
		t.Fatal(change)
	}
	b.changes <- session.Change{ThreadID: "off-page-history", Revision: 3, ContentOnly: true}
	if change := readEvent(); !strings.Contains(change, `"revision":3`) || !strings.Contains(change, `"contentOnly":true`) {
		t.Fatal(change)
	}
	if b.lists.Load() != 0 || b.reads.Load() != 0 {
		t.Fatal("stream queried catalogue or initialized projection")
	}
	close(b.changes)
	if reset := readEvent(); !strings.Contains(reset, "event: reset") {
		t.Fatal(reset)
	}
	if _, err := reader.ReadString('\n'); err != io.EOF {
		t.Fatal("stream not closed", err)
	}
}

type streamRecorder struct {
	*httptest.ResponseRecorder
	ready chan struct{}
	once  sync.Once
}

func (w *streamRecorder) Flush() {
	w.ResponseRecorder.Flush()
	w.once.Do(func() { close(w.ready) })
}

func TestSSE32GlobalSlotsAndOldGenerationCleanup(t *testing.T) {
	s, b, _ := testServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	var subscriptions atomic.Int32
	b.onSubscribe = func() {
		if subscriptions.Add(1) == 1 {
			close(started)
			<-release
		}
	}
	oldDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { oldDone <- request(s, "", "GET", "/api/events", "") }()
	<-started
	s.SetSharing(false)
	s.SetSharing(true)
	var current []*streamRecorder
	done := make(chan struct{}, maxStreams-1)
	for range maxStreams - 1 {
		w := &streamRecorder{ResponseRecorder: httptest.NewRecorder(), ready: make(chan struct{})}
		current = append(current, w)
		r := newRequest(s, "", "GET", "/api/events", "")
		go func() { s.Handler().ServeHTTP(w, r); done <- struct{}{} }()
		select {
		case <-w.ready:
		case <-time.After(3 * time.Second):
			t.Fatal("new stream did not start")
		}
	}
	if got := request(s, "", "GET", "/api/events", ""); got.Code != 429 {
		t.Fatal("old-generation stream lost its global slot", got.Code)
	}
	close(release)
	if old := <-oldDone; old.Code != 403 || strings.Contains(old.Body.String(), "event: ready") {
		t.Fatal(old.Code, old.Body.String())
	}
	s.mu.Lock()
	streams := s.streams
	s.mu.Unlock()
	if streams != maxStreams-1 {
		t.Fatal("old cleanup erased new stream slots", streams)
	}
	s.SetSharing(false)
	for range current {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("stopped stream did not finish")
		}
	}
	s.mu.Lock()
	streams = s.streams
	s.mu.Unlock()
	if streams != 0 {
		t.Fatal("global stream slots leaked", streams)
	}
}
