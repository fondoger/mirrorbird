package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/bridge"
)

const (
	maxProjections     = bridge.MaxWatchedThreads
	maxTurns           = 30
	maxItemsPerTurn    = 100
	maxIDBytes         = 512
	maxTextBytes       = 256 << 10
	maxItemBytes       = 512 << 10
	maxProjectionBytes = 4 << 20
	maxRPCBytes        = 8 << 20
	maxBufferedEvents  = 256
	maxBufferedBytes   = 4 << 20
	maxPending         = 128
	maxPendingBytes    = 512 << 10
)

type pendingEntry struct {
	request PendingRequest
	order   uint64
}

type projection struct {
	id              string
	generation      uint64
	conn            *connection
	ctx             context.Context
	cancel          context.CancelFunc
	initialized     chan struct{}
	archived        bool
	pins            int
	lastUse         uint64
	watchSlotHeld   bool
	watchAttempted  bool
	releaseQueued   bool
	ready           bool
	initErr         error
	buffer          []projectionEvent
	bufferBytes     int
	snapshot        Snapshot
	pending         map[string]*pendingEntry
	pendingOrder    uint64
	stateSequence   uint64
	pendingSequence uint64
	startPending    bool
	stateBytes      int
	itemBytes       map[string]int // JSON storage budgets keyed by turn/item identity
}

type projectionEvent struct {
	sequence uint64
	size     int
	message  eventMessage
}

