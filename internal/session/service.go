package session

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/bridge"
)

var ErrNoAttachment = errors.New("Desktop is not connected through MirrorBird")
var ErrStateChanged = errors.New("the session state changed; refresh before trying again")
var ErrResyncRequired = errors.New("session requires explicit resynchronization; reconnect if event ordering was lost")
var ErrOutcomeUnknown = errors.New("operation outcome is unknown; check Desktop before trying again")
var ErrInvalidID = errors.New("invalid thread ID")
var ErrInvalidListOptions = errors.New("invalid catalogue options: limit must be 1–100, cursor at most 4096 bytes, search at most 512 bytes")
var ErrProjectionCapacity = errors.New("session cache is busy; retry the read after current work completes")

type projectionGenerationKey struct{}

// WithProjectionGeneration binds work to the cache lifetime captured by its
// caller. Unbound local callers still capture identity before commandGate wait.
func WithProjectionGeneration(ctx context.Context, generation uint64) context.Context {
	return context.WithValue(ctx, projectionGenerationKey{}, generation)
}

type Change struct {
	Revision         uint64 `json:"revision"`
	ThreadID         string `json:"threadId,omitempty"`
	Disconnected     bool   `json:"disconnected,omitempty"`
	CatalogueChanged bool   `json:"catalogueChanged,omitempty"`
	// Only validated text/output notifications may leave action state unchanged.
	ContentOnly bool `json:"contentOnly,omitempty"`
}

// Kept private: tests exercise the service without a Desktop process or IPC.
// A connection wrapper, not interface equality, is the attachment identity.
type attachmentClient interface {
	Call(context.Context, string, any) (json.RawMessage, error)
	Subscribe(context.Context) error
	SubscriptionSequence() uint64
	Events() <-chan bridge.Event
	Done() <-chan struct{}
	Metadata() attachment.Metadata
	Close()
}

type connection struct {
	client       attachmentClient
	ctx          context.Context
	cancel       context.CancelFunc
	lastSequence uint64
	initialFloor uint64
	orderFailed  bool
	progress     chan struct{}
	releases     chan *projection
	watchSlots   chan struct{} // includes retired projections until unwatch settles
}

type Service struct {
	mu             sync.Mutex
	conn           *connection
	connectAttempt uint64
	nextGeneration uint64
	revision       uint64
	lastUse        uint64
	projections    map[string]*projection
	listeners      map[chan Change]struct{}
	commandGate    chan struct{}
	home           string
	discover       func(context.Context, string) (attachmentClient, error)
}

func New(home string) *Service {
	s := &Service{
		home: home, projections: make(map[string]*projection),
		listeners: make(map[chan Change]struct{}), commandGate: make(chan struct{}, 1),
		discover: func(ctx context.Context, home string) (attachmentClient, error) {
			return attachment.Discover(ctx, home)
		},
	}
	s.commandGate <- struct{}{}
	return s
}

func (s *Service) Connect(ctx context.Context) error {
	s.mu.Lock()
	s.connectAttempt++
	attempt := s.connectAttempt
	s.mu.Unlock()
	client, err := s.discover(ctx, s.home)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrNoAttachment
	}
	if err = client.Subscribe(ctx); err != nil {
		client.Close()
		return err
	}
	cctx, cancel := context.WithCancel(context.Background())
	c := &connection{client: client, ctx: cctx, cancel: cancel, initialFloor: client.SubscriptionSequence(),
		progress: make(chan struct{}), releases: make(chan *projection, 128), watchSlots: make(chan struct{}, maxProjections)}
	s.mu.Lock()
	if attempt != s.connectAttempt || ctx.Err() != nil || clientClosed(client) {
		s.mu.Unlock()
		cancel()
		client.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrNoAttachment
	}
	old := s.conn
	for _, p := range s.projections {
		p.cancel()
	}
	s.conn = c
	s.projections = make(map[string]*projection)
	s.connectionChangedLocked()
	s.mu.Unlock()
	if old != nil {
		old.cancel()
		old.client.Close()
	}
	go s.observe(c)
	go s.releaseWatches(c)
	return nil
}

