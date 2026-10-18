package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"mirrorbird/internal/session"
)

const (
	maxStreams       = 32
	defaultListLimit = 50
	maxListLimit     = 100
	maxQueryBytes    = 16 * 1024
	maxCursorBytes   = 4096
	maxSearchBytes   = 512
)

var errAccessChanged = errors.New("remote service lifetime changed")

type Backend interface {
	Connected() bool
	ListThreads(context.Context, session.ListOptions) (session.ThreadPage, error)
	Read(context.Context, string, session.ReadOptions) (session.Snapshot, error)
	ProjectionGeneration(string) uint64
	Send(context.Context, string, string, string, string) (json.RawMessage, error)
	Interrupt(context.Context, string, string) (json.RawMessage, error)
	Respond(context.Context, string, json.RawMessage, string) (json.RawMessage, error)
	Subscribe() (<-chan session.Change, func())
}

type Server struct {
	Backend    Backend
	assets     http.Handler
	mu         sync.Mutex
	sharing    bool
	origin     string
	csrf       string
	generation uint64
	stop       <-chan struct{}
	lifetime   context.Context
	cancel     context.CancelFunc
	requests   map[string]*operation
	rate       operationRate
	streams    int
	active     int
}

type admission struct {
	generation           uint64
	stop                 <-chan struct{}
	lifetime             context.Context
	threadID             string
	projectionGeneration uint64 // Mutations bind an already-loaded projection.
}

type endpoint func(http.ResponseWriter, *http.Request, admission)

func New(backend Backend, assets fs.FS) *Server {
	lifetime, cancel := context.WithCancel(context.Background())
	return &Server{
		Backend: backend, assets: http.FileServer(http.FS(assets)),
		stop: lifetime.Done(), lifetime: lifetime, cancel: cancel,
		requests: map[string]*operation{},
	}
}

// EnableSharing accepts only the verified forwarding origin supplied by the
// local tunnel controller. The private tunnel authenticates remote visitors;
// Host, Origin and the lifetime CSRF proof are request defenses, not identities.
func (s *Server) EnableSharing(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		strings.ContainsAny(origin, "\\\r\n\t ") {
		return errors.New("a verified HTTPS tunnel origin is required")
	}
	if strings.HasSuffix(u.Host, ":") || (u.Port() != "" && u.Port() != "443") {
		return errors.New("the verified tunnel origin must use the standard HTTPS port")
	}
	s.setSharing(true, "https://"+strings.TrimSuffix(strings.ToLower(u.Host), ":443"))
	return nil
}

// SetSharing enables the isolated loopback test/preview path, or stops access.
// Production starts with EnableSharing. Stopping preserves its Host restriction
// and invalidates the old CSRF proof and every admitted request's stop channel.
func (s *Server) SetSharing(enabled bool) { s.setSharing(enabled, "") }

func (s *Server) setSharing(enabled bool, origin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !enabled {
		origin = s.origin
	}
	if s.sharing == enabled && s.origin == origin {
		return
	}
	s.cancel() // Synchronously cancels all old-generation backend contexts.
	s.lifetime, s.cancel = context.WithCancel(context.Background())
	s.stop = s.lifetime.Done()
	s.generation++
	s.sharing, s.origin, s.csrf = enabled, origin, ""
	if enabled {
		s.csrf = rand.Text()
	}
	// Counts belong to outstanding work, not the new lifetime. Old commands and
	// streams release their slots only when they actually finish. Dedup entries
	// are generation-keyed and bounded; the one global rate budget also survives
	// Stop/Start rather than being replenished by a lifetime transition.
}