type eventMessage struct {
	ID       json.RawMessage `json:"id"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
	threadID string
}

type turnsPage struct {
	Data       []Turn `json:"data"`
	NextCursor string `json:"nextCursor"`
}

type resumeResult struct {
	Thread           Thread     `json:"thread"`
	Model            string     `json:"model"`
	InitialTurnsPage *turnsPage `json:"initialTurnsPage"`
	TurnsCursor      string     `json:"turnsBackwardsCursor"`
	ItemsCursor      string     `json:"itemsBackwardsCursor"`
}

type sequencedResult struct {
	Result   json.RawMessage `json:"result"`
	Sequence *uint64         `json:"sequence"`
}

func (p *projection) stateError() error {
	if p.initErr != nil {
		return p.initErr
	}
	if p.snapshot.ResyncRequired {
		return ErrResyncRequired
	}
	return nil
}

func validID(id string) bool { return id != "" && len(id) <= maxIDBytes && utf8.ValidString(id) }

func decodeSequenced(raw json.RawMessage, out any) (uint64, error) {
	var envelope sequencedResult
	if len(raw) > maxRPCBytes || !utf8.Valid(raw) || json.Unmarshal(raw, &envelope) != nil || envelope.Sequence == nil || *envelope.Sequence == 0 || len(envelope.Result) == 0 {
		return 0, ErrResyncRequired
	}
	if json.Unmarshal(envelope.Result, out) != nil {
		return 0, ErrResyncRequired
	}
	return *envelope.Sequence, nil
}

func (s *Service) initialize(ctx context.Context, p *projection) error {
	// Cache eviction may have queued an unwatch. Reserve the local bridge budget
	// only after its acknowledgement, not merely after removing an LRU entry.
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	select {
	case p.conn.watchSlots <- struct{}{}:
		p.watchSlotHeld = true
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrProjectionCapacity
	case <-p.conn.client.Done():
		return ErrNoAttachment
	}
	s.mu.Lock()
	if s.projections[p.id] != p || s.conn != p.conn {
		s.mu.Unlock()
		return ErrStateChanged
	}
	p.watchAttempted = true
	s.mu.Unlock()
	if _, err := p.conn.client.Call(ctx, "watch", map[string]string{"threadId": p.id, "watchId": strconv.FormatUint(p.generation, 10)}); err != nil {
		return err
	}
	readOnly := p.archived
	if !readOnly {
		metadata, err := readMetadata(ctx, p.conn, p.id)
		if err != nil {
			return err
		}
		readOnly = metadata.CanAcceptDirectInput != nil && !*metadata.CanAcceptDirectInput
	}
	data, err := p.conn.client.Call(ctx, "snapshot", map[string]any{"threadId": p.id, "readOnly": readOnly})
	if err != nil {
		return err
	}
	var resumed resumeResult
	sequence, err := decodeSequenced(data, &resumed)
	if err != nil || resumed.Thread.ID != p.id {
		return ErrResyncRequired
	}
	candidate := Snapshot{Thread: resumed.Thread, Pending: []PendingRequest{}, ReadOnly: readOnly || !acceptsInput(resumed.Thread.CanAcceptDirectInput)}
	if p.archived {
		candidate.ReadOnlyReason = "Archived history; viewing does not unarchive the thread."
	} else if candidate.ReadOnly {
		candidate.ReadOnlyReason = "The app-server does not expose direct input for this thread."
	}
	if candidate.Thread.Model == "" {
		candidate.Thread.Model = resumed.Model
	}
	if resumed.InitialTurnsPage != nil {
		// Descending pagination is requested by the attachment host. thread.turns
		// is the live resume view and wins over the persisted page on overlaps.
		page := resumed.InitialTurnsPage.Data
		reverseTurns(page)
		candidate.Thread.Turns = mergeResumeTurns(page, resumed.Thread.Turns)
		candidate.HistoryTruncated = resumed.InitialTurnsPage.NextCursor != ""
	}
	if resumed.ItemsCursor != "" {
		candidate.HistoryTruncated = true
	}

	// Older paginated servers may omit the bundled page. Fetch exactly one
	// bounded page, and only graft completed history onto an idle resume. The
	// second read is NEVER treated as an atomic state at the resume sequence.
	var historySequence uint64
	needsHistory := resumed.InitialTurnsPage == nil && (len(candidate.Thread.Turns) == 0 || resumed.TurnsCursor != "")
	if needsHistory {
		data, err = p.conn.client.Call(ctx, "history", map[string]string{"threadId": p.id})
		if err != nil {
			return fmt.Errorf("bounded history is unavailable for this thread: %w", err)
		}
		var page turnsPage
		historySequence, err = decodeSequenced(data, &page)
		if err != nil || historySequence <= sequence {
			return ErrResyncRequired
		}
		if !candidate.ReadOnly {
			if candidate.Thread.Status.Type == "active" || activeTurn(candidate.Thread) != "" {
				return ErrResyncRequired
			}
			for _, turn := range page.Data {
				if turn.Status == "inProgress" {
					return ErrResyncRequired
				}
			}
		}
		reverseTurns(page.Data)
		candidate.Thread.Turns = mergeResumeTurns(page.Data, candidate.Thread.Turns)
		candidate.HistoryTruncated = candidate.HistoryTruncated || page.NextCursor != ""
	}
	if err = normalizeSnapshot(&candidate); err != nil {
		return err
	}

	// The pending read is atomically paired with its OWN watermark, not the
	// resume response sequence. Desktop may have resolved a request meanwhile.
	data, err = p.conn.client.Call(ctx, "pending/snapshot", nil)
	if err != nil {
		return err
	}
	var pending struct {
		Requests []PendingRequest `json:"requests"`
		Sequence *uint64          `json:"sequence"`
	}
	if len(data) > maxRPCBytes || json.Unmarshal(data, &pending) != nil || pending.Sequence == nil || *pending.Sequence < sequence {
		return ErrResyncRequired
	}
	watermark := *pending.Sequence
	if historySequence > watermark {
		return ErrResyncRequired
	}
	if err = s.awaitSequence(ctx, p, watermark); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != p.conn || s.projections[p.id] != p {
		return ErrStateChanged
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if p.initErr != nil || p.conn.orderFailed {
		return ErrResyncRequired
	}
	if err = validateResumeBoundary(candidate, p.buffer, sequence); err != nil {
		return err
	}
	p.snapshot = candidate
	p.stateSequence = sequence
	p.pendingSequence = watermark
	for _, request := range pending.Requests {
		if err = p.addPending(request); err != nil {
			return err
		}
	}
	if err = p.validateBounds(); err != nil {
		return err
	}
	for _, event := range p.buffer {
		if historySequence != 0 && event.sequence > sequence && event.sequence <= historySequence && event.message.ID == nil && event.message.Method != "serverRequest/resolved" {
			// A mutation was observed across the non-atomic fallback read. Even
			// if its bytes look plausible, do not merge/double-apply active data.
			return ErrResyncRequired
		}
		changed, applyErr := p.apply(event)
		if applyErr != nil {
			return applyErr
		}
		if changed && !isDelta(event.message.Method) {
			if err = p.validateBounds(); err != nil {
				return err
			}
		}
	}
	if err = p.validateBounds(); err != nil {
		return err
	}
	// Switch buffering to live reduction in this same critical section. Leaving
	// this until Read returns would drop an event arriving between the two locks.
	p.ready = true
	p.buffer = nil
	p.bufferBytes = 0
	s.changedLocked(p.id, p)
	return nil
}

// A response sequence orders transport, not the backend's snapshot capture.
// Reject observable overlap that would otherwise drop a prefix or double-apply
// text. We can accept only pre-response deltas already covered by a known item,
// and require terminal/authoritative events to agree with the resume contents.
func validateResumeBoundary(snapshot Snapshot, events []projectionEvent, boundary uint64) error {
	p := projection{snapshot: snapshot}
	type key struct {
		turn, item, method string
		index              int
	}
	deltas := make(map[key]string)
	for _, event := range events {
		m := event.message
		if event.sequence > boundary || len(m.ID) != 0 || m.Method == "serverRequest/resolved" {
			continue
		}
		var v struct {
			TurnID       string `json:"turnId"`
			ItemID       string `json:"itemId"`
			Delta        string `json:"delta"`
			Turn         Turn   `json:"turn"`
			Item         Item   `json:"item"`
			SummaryIndex int    `json:"summaryIndex"`
			ContentIndex int    `json:"contentIndex"`
		}
		if json.Unmarshal(m.Params, &v) != nil {
			return ErrResyncRequired
		}
		if isDelta(m.Method) {
			item := itemAt(p.turn(v.TurnID), v.ItemID)
			if item == nil {
				return ErrResyncRequired
			}
			index := v.SummaryIndex
			if m.Method == "item/reasoning/textDelta" {
				index = v.ContentIndex
			}
			k := key{v.TurnID, v.ItemID, m.Method, index}
			deltas[k] += v.Delta
			var content string
			switch m.Method {
			case "item/agentMessage/delta", "item/plan/delta":
				content = item.Text
			case "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
				content = item.Output
			case "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded":
				if index < 0 || index >= len(item.Summary) {
					return ErrResyncRequired
				}
				content = item.Summary[index]
			case "item/reasoning/textDelta":
				var parts []string
				if json.Unmarshal(item.Content, &parts) != nil || index < 0 || index >= len(parts) {
					return ErrResyncRequired
				}
				content = parts[index]
			}
			if !strings.Contains(content, deltas[k]) {
				return ErrResyncRequired
			}
			continue
		}
		switch m.Method {
		case "turn/started", "turn/completed":
			turn := p.turn(v.Turn.ID)
			if turn == nil || (m.Method == "turn/completed" && turn.Status != v.Turn.Status) {
				return ErrResyncRequired
			}
		case "item/started", "item/completed":
			item := itemAt(p.turn(v.TurnID), v.Item.ID)
			if item == nil || item.Type != v.Item.Type {
				return ErrResyncRequired
			}
			if m.Method == "item/completed" && (item.Text != v.Item.Text || item.Output != v.Item.Output || item.Status != v.Item.Status) {
				return ErrResyncRequired
			}
		case "thread/reverted", "thread/compacted", "thread/closed", "thread/deleted", "thread/archived":
			return ErrResyncRequired
		}
	}
	return nil
}

func reverseTurns(turns []Turn) {
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
}

func mergeResumeTurns(history, live []Turn) []Turn {
	out := make([]Turn, 0, len(history)+len(live))
	indexes := make(map[string]int, len(history))
	for _, turn := range history {
		indexes[turn.ID] = len(out)
		out = append(out, turn)
	}
	for _, turn := range live {
		if i, ok := indexes[turn.ID]; ok {
			out[i] = turn
		} else {
			indexes[turn.ID] = len(out)
			out = append(out, turn)
		}
	}
	return out
}

func (s *Service) awaitSequence(ctx context.Context, p *projection, sequence uint64) error {
	for {
		s.mu.Lock()
		if s.conn != p.conn || s.projections[p.id] != p {
			s.mu.Unlock()
			return ErrStateChanged
		}
		if p.initErr != nil || p.conn.orderFailed {
			s.mu.Unlock()
			return ErrResyncRequired
		}
		if p.conn.lastSequence >= sequence {
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

func (s *Service) observe(c *connection) {
	for {
		select {
		case <-c.ctx.Done():
			return
		case event, ok := <-c.client.Events():
			if !ok {
				s.disconnected(c)
				return
			}
			s.acceptEvent(c, event)
		case <-c.client.Done():
			s.disconnected(c)
			return
		}
	}
}

func (s *Service) disconnected(c *connection) {
	s.mu.Lock()
	if s.conn == c {
		s.conn = nil
		for _, p := range s.projections {
			p.cancel()
		}
		s.projections = make(map[string]*projection)
		s.connectionChangedLocked()
	}
	s.mu.Unlock()
	c.cancel()
	c.client.Close()
}

func (s *Service) acceptEvent(c *connection, event bridge.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != c {
		return
	}
	if event.Sequence == 0 || (c.lastSequence != 0 && event.Sequence != c.lastSequence+1) || (c.lastSequence == 0 && event.Sequence > c.initialFloor+1) {
		c.orderFailed = true
		for _, p := range s.projections {
			s.failProjectionLocked(p, "event ordering was lost; reconnect Desktop attachment")
		}
	}
	if event.Sequence > c.lastSequence {
		c.lastSequence = event.Sequence
	}
	close(c.progress)
	c.progress = make(chan struct{})
	if c.orderFailed {
		return
	}
	var message eventMessage
	if len(event.Message) > bridge.MaxFrameBytes || !utf8.Valid(event.Message) || json.Unmarshal(event.Message, &message) != nil {
		for _, p := range s.projections {
			s.failProjectionLocked(p, "invalid event received")
		}
		return
	}
	if message.Method == "" { // RPC responses are ordering fences, not state deltas.
		return
	}
	var params struct {
		ThreadID string `json:"threadId"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(message.Params, &params) != nil {
		for _, p := range s.projections {
			s.failProjectionLocked(p, "invalid event parameters received")
		}
		return
	}
	message.threadID = params.ThreadID
	if message.threadID == "" {
		message.threadID = params.Thread.ID
	}
	// Directory discovery is independent of selected transcript projections. A
	// metadata hint does not hydrate a thread, enumerate the catalogue, or turn
	// a content delta into structural invalidation.
	if len(message.ID) == 0 && isCatalogueEvent(message.Method) {
		s.catalogueChangedLocked()
	}
	e := projectionEvent{sequence: event.Sequence, size: len(event.Message), message: message}
	if message.Method == "serverRequest/resolved" {
		// Older servers omit threadId. JSON request IDs are global per attachment;
		// resolve across initialized AND initializing projections, after their own fence.
		for _, p := range s.projections {
			s.acceptProjectionEventLocked(p, e)
		}
	} else if p := s.projections[message.threadID]; p != nil {
		s.acceptProjectionEventLocked(p, e)
	}
}