func clientClosed(client attachmentClient) bool {
	select {
	case <-client.Done():
		return true
	default:
		return false
	}
}

func (s *Service) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil && !clientClosed(s.conn.client)
}

func (s *Service) EnginePID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil || clientClosed(s.conn.client) {
		return 0
	}
	return s.conn.client.Metadata().EnginePID
}

func (s *Service) Close() {
	s.mu.Lock()
	s.connectAttempt++ // also invalidates a discovery that is still in flight
	c := s.conn
	s.conn = nil
	for _, p := range s.projections {
		p.cancel()
	}
	s.projections = make(map[string]*projection)
	s.connectionChangedLocked()
	s.mu.Unlock()
	if c != nil {
		c.cancel()
		c.client.Close()
	}
}

// ClearProjections cancels local cache lifetimes synchronously. Bounded unwatch
// cleanup never sends an upstream unsubscribe or waits for Desktop on Stop.
func (s *Service) ClearProjections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.projections {
		s.retireProjectionLocked(p)
	}
}

// ProjectionGeneration is a process-local lifetime token, never an event
// revision. Initializing/absent projections have no admissible generation.
func (s *Service) ProjectionGeneration(id string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.projections[id]
	if p == nil || !p.ready || s.conn != p.conn || clientClosed(p.conn.client) {
		return 0
	}
	return p.generation
}

func (s *Service) currentConnection() (*connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil || clientClosed(s.conn.client) {
		return nil, ErrNoAttachment
	}
	return s.conn, nil
}

// Refresh is a cheap health check, not a directory scan. An ordering failure
// requires an explicit reconnect; a status response cannot repair a lost stream.
func (s *Service) Refresh(ctx context.Context) error {
	c, err := s.currentConnection()
	if err != nil {
		return err
	}
	data, err := c.client.Call(ctx, "status", nil)
	if err != nil {
		return err
	}
	var status struct {
		Ready *bool `json:"ready"`
	}
	if len(data) > 1024 || json.Unmarshal(data, &status) != nil || status.Ready == nil {
		return errors.New("invalid attachment status response")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != c || clientClosed(c.client) || !*status.Ready {
		return ErrNoAttachment
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if c.orderFailed {
		return ErrResyncRequired
	}
	return nil
}

// Directory decoding deliberately excludes turns/items, even if a server sends
// them. Unloaded/null capability is metadata, not a reason to hide a session.
type catalogueThread struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	CWD    string `json:"cwd"`
	Status struct {
		Type string `json:"type"`
	} `json:"status"`
	UpdatedAt            int64 `json:"updatedAt"`
	CanAcceptDirectInput *bool `json:"canAcceptDirectInput"`
	Ephemeral            bool  `json:"ephemeral"`
}

func (t catalogueThread) summary(archived bool) (Summary, error) {
	if !validID(t.ID) || len(t.Name) > maxTextBytes || len(t.CWD) > maxTextBytes || len(t.Status.Type) > 64 {
		return Summary{}, errors.New("invalid thread metadata response")
	}
	return Summary{ID: t.ID, Name: displayName(t.Name), Status: t.Status.Type, Archived: archived,
		CWD: t.CWD, UpdatedAt: t.UpdatedAt, CanAcceptDirectInput: copyBool(t.CanAcceptDirectInput)}, nil
}

func displayName(name string) string {
	if name == "" {
		return "Untitled session"
	}
	return name
}

// ListThreads has no aggregate visibility cap and never changes projection
// membership. Its opaque cursor walks persisted metadata, then loaded ephemeral
// threads that do not have rollout files. Only that latter page needs individual
// metadata-only reads; neither phase hydrates transcripts or resumes threads.
func (s *Service) ListThreads(ctx context.Context, options ListOptions) (ThreadPage, error) {
	if options.Limit == 0 {
		options.Limit = 50
	}
	if options.Limit < 1 || options.Limit > 100 || len(options.Cursor) > 4096 || len(options.Search) > 512 || !utf8.ValidString(options.Cursor) || !utf8.ValidString(options.Search) {
		return ThreadPage{}, ErrInvalidListOptions
	}
	phase, cursor := "stored", ""
	if options.Cursor != "" {
		var ok bool
		phase, cursor, ok = strings.Cut(options.Cursor, ":")
		if !ok || (phase != "stored" && phase != "loaded") || (options.Archived && phase == "loaded") {
			return ThreadPage{}, ErrInvalidListOptions
		}
	}
	c, err := s.currentConnection()
	if err != nil {
		return ThreadPage{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	page := ThreadPage{Threads: []Summary{}}
	if phase == "stored" {
		params := map[string]any{
			"sourceKinds":    []string{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown"},
			"modelProviders": []string{}, "archived": options.Archived, "limit": options.Limit,
			"sortKey": "updated_at", "sortDirection": "desc",
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if options.Search != "" {
			params["searchTerm"] = options.Search
		}
		data, callErr := c.client.Call(callCtx, "thread/list", params)
		if callErr != nil {
			return ThreadPage{}, callErr
		}
		var result struct {
			Data       []catalogueThread `json:"data"`
			NextCursor string            `json:"nextCursor"`
		}
		if len(data) > maxRPCBytes || !utf8.Valid(data) || json.Unmarshal(data, &result) != nil || result.Data == nil || len(result.Data) > options.Limit {
			return ThreadPage{}, errors.New("invalid thread catalogue response")
		}
		for _, thread := range result.Data {
			summary, err := thread.summary(options.Archived)
			if err != nil {
				return ThreadPage{}, err
			}
			page.Threads = append(page.Threads, summary)
		}
		if result.NextCursor != "" {
			if result.NextCursor == cursor {
				return ThreadPage{}, errors.New("thread catalogue cursor did not advance")
			}
			page.NextCursor = "stored:" + result.NextCursor
		} else if !options.Archived {
			page.NextCursor = "loaded:"
		}
	} else {
		params := map[string]any{"limit": options.Limit}
		if cursor != "" {
			params["cursor"] = cursor
		}
		data, callErr := c.client.Call(callCtx, "thread/loaded/list", params)
		if callErr != nil {
			return ThreadPage{}, callErr
		}
		var result struct {
			Data       []string `json:"data"`
			NextCursor string   `json:"nextCursor"`
		}
		if len(data) > maxRPCBytes || !utf8.Valid(data) || json.Unmarshal(data, &result) != nil || result.Data == nil || len(result.Data) > options.Limit {
			return ThreadPage{}, errors.New("invalid loaded-thread catalogue response")
		}
		for _, id := range result.Data {
			if !validID(id) {
				return ThreadPage{}, errors.New("invalid loaded thread ID")
			}
			thread, err := readMetadata(callCtx, c, id)
			if err != nil {
				return ThreadPage{}, err
			}
			if !thread.Ephemeral || (options.Search != "" && !strings.Contains(strings.ToLower(thread.Name), strings.ToLower(options.Search))) {
				continue
			}
			summary, err := thread.summary(false)
			if err != nil {
				return ThreadPage{}, err
			}
			page.Threads = append(page.Threads, summary)
		}
		if result.NextCursor != "" {
			if result.NextCursor == cursor {
				return ThreadPage{}, errors.New("loaded-thread cursor did not advance")
			}
			page.NextCursor = "loaded:" + result.NextCursor
		}
	}
	if len(page.NextCursor) > 4096 {
		return ThreadPage{}, errors.New("thread catalogue cursor exceeds its limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != c || clientClosed(c.client) {
		return ThreadPage{}, ErrNoAttachment
	}
	if callCtx.Err() != nil {
		return ThreadPage{}, callCtx.Err()
	}
	return page, nil
}

func readMetadata(ctx context.Context, c *connection, id string) (catalogueThread, error) {
	data, err := c.client.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
	if err != nil {
		return catalogueThread{}, err
	}
	var result struct {
		Thread catalogueThread `json:"thread"`
	}
	if len(data) > maxRPCBytes || !utf8.Valid(data) || json.Unmarshal(data, &result) != nil || result.Thread.ID != id {
		return catalogueThread{}, errors.New("invalid thread metadata response")
	}
	if _, err := result.Thread.summary(false); err != nil {
		return catalogueThread{}, err
	}
	return result.Thread, nil
}

func (s *Service) touchLocked(p *projection) {
	s.lastUse++
	p.lastUse = s.lastUse
}

func (s *Service) unpin(p *projection) {
	s.mu.Lock()
	p.pins--
	s.mu.Unlock()
}

func (s *Service) retireProjectionLocked(p *projection) {
	if s.projections[p.id] != p {
		return
	}
	delete(s.projections, p.id)
	p.cancel()
	s.changedLocked(p.id, nil)
	s.releaseWatchLocked(p)
}

// Read pins one exact projection through initialization and copying. No prior
// catalogue membership is needed. Resync is an explicit bounded cache rebuild,
// never a retry of an input operation and never a repair of a lost event stream.
func (s *Service) Read(ctx context.Context, id string, options ReadOptions) (Snapshot, error) {
	if ctx.Err() != nil {
		return Snapshot{}, ctx.Err()
	}
	if !validID(id) {
		return Snapshot{}, ErrInvalidID
	}
	s.mu.Lock()
	c := s.conn
	if c == nil || clientClosed(c.client) {
		s.mu.Unlock()
		return Snapshot{}, ErrNoAttachment
	}
	p := s.projections[id]
	if generation, _ := ctx.Value(projectionGenerationKey{}).(uint64); generation != 0 && (p == nil || !p.ready || generation != p.generation) {
		s.mu.Unlock()
		return Snapshot{}, ErrStateChanged
	}
	if p != nil && (p.archived != options.Archived || (options.Resync && p.ready)) {
		if p.pins != 0 {
			s.mu.Unlock()
			return Snapshot{}, ErrProjectionCapacity
		}
		if c.orderFailed {
			s.mu.Unlock()
			return Snapshot{}, ErrResyncRequired
		}
		s.retireProjectionLocked(p)
		p = nil
	}
	created := p == nil
	if created {
		if c.orderFailed {
			s.mu.Unlock()
			return Snapshot{}, ErrResyncRequired
		}
		if len(s.projections) >= maxProjections {
			var oldest *projection
			for _, candidate := range s.projections {
				if candidate.pins == 0 && candidate.ready && (oldest == nil || candidate.lastUse < oldest.lastUse) {
					oldest = candidate
				}
			}
			if oldest == nil {
				s.mu.Unlock()
				return Snapshot{}, ErrProjectionCapacity
			}
			s.retireProjectionLocked(oldest)
		}
		if s.nextGeneration == ^uint64(0) {
			s.mu.Unlock()
			return Snapshot{}, ErrProjectionCapacity
		}
		s.nextGeneration++
		pctx, cancel := context.WithCancel(c.ctx)
		p = &projection{id: id, generation: s.nextGeneration, conn: c, ctx: pctx, cancel: cancel,
			archived: options.Archived, initialized: make(chan struct{}), pending: make(map[string]*pendingEntry)}
		// Install buffering before watch/read/resume on the existing attachment.
		s.projections[id] = p
	}
	p.pins++
	s.touchLocked(p)
	s.mu.Unlock()
	defer s.unpin(p)

	if created {
		initCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		stop := context.AfterFunc(p.ctx, cancel)
		err := s.initialize(initCtx, p)
		stop()
		cancel()
		s.mu.Lock()
		if s.conn != c || clientClosed(c.client) {
			err = ErrNoAttachment
		} else if s.projections[id] != p {
			err = ErrStateChanged
		} else if p.initErr != nil {
			err = p.initErr
		} else if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			p.initErr = err
			if s.projections[id] == p {
				delete(s.projections, id)
			}
			p.cancel()
			p.buffer = nil
			s.releaseWatchLocked(p)
		}
		close(p.initialized)
		s.mu.Unlock()
	} else {
		select {
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		case <-p.initialized:
		case <-p.ctx.Done():
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != c || clientClosed(c.client) {
		return Snapshot{}, ErrNoAttachment
	}
	if p.initErr != nil {
		return Snapshot{}, p.initErr
	}
	if s.projections[id] != p || !p.ready {
		return Snapshot{}, ErrStateChanged
	}
	if ctx.Err() != nil {
		return Snapshot{}, ctx.Err()
	}
	out := copySnapshot(p.snapshot)
	out.Pending = p.pendingSnapshot()
	return out, nil
}

func (s *Service) releaseWatchLocked(p *projection) {
	if p.releaseQueued {
		return
	}
	p.releaseQueued = true
	select {
	case p.conn.releases <- p:
	default:
		// Closing local IPC releases its watches. A cleanup backlog cannot grow
		// without bound or silently retain abandoned bridge capacity.
		p.conn.client.Close()
	}
}

func (s *Service) releaseWatches(c *connection) {
	for {
		select {
		case <-c.ctx.Done():
			return
		case p := <-c.releases:
			select {
			case <-p.initialized:
			case <-c.ctx.Done():
				return
			}
			if p.watchAttempted {
				ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
				_, err := c.client.Call(ctx, "unwatch", map[string]string{"threadId": p.id, "watchId": strconv.FormatUint(p.generation, 10)})
				cancel()
				if err != nil {
					c.client.Close()
					return
				}
			}
			if p.watchSlotHeld {
				<-c.watchSlots
			}
		}
	}
}

func (s *Service) projectionLocked(ctx context.Context, id string) (*projection, error) {
	p := s.projections[id]
	if p == nil || !p.ready {
		return nil, ErrStateChanged
	}
	if generation, _ := ctx.Value(projectionGenerationKey{}).(uint64); generation != 0 && generation != p.generation {
		return nil, ErrStateChanged
	}
	if s.conn != p.conn || clientClosed(p.conn.client) {
		return nil, ErrNoAttachment
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return p, nil
}

func (s *Service) sameProjectionLocked(ctx context.Context, p *projection) error {
	current, err := s.projectionLocked(ctx, p.id)
	if err != nil {
		return err
	}
	if current != p {
		return ErrStateChanged
	}
	return nil
}

func (s *Service) acquireCommand(ctx context.Context, id string) (*projection, func(), error) {
	// Pin BEFORE waiting, so queued work cannot be evicted or switch lifetimes.
	s.mu.Lock()
	p, err := s.projectionLocked(ctx, id)
	if err == nil {
		p.pins++
		s.touchLocked(p)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	select {
	case <-ctx.Done():
		s.unpin(p)
		return nil, nil, ctx.Err()
	case <-p.ctx.Done():
		s.unpin(p)
		return nil, nil, ErrStateChanged
	case <-s.commandGate:
	}
	release := func() {
		s.commandGate <- struct{}{}
		s.unpin(p)
	}
	s.mu.Lock()
	err = s.sameProjectionLocked(ctx, p)
	if err == nil {
		err = p.stateError()
	}
	if err == nil && (p.snapshot.ReadOnly || !acceptsInput(p.snapshot.Thread.CanAcceptDirectInput)) {
		err = ErrStateChanged
	}
	s.mu.Unlock()
	if err != nil {
		release()
		return nil, nil, err
	}
	return p, release, nil
}

func (s *Service) Send(ctx context.Context, id, text, mode, expectedTurn string) (json.RawMessage, error) {
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || len(text) > 32*1024 {
		return nil, errors.New("message must contain 1–32768 bytes of text")
	}
	if mode != "start" && mode != "steer" {
		return nil, errors.New("invalid send mode")
	}
	p, release, err := s.acquireCommand(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	s.mu.Lock()
	if err = s.sameProjectionLocked(ctx, p); err == nil {
		active := p.snapshot.ActiveTurnID
		if mode == "steer" {
			if expectedTurn == "" || active != expectedTurn {
				err = ErrStateChanged
			}
		} else if active != "" || p.startPending || p.snapshot.Thread.Status.Type == "active" {
			err = ErrStateChanged
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	params := map[string]any{"threadId": id, "input": []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}}
	method := "turn/start"
	if mode == "steer" {
		params["expectedTurnId"] = expectedTurn
		// Desktop hides later anonymous user messages. Each deliberate steer gets
		// a distinct identity for its real item notification, never a local echo.
		params["clientUserMessageId"] = rand.Text()
		method = "turn/steer"
	}
	return s.callCommand(ctx, p, method, params, nil)
}

func (s *Service) Interrupt(ctx context.Context, id, turnID string) (json.RawMessage, error) {
	if turnID == "" {
		return nil, ErrStateChanged
	}
	p, release, err := s.acquireCommand(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	s.mu.Lock()
	err = s.sameProjectionLocked(ctx, p)
	if err == nil && p.snapshot.ActiveTurnID != turnID {
		err = ErrStateChanged
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.callCommand(ctx, p, "turn/interrupt", map[string]string{"threadId": id, "turnId": turnID}, nil)
}

func (s *Service) Respond(ctx context.Context, id string, requestID json.RawMessage, decision string) (json.RawMessage, error) {
	if decision != "accept" && decision != "decline" {
		return nil, errors.New("invalid approval decision")
	}
	if len(requestID) > maxIDBytes {
		return nil, ErrStateChanged
	}
	key, err := bridge.RequestIDKey(requestID)
	if err != nil {
		return nil, ErrStateChanged
	}
	p, release, err := s.acquireCommand(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	s.mu.Lock()
	err = s.sameProjectionLocked(ctx, p)
	request := p.pending[key]
	if err == nil && (request == nil || !p.phoneApprovalAllowed(request.request)) {
		err = ErrStateChanged
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.callCommand(ctx, p, "respond", map[string]any{"id": request.request.ID, "result": map[string]string{"decision": decision}}, request)
}

func (s *Service) callCommand(ctx context.Context, p *projection, method string, params any, approval *pendingEntry) (json.RawMessage, error) {
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	stop := context.AfterFunc(p.ctx, cancel)
	defer func() { stop(); cancel() }()
	// The final admission uses the same mutex as lifetime teardown. A stale
	// response cannot be returned or projected into a replacement cache.
	s.mu.Lock()
	err := s.sameProjectionLocked(callCtx, p)
	if err == nil {
		err = p.stateError()
	}
	if err == nil && (p.snapshot.ReadOnly || !acceptsInput(p.snapshot.Thread.CanAcceptDirectInput)) {
		err = ErrStateChanged
	}
	if err == nil {
		switch method {
		case "turn/start":
			if p.startPending || p.snapshot.ActiveTurnID != "" || p.snapshot.Thread.Status.Type == "active" {
				err = ErrStateChanged
			}
		case "turn/steer":
			if input, ok := params.(map[string]any); !ok || p.snapshot.ActiveTurnID == "" || p.snapshot.ActiveTurnID != input["expectedTurnId"] {
				err = ErrStateChanged
			}
		case "turn/interrupt":
			if input, ok := params.(map[string]string); !ok || p.snapshot.ActiveTurnID == "" || p.snapshot.ActiveTurnID != input["turnId"] {
				err = ErrStateChanged
			}
		}
	}
	if err == nil && approval != nil {
		key, _ := bridge.RequestIDKey(approval.request.ID)
		if p.pending[key] != approval || !p.phoneApprovalAllowed(approval.request) {
			err = ErrStateChanged
		}
	}
	if err == nil && method == "turn/start" {
		p.startPending = true
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	result, err := p.conn.client.Call(callCtx, method, params)
	if err == nil {
		// Response delivery can race the observer. Fence lifecycle/resolution
		// events before another command relies on this cache; no history RPC.
		err = s.commandBarrier(callCtx, p)
		if err == nil && method == "turn/start" {
			err = s.awaitStarted(callCtx, p)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if stateErr := s.sameProjectionLocked(ctx, p); stateErr != nil {
		if s.conn == p.conn && s.projections[p.id] == p {
			s.failProjectionLocked(p, "command outcome is uncertain; check Desktop before explicit resync")
		}
		return nil, ErrOutcomeUnknown
	}
	if approval != nil && (err == nil || errors.Is(err, attachment.ErrRequestResolved)) {
		key, _ := bridge.RequestIDKey(approval.request.ID)
		if p.pending[key] == approval {
			delete(p.pending, key)
			s.changedLocked(p.id, p)
		}
		if errors.Is(err, attachment.ErrRequestResolved) {
			return nil, ErrStateChanged
		}
	}
	if err != nil {
		// No retry: without ordered success we cannot prove whether work ran.
		s.failProjectionLocked(p, "command outcome is uncertain; check Desktop before explicit resync")
		return nil, ErrOutcomeUnknown
	}
	if len(result) > maxRPCBytes {
		s.failProjectionLocked(p, "command response exceeds the data limit")
		return nil, ErrOutcomeUnknown
	}
	if method == "turn/start" && p.startPending {
		s.failProjectionLocked(p, "turn start was accepted without an ordered lifecycle event")
		return nil, ErrOutcomeUnknown
	}
	return append(json.RawMessage(nil), result...), nil
}

func (s *Service) awaitStarted(ctx context.Context, p *projection) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		s.mu.Lock()
		err := s.sameProjectionLocked(ctx, p)
		if err == nil {
			err = p.stateError()
		}
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if !p.startPending {
			s.mu.Unlock()
			return nil
		}
		progress := p.conn.progress
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.ctx.Done():
			return ErrStateChanged
		case <-p.conn.client.Done():
			return ErrNoAttachment
		case <-progress:
		}
	}
}

func (s *Service) commandBarrier(ctx context.Context, p *projection) error {
	s.mu.Lock()
	if err := s.sameProjectionLocked(ctx, p); err != nil {
		s.mu.Unlock()
		return err
	}
	before := p.conn.lastSequence
	s.mu.Unlock()
	data, err := p.conn.client.Call(ctx, "barrier", nil)
	if err != nil {
		return err
	}
	var fence struct {
		Sequence *uint64 `json:"sequence"`
	}
	if len(data) > 1024 || json.Unmarshal(data, &fence) != nil || fence.Sequence == nil || *fence.Sequence < before {
		return ErrResyncRequired
	}
	return s.awaitSequence(ctx, p, *fence.Sequence)
}

func (s *Service) Subscribe() (<-chan Change, func()) {
	ch := make(chan Change, 32)
	s.mu.Lock()
	if len(s.listeners) >= 128 {
		close(ch)
	} else {
		s.listeners[ch] = struct{}{}
	}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			if _, ok := s.listeners[ch]; ok {
				delete(s.listeners, ch)
				close(ch)
			}
			s.mu.Unlock()
		})
	}
}

func (s *Service) broadcastLocked(change Change) {
	for ch := range s.listeners {
		select {
		case ch <- change:
		default:
			delete(s.listeners, ch)
			close(ch)
		}
	}
}

func (s *Service) connectionChangedLocked() {
	s.revision++
	s.broadcastLocked(Change{Revision: s.revision, Disconnected: true})
}

func (s *Service) catalogueChangedLocked() {
	s.revision++
	// No ThreadID: this hint does not invalidate any selected transcript/controls.
	s.broadcastLocked(Change{Revision: s.revision, CatalogueChanged: true})
}

func (s *Service) changedLocked(id string, p *projection) {
	s.publishChangeLocked(id, p, false)
}

func (s *Service) publishChangeLocked(id string, p *projection, contentOnly bool) {
	s.revision++
	if p != nil {
		p.snapshot.Revision = s.revision
		p.snapshot.ActiveTurnID = activeTurn(p.snapshot.Thread)
		if p.snapshot.ReadOnly {
			p.snapshot.ActiveTurnID = ""
		}
	}
	s.broadcastLocked(Change{ThreadID: id, Revision: s.revision, ContentOnly: contentOnly})
}