func (s *Server) Sharing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sharing
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]string{"service": "mirrorbird"})
	})
	mux.HandleFunc("GET /api/connection", s.connection)
	mux.HandleFunc("GET /api/threads", s.admitted(s.threads, false))
	mux.HandleFunc("GET /api/threads/{id}", s.admitted(s.snapshot, false))
	mux.HandleFunc("POST /api/threads/{id}/messages", s.admitted(s.messages, true))
	mux.HandleFunc("POST /api/threads/{id}/interrupt", s.admitted(s.interrupt, true))
	mux.HandleFunc("POST /api/threads/{id}/respond", s.admitted(s.respond, true))
	mux.HandleFunc("GET /api/events", s.admitted(s.events, false))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if s.checkRequest(w, r, true) {
			failure(w, http.StatusNotFound, "unknown API route", "not_found")
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			failure(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed")
			return
		}
		s.assets.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// API admission checks Host/Origin and captures the lifetime in the same
		// critical section. A preliminary unlocked check must not authorize a
		// request against a different forwarding origin after Stop/Start.
		if !strings.HasPrefix(r.URL.Path, "/api/") && !s.checkRequest(w, r, false) {
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) connection(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if code := s.requestDeniedLocked(r, true); code != "" {
		s.mu.Unlock()
		requestDenied(w, code)
		return
	}
	generation := s.generation
	state := struct {
		Sharing   bool   `json:"sharing"`
		Connected bool   `json:"connected"`
		CSRF      string `json:"csrf,omitempty"`
	}{Sharing: s.sharing, CSRF: s.csrf}
	s.mu.Unlock()
	state.Connected = s.Backend.Connected()
	s.mu.Lock()
	current := generation == s.generation
	s.mu.Unlock()
	if !current {
		accessChanged(w)
		return
	}
	reply(w, http.StatusOK, state)
}

// requestDeniedLocked validates the real Host, never a forwarding header. The
// empty-origin path is deliberately loopback-only for offline tests/preview.
func (s *Server) requestDeniedLocked(r *http.Request, api bool) string {
	expectedOrigin := s.origin
	if expectedOrigin != "" {
		host := strings.TrimSuffix(strings.ToLower(r.Host), ":443")
		if host != strings.TrimPrefix(expectedOrigin, "https://") {
			return "invalid_host"
		}
	} else {
		if !loopbackHost(r.Host) {
			return "invalid_host"
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		expectedOrigin = scheme + "://" + r.Host
	}
	if !api {
		return ""
	}
	if values := r.Header.Values("Sec-Fetch-Site"); len(values) > 1 {
		return "cross_site"
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default: // "same-site" is still a different origin.
		return "cross_site"
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 {
		return "invalid_origin"
	}
	if len(origins) == 1 {
		origin := origins[0]
		if strings.HasPrefix(origin, "https://") {
			origin = strings.TrimSuffix(origin, ":443")
		}
		if origin == "" || !strings.EqualFold(origin, expectedOrigin) {
			return "invalid_origin"
		}
	}
	return ""
}

func loopbackHost(host string) bool {
	name := host
	if parsed, port, err := net.SplitHostPort(host); err == nil {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return false
		}
		name = parsed
	} else if strings.Contains(host, ":") && host != "[::1]" {
		return false
	}
	if strings.EqualFold(name, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(name, "[]"))
	return ip != nil && ip.IsLoopback()
}

func requestDenied(w http.ResponseWriter, code string) {
	failure(w, http.StatusForbidden, "request host or origin is not allowed", code)
}

func (s *Server) checkRequest(w http.ResponseWriter, r *http.Request, api bool) bool {
	s.mu.Lock()
	code := s.requestDeniedLocked(r, api)
	s.mu.Unlock()
	if code != "" {
		requestDenied(w, code)
		return false
	}
	return true
}

func (s *Server) admitted(next endpoint, mutation bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if code := s.requestDeniedLocked(r, true); code != "" {
			s.mu.Unlock()
			requestDenied(w, code)
			return
		}
		if !s.sharing {
			s.mu.Unlock()
			failure(w, http.StatusForbidden, "remote service is stopped", "sharing_disabled")
			return
		}
		if mutation && (len(r.Header.Values("X-MirrorBird-CSRF")) != 1 || s.csrf == "" ||
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-MirrorBird-CSRF")), []byte(s.csrf)) != 1) {
			s.mu.Unlock()
			failure(w, http.StatusForbidden, "invalid CSRF proof", "csrf_failed")
			return
		}
		a := admission{generation: s.generation, stop: s.stop, lifetime: s.lifetime, threadID: r.PathValue("id")}
		if a.threadID != "" && (!utf8.ValidString(a.threadID) || len(a.threadID) > 512 || strings.ContainsAny(a.threadID, "/\\\r\n\x00")) {
			s.mu.Unlock()
			failure(w, http.StatusBadRequest, "invalid thread identifier", "invalid_input")
			return
		}
		if mutation {
			// This is the actual loaded cache generation, not catalogue membership.
			// Reads initialize lazily outside this lock; writes never do so.
			a.projectionGeneration = s.Backend.ProjectionGeneration(a.threadID)
			if a.projectionGeneration == 0 {
				s.mu.Unlock()
				backendFailure(w, session.ErrStateChanged, false)
				return
			}
		}
		s.mu.Unlock()
		next(w, r, a)
	}
}

func (s *Server) admissionErrorLocked(a admission) error {
	if !s.sharing || a.generation != s.generation {
		return errAccessChanged
	}
	if a.projectionGeneration != 0 && s.Backend.ProjectionGeneration(a.threadID) != a.projectionGeneration {
		return session.ErrStateChanged
	}
	return nil
}

func (s *Server) validLocked(a admission) bool { return s.admissionErrorLocked(a) == nil }

func (s *Server) valid(a admission) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validLocked(a)
}