func isCatalogueEvent(method string) bool {
	switch method {
	case "thread/started", "thread/name/updated", "thread/status/changed", "thread/settings/updated", "thread/archived", "thread/unarchived", "thread/closed", "thread/deleted", "thread/project/updated", "turn/started", "turn/completed":
		return true
	default:
		return false
	}
}

func (s *Service) acceptProjectionEventLocked(p *projection, event projectionEvent) {
	if p.initErr != nil || p.snapshot.ResyncRequired {
		return
	}
	if !p.ready {
		if len(p.buffer) >= maxBufferedEvents || event.size > maxBufferedBytes-p.bufferBytes {
			s.failProjectionLocked(p, "initial event buffer exceeded its limit")
			return
		}
		p.buffer = append(p.buffer, event)
		p.bufferBytes += event.size
		return
	}
	changed, err := p.apply(event)
	if err != nil {
		s.failProjectionLocked(p, "live reducer rejected "+diagnosticEventMethod(event.message.Method))
		return
	}
	// apply treats any ID-bearing message as a pending request, even if its
	// method resembles a delta. Only successfully reduced, allowlisted
	// notifications can avoid invalidating the browser's action state.
	contentOnly := len(event.message.ID) == 0 && isDelta(event.message.Method)
	if changed && !contentOnly && p.validateBounds() != nil {
		s.failProjectionLocked(p, "live bounds exceeded after "+diagnosticEventMethod(event.message.Method))
		return
	}
	if changed {
		s.publishChangeLocked(p.id, p, contentOnly)
	}
}

// Diagnostic labels are fixed protocol names only, never payloads, arbitrary
// methods, IDs or user text. They distinguish reducer failures from data bounds.
func diagnosticEventMethod(method string) string {
	if isDelta(method) || isCatalogueEvent(method) {
		return method
	}
	switch method {
	case "item/started", "item/completed", "turn/plan/updated", "turn/diff/updated", "item/fileChange/patchUpdated", "item/mcpToolCall/progress", "thread/goal/cleared", "thread/goal/updated", "thread/tokenUsage/updated", "thread/reverted", "thread/compacted", "serverRequest/resolved", "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/commandExecution/terminalInteraction", "item/autoApprovalReview/started", "item/autoApprovalReview/completed", "turn/moderationMetadata", "mcpServer/startupStatus/updated":
		return method
	default:
		return "unrecognized event"
	}
}

func (s *Service) failProjectionLocked(p *projection, reason string) {
	if !p.ready {
		p.initErr = ErrResyncRequired
		p.buffer = nil
		p.bufferBytes = 0
		p.cancel()
		return
	}
	if p.snapshot.ResyncRequired {
		return
	}
	// Discard possibly partial/oversized data rather than expose stale text as
	// current state. The explicit resync marker is safe for GET and disables work.
	thread := p.snapshot.Thread
	thread.Turns = []Turn{}
	p.snapshot = Snapshot{Thread: thread, Pending: []PendingRequest{}, HistoryTruncated: true, ResyncRequired: true, ResyncReason: reason,
		ReadOnly: p.snapshot.ReadOnly, ReadOnlyReason: p.snapshot.ReadOnlyReason}
	p.pending = make(map[string]*pendingEntry)
	p.buffer = nil
	p.stateBytes = 0
	s.changedLocked(p.id, p)
}

func (p *projection) pendingSnapshot() []PendingRequest {
	entries := make([]*pendingEntry, 0, len(p.pending))
	for _, entry := range p.pending {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].order < entries[j].order })
	out := make([]PendingRequest, 0, len(entries))
	for _, entry := range entries {
		request := copyPending(entry.request)
		request.DesktopOnly = !p.phoneApprovalAllowed(request)
		out = append(out, request)
	}
	return out
}

func (p *projection) phoneApprovalAllowed(request PendingRequest) bool {
	if p.snapshot.ReadOnly || !acceptsInput(p.snapshot.Thread.CanAcceptDirectInput) || request.DesktopOnly || !attachment.PhoneApprovalSupported(request.Method, request.Params) {
		return false
	}
	if request.Method == "item/fileChange/requestApproval" {
		var params struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
		}
		if json.Unmarshal(request.Params, &params) != nil {
			return false
		}
		item := itemAt(p.turn(params.TurnID), params.ItemID)
		var changes []json.RawMessage
		return item != nil && item.Type == "fileChange" && json.Unmarshal(item.Changes, &changes) == nil && len(changes) > 0
	}
	return true
}

func (p *projection) addPending(request PendingRequest) error {
	var params struct {
		ThreadID string `json:"threadId"`
	}
	if json.Unmarshal(request.Params, &params) != nil {
		return ErrResyncRequired
	}
	if params.ThreadID != p.id {
		return nil
	}
	if len(request.ID) > maxIDBytes || len(request.Params) > maxPendingBytes || len(request.Method) > 256 {
		return ErrResyncRequired
	}
	key, err := bridge.RequestIDKey(request.ID)
	if err != nil {
		return ErrResyncRequired
	}
	if _, ok := p.pending[key]; ok {
		return nil // retransmission does not replace the Mux arbitration entry
	}
	if len(p.pending) >= maxPending {
		return ErrResyncRequired
	}
	request.DesktopOnly = !attachment.PhoneApprovalSupported(request.Method, request.Params)
	p.pendingOrder++
	p.pending[key] = &pendingEntry{request: copyPending(request), order: p.pendingOrder}
	return nil
}

func (p *projection) apply(event projectionEvent) (bool, error) {
	m := event.message
	if m.Method == "serverRequest/resolved" {
		if event.sequence <= p.pendingSequence {
			return false, nil
		}
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(m.Params, &params) != nil || len(params.RequestID) > maxIDBytes {
			return false, ErrResyncRequired
		}
		key, err := bridge.RequestIDKey(params.RequestID)
		if err != nil {
			return false, ErrResyncRequired
		}
		_, exists := p.pending[key]
		delete(p.pending, key)
		p.pendingSequence = event.sequence
		return exists, nil
	}
	if len(m.ID) != 0 {
		if event.sequence <= p.pendingSequence {
			return false, nil
		}
		before := len(p.pending)
		err := p.addPending(PendingRequest{ID: m.ID, Method: m.Method, Params: m.Params})
		p.pendingSequence = event.sequence
		return len(p.pending) != before, err
	}
	if event.sequence <= p.stateSequence {
		return false, nil
	}
	p.stateSequence = event.sequence
	return p.reduce(m.Method, m.Params)
}

func normalizeSnapshot(snapshot *Snapshot) error {
	thread := &snapshot.Thread
	// Archived/explicitly noninteractive views never inherit an upstream true.
	// Unknown remains null; writes require a live, affirmative capability.
	if snapshot.ReadOnly && thread.CanAcceptDirectInput != nil {
		value := false
		thread.CanAcceptDirectInput = &value
	}
	if !acceptsInput(thread.CanAcceptDirectInput) {
		snapshot.ReadOnly = true
	}
	if !validID(thread.ID) || len(thread.Name) > maxTextBytes || len(thread.CWD) > maxTextBytes || len(thread.Model) > 512 || len(thread.Status.Type) > 64 || len(thread.Status.ActiveFlags) > 16 {
		return ErrResyncRequired
	}
	if len(thread.Turns) > maxTurns {
		for _, turn := range thread.Turns[:len(thread.Turns)-maxTurns] {
			if turn.Status == "inProgress" && !snapshot.ReadOnly {
				return ErrResyncRequired
			}
		}
		thread.Turns = append([]Turn(nil), thread.Turns[len(thread.Turns)-maxTurns:]...)
		snapshot.HistoryTruncated = true
	}
	seen := make(map[string]bool)
	active := 0
	for i := range thread.Turns {
		turn := &thread.Turns[i]
		if !validID(turn.ID) || seen[turn.ID] || !validTurnStatus(turn.Status) {
			return ErrResyncRequired
		}
		seen[turn.ID] = true
		if turn.Status == "inProgress" {
			active++
		}
		if turn.ItemsView != "" && turn.ItemsView != "full" {
			snapshot.HistoryTruncated = true
			if turn.Status == "inProgress" && !snapshot.ReadOnly {
				return ErrResyncRequired
			}
		}
		if len(turn.Items) > maxItemsPerTurn {
			return ErrResyncRequired
		}
		items := make(map[string]bool)
		for _, item := range turn.Items {
			if items[item.ID] || validateItem(item) != nil {
				return ErrResyncRequired
			}
			items[item.ID] = true
		}
	}
	if !snapshot.ReadOnly && (active > 1 || (thread.Status.Type == "active" && active == 0)) {
		return ErrResyncRequired
	}
	snapshot.ActiveTurnID = activeTurn(*thread)
	if snapshot.ReadOnly {
		snapshot.ActiveTurnID = ""
	}
	if thread.Turns == nil {
		thread.Turns = []Turn{}
	}
	p := projection{snapshot: *snapshot}
	return p.validateBounds()
}

func validTurnStatus(status string) bool {
	return status == "inProgress" || status == "completed" || status == "failed" || status == "interrupted"
}

func validateItem(item Item) error {
	if !validID(item.ID) || item.Type == "" || len(item.Type) > 128 || len(item.Status) > 128 || len(item.Summary) > 256 {
		return ErrResyncRequired
	}
	for _, text := range []string{item.Text, item.Output, item.Command, item.Name, item.Progress, item.Tool, item.Phase} {
		if len(text) > maxTextBytes || !utf8.ValidString(text) {
			return ErrResyncRequired
		}
	}
	for _, text := range item.Summary {
		if len(text) > maxTextBytes {
			return ErrResyncRequired
		}
	}
	for _, raw := range []json.RawMessage{item.Content, item.Changes, item.Arguments, item.Result, item.Error} {
		if len(raw) > maxTextBytes {
			return ErrResyncRequired
		}
	}
	data, err := json.Marshal(item)
	if err != nil || len(data) > maxItemBytes {
		return ErrResyncRequired
	}
	return nil
}

func (p *projection) validateBounds() error {
	if len(p.snapshot.Thread.Turns) > maxTurns || len(p.pending) > maxPending {
		return ErrResyncRequired
	}
	itemBytes := make(map[string]int)
	for _, turn := range p.snapshot.Thread.Turns {
		if len(turn.Items) > maxItemsPerTurn || len(turn.Diff) > maxTextBytes || len(turn.Explanation) > maxTextBytes || len(turn.Plan) > maxTextBytes || len(turn.Error) > maxTextBytes {
			return ErrResyncRequired
		}
		for _, item := range turn.Items {
			if err := validateItem(item); err != nil {
				return err
			}
			encoded, _ := json.Marshal(item)
			itemBytes[itemBudgetKey(turn.ID, item.ID)] = len(encoded)
		}
	}
	data, err := json.Marshal(p.snapshot.Thread)
	if err != nil || len(data) > maxProjectionBytes {
		return ErrResyncRequired
	}
	pendingBytes := 0
	for _, entry := range p.pending {
		pendingBytes += len(entry.request.ID) + len(entry.request.Method) + len(entry.request.Params)
	}
	if pendingBytes > maxPendingBytes || len(data)+pendingBytes > maxProjectionBytes {
		return ErrResyncRequired
	}
	p.stateBytes = len(data) + pendingBytes
	p.itemBytes = itemBytes
	return nil
}

func itemBudgetKey(turnID, itemID string) string {
	return strconv.Itoa(len(turnID)) + ":" + turnID + itemID
}

func isDelta(method string) bool {
	switch method {
	case "item/agentMessage/delta", "item/plan/delta", "item/commandExecution/outputDelta", "item/fileChange/outputDelta", "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "item/reasoning/textDelta":
		return true
	default:
		return false
	}
}

func (p *projection) reserveDelta(turnID, itemID, delta string) error {
	// Only encode the new bytes. JSON-escaping may grow text more than len(delta),
	// so charge its encoded size plus a small field/array overhead. Budgets are
	// conservative until the next structural event recalculates exact sizes.
	encoded, err := json.Marshal(delta)
	if err != nil {
		return ErrResyncRequired
	}
	growth := len(encoded) + 32
	key := itemBudgetKey(turnID, itemID)
	itemSize, ok := p.itemBytes[key]
	if !ok || growth > maxProjectionBytes-p.stateBytes || growth > maxItemBytes-itemSize {
		return ErrResyncRequired
	}
	p.stateBytes += growth
	p.itemBytes[key] += growth
	return nil
}

func (p *projection) turn(id string) *Turn {
	for i := range p.snapshot.Thread.Turns {
		if p.snapshot.Thread.Turns[i].ID == id {
			return &p.snapshot.Thread.Turns[i]
		}
	}
	return nil
}

func itemAt(turn *Turn, id string) *Item {
	if turn == nil {
		return nil
	}
	for i := range turn.Items {
		if turn.Items[i].ID == id {
			return &turn.Items[i]
		}
	}
	return nil
}

func (p *projection) reduce(method string, raw json.RawMessage) (bool, error) {
	if len(raw) > maxProjectionBytes {
		return false, ErrResyncRequired
	}
	if method == "mcpServer/startupStatus/updated" {
		// Thread-scoped MCP connection metadata has a string status, unlike
		// thread/status/changed. It does not mutate transcript or action state.
		// ID-bearing requests were already handled by apply, before this path.
		return false, nil
	}
	var params struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ItemID   string `json:"itemId"`
		Turn     Turn   `json:"turn"`
		Item     Item   `json:"item"`
		Delta    string `json:"delta"`
		Status   struct {
			Type        string   `json:"type"`
			ActiveFlags []string `json:"activeFlags"`
		} `json:"status"`
		ThreadName     string          `json:"threadName"`
		SummaryIndex   *int            `json:"summaryIndex"`
		ContentIndex   *int            `json:"contentIndex"`
		Changes        json.RawMessage `json:"changes"`
		Plan           json.RawMessage `json:"plan"`
		Explanation    string          `json:"explanation"`
		Diff           string          `json:"diff"`
		Message        string          `json:"message"`
		ThreadSettings struct {
			Model string `json:"model"`
			CWD   string `json:"cwd"`
		} `json:"threadSettings"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return false, ErrResyncRequired
	}
	switch method {
	case "thread/status/changed":
		if params.Status.Type == "" || len(params.Status.Type) > 64 || len(params.Status.ActiveFlags) > 16 {
			return false, ErrResyncRequired
		}
		p.snapshot.Thread.Status.Type = params.Status.Type
		p.snapshot.Thread.Status.ActiveFlags = append([]string(nil), params.Status.ActiveFlags...)
		return true, nil
	case "thread/name/updated":
		if len(params.ThreadName) > maxTextBytes {
			return false, ErrResyncRequired
		}
		p.snapshot.Thread.Name = params.ThreadName
		return true, nil
	case "thread/settings/updated":
		if len(params.ThreadSettings.Model) > 512 || len(params.ThreadSettings.CWD) > maxTextBytes {
			return false, ErrResyncRequired
		}
		p.snapshot.Thread.Model = params.ThreadSettings.Model
		p.snapshot.Thread.CWD = params.ThreadSettings.CWD
		return true, nil
	case "thread/closed", "thread/deleted", "thread/archived", "thread/reverted", "thread/compacted":
		return false, ErrResyncRequired
	case "turn/started", "turn/completed":
		turn := params.Turn
		if !validID(turn.ID) || !validTurnStatus(turn.Status) || len(turn.Items) > maxItemsPerTurn {
			return false, ErrResyncRequired
		}
		if (method == "turn/started") != (turn.Status == "inProgress") {
			return false, ErrResyncRequired
		}
		current := p.turn(turn.ID)
		if current == nil {
			if method != "turn/started" {
				return false, ErrResyncRequired
			}
			if active := activeTurn(p.snapshot.Thread); active != "" && active != turn.ID {
				return false, ErrResyncRequired
			}
			if len(p.snapshot.Thread.Turns) == maxTurns {
				if p.snapshot.Thread.Turns[0].Status == "inProgress" {
					return false, ErrResyncRequired
				}
				p.snapshot.Thread.Turns = append([]Turn(nil), p.snapshot.Thread.Turns[1:]...)
				p.snapshot.HistoryTruncated = true
			}
			p.snapshot.Thread.Turns = append(p.snapshot.Thread.Turns, turn)
		} else {
			if current.Status != "inProgress" && method == "turn/started" {
				return false, ErrResyncRequired
			}
			// Lifecycle notifications often carry no items. Never erase already
			// streamed data on turn/completed just because its list is empty.
			current.Status = turn.Status
			current.Error = cloneRaw(turn.Error)
			if len(turn.Items) != 0 {
				for _, item := range turn.Items {
					if old := itemAt(current, item.ID); old != nil {
						*old = copyItem(item)
					} else {
						current.Items = append(current.Items, copyItem(item))
					}
				}
			}
		}
		p.startPending = false
		if turn.Status == "inProgress" {
			p.snapshot.Thread.Status.Type = "active"
		} else if activeTurn(p.snapshot.Thread) == "" {
			p.snapshot.Thread.Status.Type = "idle"
			p.snapshot.Thread.Status.ActiveFlags = nil
		}
		return true, nil
	case "item/started", "item/completed":
		turn := p.turn(params.TurnID)
		if turn == nil || validateItem(params.Item) != nil {
			return false, ErrResyncRequired
		}
		if old := itemAt(turn, params.Item.ID); old != nil {
			*old = copyItem(params.Item)
		} else {
			if len(turn.Items) >= maxItemsPerTurn {
				return false, ErrResyncRequired
			}
			turn.Items = append(turn.Items, copyItem(params.Item))
		}
		return true, nil
	case "turn/plan/updated", "turn/diff/updated":
		turn := p.turn(params.TurnID)
		if turn == nil {
			return false, ErrResyncRequired
		}
		if method == "turn/plan/updated" {
			turn.Plan = cloneRaw(params.Plan)
			turn.Explanation = params.Explanation
		} else {
			turn.Diff = params.Diff
		}
		return true, nil
	case "item/agentMessage/delta", "item/plan/delta", "item/commandExecution/outputDelta", "item/fileChange/outputDelta", "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "item/reasoning/textDelta", "item/fileChange/patchUpdated", "item/mcpToolCall/progress":
		item := itemAt(p.turn(params.TurnID), params.ItemID)
		if item == nil {
			return false, ErrResyncRequired
		}
		if len(params.Delta) > maxTextBytes {
			return false, ErrResyncRequired
		}
		if isDelta(method) {
			if err := p.reserveDelta(params.TurnID, params.ItemID, params.Delta); err != nil {
				return false, err
			}
		}
		switch method {
		case "item/agentMessage/delta", "item/plan/delta":
			want := "agentMessage"
			if method == "item/plan/delta" {
				want = "plan"
			}
			if item.Type != want || len(params.Delta) > maxTextBytes-len(item.Text) {
				return false, ErrResyncRequired
			}
			item.Text += params.Delta
		case "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
			want := "commandExecution"
			if method == "item/fileChange/outputDelta" {
				want = "fileChange"
			}
			if item.Type != want || len(params.Delta) > maxTextBytes-len(item.Output) {
				return false, ErrResyncRequired
			}
			item.Output += params.Delta
		case "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded":
			if item.Type != "reasoning" || params.SummaryIndex == nil || *params.SummaryIndex < 0 || *params.SummaryIndex > len(item.Summary) || *params.SummaryIndex >= 256 {
				return false, ErrResyncRequired
			}
			i := *params.SummaryIndex
			if i == len(item.Summary) {
				item.Summary = append(item.Summary, "")
			}
			if len(params.Delta) > maxTextBytes-len(item.Summary[i]) {
				return false, ErrResyncRequired
			}
			item.Summary[i] += params.Delta
		case "item/reasoning/textDelta":
			if item.Type != "reasoning" || params.ContentIndex == nil {
				return false, ErrResyncRequired
			}
			var parts []string
			if len(item.Content) != 0 && json.Unmarshal(item.Content, &parts) != nil {
				return false, ErrResyncRequired
			}
			i := *params.ContentIndex
			if i < 0 || i > len(parts) || i >= 256 {
				return false, ErrResyncRequired
			}
			if i == len(parts) {
				parts = append(parts, "")
			}
			if len(params.Delta) > maxTextBytes-len(parts[i]) {
				return false, ErrResyncRequired
			}
			parts[i] += params.Delta
			item.Content, _ = json.Marshal(parts)
		case "item/fileChange/patchUpdated":
			if item.Type != "fileChange" || len(params.Changes) == 0 {
				return false, ErrResyncRequired
			}
			item.Changes = cloneRaw(params.Changes)
		case "item/mcpToolCall/progress":
			if item.Type != "mcpToolCall" {
				return false, ErrResyncRequired
			}
			item.Progress = params.Message
		}
		if isDelta(method) {
			if len(item.Content) > maxTextBytes {
				return false, ErrResyncRequired
			}
			return true, nil
		}
		return true, validateItem(*item)
	default:
		// Notifications which do not change the projected conversation are safe
		// to ignore. Unknown item/turn mutations are not: make uncertainty visible.
		if strings.HasPrefix(method, "item/") || strings.HasPrefix(method, "turn/") {
			switch method {
			case "item/commandExecution/terminalInteraction", "item/autoApprovalReview/started", "item/autoApprovalReview/completed", "turn/moderationMetadata":
				return false, nil
			default:
				return false, ErrResyncRequired
			}
		}
		return false, nil
	}
}