func requestContext(request, lifetime context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	// Parenting on the service lifetime makes Stop cancellation synchronous;
	// reads also observe the browser request without retaining a watcher goroutine.
	ctx, cancel := context.WithTimeout(lifetime, timeout)
	stopRequest := context.AfterFunc(request, cancel)
	if request.Err() != nil {
		cancel()
	}
	return ctx, func() { stopRequest(); cancel() }
}

func boundedQuery(r *http.Request, allowed ...string) (url.Values, error) {
	if len(r.URL.RawQuery) > maxQueryBytes {
		return nil, errors.New("query is too large")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	for key, items := range values {
		known := false
		for _, name := range allowed {
			known = known || key == name
		}
		if !known || len(items) != 1 {
			return nil, errors.New("unknown or repeated query parameter")
		}
	}
	return values, nil
}

func queryBool(values url.Values, key string) (bool, error) {
	if !values.Has(key) {
		return false, nil
	}
	switch values.Get(key) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("query boolean must be true or false")
	}
}

func (s *Server) threads(w http.ResponseWriter, r *http.Request, a admission) {
	values, err := boundedQuery(r, "cursor", "search", "archived", "limit")
	if err != nil {
		failure(w, http.StatusBadRequest, "invalid catalogue query", "invalid_input")
		return
	}
	options := session.ListOptions{Cursor: values.Get("cursor"), Search: values.Get("search"), Limit: defaultListLimit}
	options.Archived, err = queryBool(values, "archived")
	if values.Has("limit") && err == nil {
		options.Limit, err = strconv.Atoi(values.Get("limit"))
	}
	if err != nil || options.Limit < 1 || options.Limit > maxListLimit || len(options.Cursor) > maxCursorBytes || len(options.Search) > maxSearchBytes ||
		!utf8.ValidString(options.Cursor) || !utf8.ValidString(options.Search) || strings.ContainsAny(options.Cursor+options.Search, "\r\n\x00") {
		failure(w, http.StatusBadRequest, "catalogue query is invalid or exceeds its bounds (limit must be 1–100)", "invalid_input")
		return
	}
	ctx, cancel := requestContext(r.Context(), a.lifetime, 15*time.Second)
	defer cancel()
	page, err := s.Backend.ListThreads(ctx, options)
	if !s.valid(a) {
		accessChanged(w)
		return
	}
	if err != nil {
		backendFailure(w, err, false)
		return
	}
	// Never hide additional sessions by silently truncating a malformed page.
	if len(page.Threads) > options.Limit || len(page.NextCursor) > maxCursorBytes {
		backendFailure(w, errors.New("invalid catalogue page"), false)
		return
	}
	if page.Threads == nil {
		page.Threads = []session.Summary{}
	}
	connected := s.Backend.Connected()
	if !s.valid(a) {
		accessChanged(w)
		return
	}
	reply(w, http.StatusOK, struct {
		session.ThreadPage
		Connected bool `json:"connected"`
	}{ThreadPage: page, Connected: connected})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request, a admission) {
	values, err := boundedQuery(r, "archived", "resync")
	if err != nil {
		failure(w, http.StatusBadRequest, "invalid thread query", "invalid_input")
		return
	}
	archived, archiveErr := queryBool(values, "archived")
	resync, resyncErr := queryBool(values, "resync")
	if archiveErr != nil || resyncErr != nil {
		failure(w, http.StatusBadRequest, "archived and resync must be true or false", "invalid_input")
		return
	}
	// A dormant history can require lazy resume plus snapshot/pending reads.
	// Keep this longer budget local to thread reads; catalogue stays at 15s.
	ctx, cancel := requestContext(r.Context(), a.lifetime, 45*time.Second)
	defer cancel()
	// Read owns cache initialization/pinning and validates that projection's
	// lifetime. HTTP owns the service lifetime; no catalogue whitelist is used.
	value, err := s.Backend.Read(ctx, a.threadID, session.ReadOptions{Archived: archived, Resync: resync})
	if !s.valid(a) {
		accessChanged(w)
		return
	}
	if err != nil {
		backendFailure(w, err, false)
		return
	}
	response := browserSnapshot(value)
	if !s.valid(a) {
		accessChanged(w)
		return
	}
	reply(w, http.StatusOK, response)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request, a admission) {
	var input struct {
		Text           string `json:"text"`
		Mode           string `json:"mode"`
		ExpectedTurnID string `json:"expectedTurnId"`
		RequestID      string `json:"requestId"`
	}
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Text) == "" || !utf8.ValidString(input.Text) || len(input.Text) > 32*1024 || len(input.ExpectedTurnID) > 512 ||
		(input.Mode != "start" && input.Mode != "steer") || (input.Mode == "steer" && input.ExpectedTurnID == "") || (input.Mode == "start" && input.ExpectedTurnID != "") {
		failure(w, http.StatusBadRequest, "invalid message or send mode", "invalid_input")
		return
	}
	s.operation(w, r, a, input.RequestID, input, func(ctx context.Context) (json.RawMessage, error) {
		return s.Backend.Send(ctx, a.threadID, input.Text, input.Mode, input.ExpectedTurnID)
	})
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request, a admission) {
	var input struct {
		TurnID    string `json:"turnId"`
		RequestID string `json:"requestId"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.TurnID == "" || len(input.TurnID) > 512 {
		failure(w, http.StatusBadRequest, "turnId is required", "invalid_input")
		return
	}
	s.operation(w, r, a, input.RequestID, input, func(ctx context.Context) (json.RawMessage, error) {
		return s.Backend.Interrupt(ctx, a.threadID, input.TurnID)
	})
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, a admission) {
	var input struct {
		ID        string `json:"id"`
		Decision  string `json:"decision"`
		RequestID string `json:"requestId"`
	}
	if !decode(w, r, &input) {
		return
	}
	id, err := decodeApprovalID(input.ID)
	if err != nil || (input.Decision != "accept" && input.Decision != "decline") {
		failure(w, http.StatusBadRequest, "invalid approval response", "invalid_input")
		return
	}
	s.operation(w, r, a, input.RequestID, input, func(ctx context.Context) (json.RawMessage, error) {
		return s.Backend.Respond(ctx, a.threadID, id, input.Decision)
	})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, a admission) {
	if _, ok := w.(http.Flusher); !ok {
		failure(w, http.StatusInternalServerError, "streaming unsupported", "streaming_unavailable")
		return
	}
	s.mu.Lock()
	if !s.validLocked(a) {
		s.mu.Unlock()
		accessChanged(w)
		return
	}
	if s.streams >= maxStreams {
		s.mu.Unlock()
		failure(w, http.StatusTooManyRequests, "too many event streams", "rate_limited")
		return
	}
	s.streams++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.streams-- // Old-generation cleanup must release only its own slot.
		s.mu.Unlock()
	}()

	changes, cancel := s.Backend.Subscribe()
	defer cancel()
	// Subscribe can race with Stop. Never capture a newer lifetime here.
	if !s.valid(a) {
		accessChanged(w)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(event string, value any) error {
		if !s.valid(a) {
			return errAccessChanged
		}
		data, _ := json.Marshal(value)
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		return controller.Flush()
	}
	if write("ready", map[string]bool{"connected": s.Backend.Connected()}) != nil {
		return
	}
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.stop:
			return
		case change, ok := <-changes:
			if !ok {
				_ = write("reset", map[string]string{"reason": "resync required"})
				return
			}
			// Catalogue hints and per-thread changes are available to every
			// visitor admitted by this service lifetime, not a membership list.
			if write("change", change) != nil || change.Disconnected {
				return
			}
		case <-heartbeat.C:
			if !s.valid(a) {
				return
			}
			controller := http.NewResponseController(w)
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		}
	}
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		failure(w, http.StatusUnsupportedMediaType, "application/json required", "invalid_content_type")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		failure(w, http.StatusBadRequest, "invalid or oversized request body", "invalid_input")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		failure(w, http.StatusBadRequest, "one JSON object required", "invalid_input")
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func failure(w http.ResponseWriter, status int, message, code string) {
	reply(w, status, map[string]string{"error": message, "code": code})
}

func accessChanged(w http.ResponseWriter) {
	failure(w, http.StatusForbidden, "remote service lifetime changed; reconnect before continuing", "access_changed")
}

func backendFailure(w http.ResponseWriter, err error, mutation bool) {
	status, message, code := backendError(err, mutation)
	failure(w, status, message, code)
}

func backendError(err error, mutation bool) (int, string, string) {
	switch {
	case errors.Is(err, session.ErrOutcomeUnknown):
		return http.StatusBadGateway, "the action may have been sent; check Desktop before sending again", "operation_failed_or_outcome_unknown"
	case errors.Is(err, session.ErrInvalidID), errors.Is(err, session.ErrInvalidListOptions):
		return http.StatusBadRequest, "invalid thread identifier or catalogue options", "invalid_input"
	case errors.Is(err, session.ErrProjectionCapacity):
		return http.StatusTooManyRequests, "thread cache is busy; retry the read after pending actions finish", "capacity_limited"
	case errors.Is(err, session.ErrNoAttachment):
		return http.StatusServiceUnavailable, "Desktop is not connected", "disconnected"
	case errors.Is(err, session.ErrStateChanged):
		return http.StatusConflict, "the thread or active turn changed; refresh before sending", "state_changed"
	case errors.Is(err, session.ErrResyncRequired):
		return http.StatusConflict, "this thread needs resynchronization; refresh its snapshot before continuing", "resync_required"
	case mutation:
		return http.StatusBadGateway, "the action may have been sent; check Desktop before sending again", "operation_failed_or_outcome_unknown"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "Desktop did not respond in time", "timeout"
	default:
		return http.StatusBadGateway, "Desktop data is temporarily unavailable", "backend_unavailable"
	}
}

func PortAddress(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }
