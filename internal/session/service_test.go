package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/bridge"
)

type fakeCall struct {
	method string
	params json.RawMessage
}

type fakeClient struct {
	mu       sync.Mutex
	eventMu  sync.Mutex
	events   chan bridge.Event
	done     chan struct{}
	once     sync.Once
	sequence uint64
	calls    []fakeCall
	threads  map[string]Thread
	pending  []PendingRequest
	hook     func(context.Context, string, json.RawMessage) (json.RawMessage, error, bool)
	pid      int
}

func newFake() *fakeClient {
	return &fakeClient{events: make(chan bridge.Event, 4096), done: make(chan struct{}), sequence: 10,
		threads: map[string]Thread{"a": testThread("a", false)}, pid: 101}
}

func testThread(id string, active bool) Thread {
	thread := Thread{ID: id, Name: "Thread " + id, CWD: "C:/test", CanAcceptDirectInput: boolPtr(true), Model: "test"}
	thread.Status.Type = "idle"
	status := "completed"
	if active {
		status = "inProgress"
		thread.Status.Type = "active"
	}
	thread.Turns = []Turn{{ID: "turn-1", Status: status, Items: []Item{{ID: "item-1", Type: "agentMessage", Text: "base"}}}}
	return thread
}

func boolPtr(value bool) *bool { return &value }

func rawJSON(v any) json.RawMessage                   { data, _ := json.Marshal(v); return data }
func (f *fakeClient) Subscribe(context.Context) error { return nil }
func (f *fakeClient) SubscriptionSequence() uint64    { return 10 }
func (f *fakeClient) Events() <-chan bridge.Event     { return f.events }
func (f *fakeClient) Done() <-chan struct{}           { return f.done }
func (f *fakeClient) Metadata() attachment.Metadata   { return attachment.Metadata{EnginePID: f.pid} }
func (f *fakeClient) Close()                          { f.once.Do(func() { close(f.done) }) }

func (f *fakeClient) emitMessage(message any) uint64 {
	return f.emitRaw(rawJSON(message))
}
func (f *fakeClient) emitRaw(message json.RawMessage) uint64 {
	f.eventMu.Lock()
	defer f.eventMu.Unlock()
	f.mu.Lock()
	f.sequence++
	sequence := f.sequence
	f.mu.Unlock()
	f.events <- bridge.Event{Sequence: sequence, Message: message}
	return sequence
}
func (f *fakeClient) event(method string, params any) uint64 {
	return f.emitMessage(map[string]any{"method": method, "params": params})
}
func (f *fakeClient) sequenced(result any) json.RawMessage {
	sequence := f.emitMessage(map[string]any{"id": "fake-response", "result": result})
	return rawJSON(map[string]any{"result": result, "sequence": sequence})
}
func (f *fakeClient) setHook(hook func(context.Context, string, json.RawMessage) (json.RawMessage, error, bool)) {
	f.mu.Lock()
	f.hook = hook
	f.mu.Unlock()
}
func (f *fakeClient) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method {
			n++
		}
	}
	return n
}
func (f *fakeClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	raw := rawJSON(params)
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{method, raw})
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		if result, err, handled := hook(ctx, method, raw); handled {
			return result, err
		}
	}
	var input struct {
		ThreadID       string          `json:"threadId"`
		ExpectedTurnID string          `json:"expectedTurnId"`
		TurnID         string          `json:"turnId"`
		ID             json.RawMessage `json:"id"`
		Cursor         string          `json:"cursor"`
		Search         string          `json:"searchTerm"`
		Limit          int             `json:"limit"`
		ReadOnly       bool            `json:"readOnly"`
	}
	_ = json.Unmarshal(raw, &input)
	switch method {
	case "status":
		return rawJSON(map[string]bool{"ready": true}), nil
	case "watch", "unwatch":
		return rawJSON(map[string]bool{"ok": true}), nil
	case "thread/list", "thread/loaded/list":
		f.mu.Lock()
		ids := make([]string, 0, len(f.threads))
		for id, thread := range f.threads {
			if input.Search == "" || strings.Contains(strings.ToLower(thread.Name), strings.ToLower(input.Search)) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		start, _ := strconv.Atoi(input.Cursor)
		if start > len(ids) {
			start = len(ids)
		}
		end := start + input.Limit
		if input.Limit <= 0 || end > len(ids) {
			end = len(ids)
		}
		next := ""
		if end < len(ids) {
			next = strconv.Itoa(end)
		}
		ids = ids[start:end]
		if method == "thread/loaded/list" {
			f.mu.Unlock()
			return rawJSON(map[string]any{"data": ids, "nextCursor": next}), nil
		}
		threads := make([]Thread, 0, len(ids))
		for _, id := range ids {
			thread := copySnapshot(Snapshot{Thread: f.threads[id]}).Thread
			thread.Turns = nil
			threads = append(threads, thread)
		}
		f.mu.Unlock()
		return rawJSON(map[string]any{"data": threads, "nextCursor": next}), nil
	case "thread/read":
		f.mu.Lock()
		thread := f.threads[input.ThreadID]
		f.mu.Unlock()
		return rawJSON(map[string]any{"thread": thread}), nil
	case "snapshot":
		f.mu.Lock()
		thread := copySnapshot(Snapshot{Thread: f.threads[input.ThreadID]}).Thread
		f.mu.Unlock()
		if input.ReadOnly {
			thread.Turns = nil
		}
		return f.sequenced(map[string]any{"thread": thread}), nil
	case "history":
		f.mu.Lock()
		turns := copySnapshot(Snapshot{Thread: f.threads[input.ThreadID]}).Thread.Turns
		f.mu.Unlock()
		reverseTurns(turns)
		return f.sequenced(map[string]any{"data": turns}), nil
	case "pending/snapshot":
		f.mu.Lock()
		sequence := f.sequence
		requests := append([]PendingRequest{}, f.pending...)
		f.mu.Unlock()
		return rawJSON(map[string]any{"requests": requests, "sequence": sequence}), nil
	case "barrier":
		f.mu.Lock()
		sequence := f.sequence
		f.mu.Unlock()
		return rawJSON(map[string]any{"sequence": sequence}), nil
	case "respond":
		f.event("serverRequest/resolved", map[string]any{"requestId": input.ID})
		return rawJSON(map[string]bool{"ok": true}), nil
	case "turn/start":
		turn := Turn{ID: "turn-new", Status: "inProgress", Items: []Item{}}
		f.event("turn/started", map[string]any{"threadId": input.ThreadID, "turn": turn})
		return rawJSON(map[string]any{"turn": turn}), nil
	case "turn/steer":
		return rawJSON(map[string]any{"turnId": input.ExpectedTurnID}), nil
	case "turn/interrupt":
		f.event("turn/completed", map[string]any{"threadId": input.ThreadID, "turn": Turn{ID: input.TurnID, Status: "interrupted", Items: []Item{}}})
		return rawJSON(map[string]bool{"ok": true}), nil
	default:
		return nil, fmt.Errorf("unexpected fake method %s", method)
	}
}

func connectedService(t *testing.T, f *fakeClient) *Service {
	t.Helper()
	s := New(t.TempDir())
	s.discover = func(context.Context, string) (attachmentClient, error) { return f, nil }
	if err := s.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func readError(s *Service, ctx context.Context, id string, options ReadOptions) error {
	_, err := s.Read(ctx, id, options)
	return err
}
func openThread(t *testing.T, s *Service, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := readError(s, ctx, id, ReadOptions{}); err != nil {
		t.Fatal(err)
	}
}
func snapshot(t *testing.T, s *Service, id string) Snapshot {
	t.Helper()
	value, err := s.Read(context.Background(), id, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func awaitSequence(t *testing.T, s *Service, sequence uint64) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		c := s.conn
		if c == nil {
			s.mu.Unlock()
			t.Fatal("connection lost")
		}
		if c.lastSequence >= sequence {
			s.mu.Unlock()
			return
		}
		progress := c.progress
		s.mu.Unlock()
		select {
		case <-progress:
		case <-deadline.C:
			t.Fatal("event observer did not reach sequence")
		}
	}
}
func request(id json.RawMessage, thread string) PendingRequest {
	return PendingRequest{ID: id, Method: "item/commandExecution/requestApproval", Params: rawJSON(map[string]any{
		"threadId": thread, "turnId": "turn-1", "itemId": "item-1", "command": "git status", "cwd": "C:/test", "reason": "inspect files",
	})}
}

func TestAbsentProjectionCommandsFailBeforeRPC(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Read(context.Background(), "missing", ReadOptions{}); !errors.Is(err, ErrNoAttachment) {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "missing", "hi", "start", ""); !errors.Is(err, ErrStateChanged) {
		t.Fatal(err)
	}
	if _, err := s.Interrupt(context.Background(), "missing", "turn"); !errors.Is(err, ErrStateChanged) {
		t.Fatal(err)
	}
}
func TestActiveTurnUsesLatestInProgress(t *testing.T) {
	thread := Thread{Turns: []Turn{{ID: "old", Status: "completed"}, {ID: "active", Status: "inProgress"}}}
	if got := activeTurn(thread); got != "active" {
		t.Fatal(got)
	}
	thread.Turns[1].Status = "interrupted"
	if activeTurn(thread) != "" {
		t.Fatal("interrupted turn treated as active")
	}
}
func TestCatalogueIndependentOfProjectionMembership(t *testing.T) {
	f := newFake()
	f.threads["b"] = testThread("b", false)
	s := connectedService(t, f)
	openThread(t, s, "b")
	page, err := s.ListThreads(context.Background(), ListOptions{})
	if err != nil || len(page.Threads) != 2 || s.ProjectionGeneration("a") != 0 {
		t.Fatal(page, err)
	}
	if f.count("thread/read") != 1 || f.count("snapshot") != 1 {
		t.Fatal("listing hydrated transcripts", f.calls)
	}
}
func TestSlowSubscriberIsClosed(t *testing.T) {
	s := New(t.TempDir())
	events, cancel := s.Subscribe()
	defer cancel()
	s.mu.Lock()
	for i := 0; i < 40; i++ {
		s.broadcastLocked(Change{Revision: uint64(i)})
	}
	s.mu.Unlock()
	for range events {
	}
}

func TestChangesOnlyAllowValidatedContentNotifications(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	items := []Item{
		{ID: "item-1", Type: "agentMessage", Text: "base"},
		{ID: "plan", Type: "plan"},
		{ID: "cmd", Type: "commandExecution", Command: "git status"},
		{ID: "file", Type: "fileChange", Changes: rawJSON([]map[string]string{{"path": "before.txt"}})},
		{ID: "reason", Type: "reasoning"},
		{ID: "tool", Type: "mcpToolCall"},
	}
	thread := f.threads["a"]
	thread.Turns[0].Items = items
	f.threads["a"] = thread
	s := connectedService(t, f)
	changes, cancel := s.Subscribe()
	defer cancel()
	var previous uint64
	check := func(contentOnly bool) Change {
		t.Helper()
		for {
			select {
			case change, ok := <-changes:
				if ok && change.CatalogueChanged && change.ThreadID == "" {
					continue
				}
				if !ok || change.ThreadID != "a" || change.Revision <= previous || change.ContentOnly != contentOnly {
					t.Fatalf("unexpected change %+v, want contentOnly=%v after revision %d", change, contentOnly, previous)
				}
				previous = change.Revision
				return change
			case <-time.After(3 * time.Second):
				t.Fatal("missing change")
				return Change{}
			}
		}
	}
	openThread(t, s, "a")
	check(false) // Initialization establishes action state, never merely content.
	cases := []struct {
		method      string
		itemID      string
		extra       map[string]any
		contentOnly bool
	}{
		{"item/agentMessage/delta", "item-1", map[string]any{"delta": " more"}, true},
		{"item/plan/delta", "plan", map[string]any{"delta": " plan"}, true},
		{"item/commandExecution/outputDelta", "cmd", map[string]any{"delta": " output"}, true},
		{"item/fileChange/outputDelta", "file", map[string]any{"delta": " output"}, true},
		{"item/reasoning/summaryPartAdded", "reason", map[string]any{"summaryIndex": 0}, true},
		{"item/reasoning/summaryTextDelta", "reason", map[string]any{"summaryIndex": 0, "delta": " summary"}, true},
		{"item/reasoning/textDelta", "reason", map[string]any{"contentIndex": 0, "delta": " reason"}, true},
		{"item/fileChange/patchUpdated", "file", map[string]any{"changes": []map[string]string{{"path": "after.txt"}}}, false},
		{"item/mcpToolCall/progress", "tool", map[string]any{"message": "progress"}, false},
		{"item/started", "", map[string]any{"item": Item{ID: "second", Type: "agentMessage", Status: "inProgress"}}, false},
		{"item/completed", "", map[string]any{"item": Item{ID: "second", Type: "agentMessage", Text: "done"}}, false},
		{"thread/status/changed", "", map[string]any{"status": map[string]any{"type": "active"}}, false},
		{"turn/plan/updated", "", map[string]any{"plan": []map[string]string{{"step": "verify", "status": "inProgress"}}}, false},
		{"turn/diff/updated", "", map[string]any{"diff": "changed"}, false},
		{"thread/name/updated", "", map[string]any{"threadName": "renamed"}, false},
		{"thread/settings/updated", "", map[string]any{"threadSettings": map[string]string{"cwd": "C:/test", "model": "test"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			params := map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": tc.itemID}
			for key, value := range tc.extra {
				params[key] = value
			}
			awaitSequence(t, s, f.event(tc.method, params))
			change := check(tc.contentOnly)
			got := snapshot(t, s, "a")
			if got.ResyncRequired || got.Revision != change.Revision || got.ActiveTurnID != "turn-1" {
				t.Fatal(got)
			}
		})
	}
	// An ID-bearing delta-looking method is a server request, not streamed text.
	for i, method := range []string{"item/agentMessage/delta", "item/commandExecution/requestApproval"} {
		id := fmt.Sprintf("request-%d", i)
		params := map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "cmd", "command": "git status", "cwd": "C:/test"}
		awaitSequence(t, s, f.emitMessage(map[string]any{"id": id, "method": method, "params": params}))
		check(false)
		if len(snapshot(t, s, "a").Pending) != 1 {
			t.Fatal("pending request not projected")
		}
		awaitSequence(t, s, f.event("serverRequest/resolved", map[string]any{"requestId": id}))
		check(false)
	}
	awaitSequence(t, s, f.event("turn/completed", map[string]any{"threadId": "a", "turn": Turn{ID: "turn-1", Status: "completed"}}))
	check(false)
	awaitSequence(t, s, f.event("turn/started", map[string]any{"threadId": "a", "turn": Turn{ID: "turn-2", Status: "inProgress"}}))
	check(false)
	s.ClearProjections()
	check(false)
}

func TestContentFailureAlwaysPublishesStructuralResync(t *testing.T) {
	for _, failure := range []string{"missing-item", "wrong-type", "overflow", "unknown", "gap"} {
		t.Run(failure, func(t *testing.T) {
			f := newFake()
			f.threads["a"] = testThread("a", true)
			s := connectedService(t, f)
			openThread(t, s, "a")
			changes, cancel := s.Subscribe()
			defer cancel()
			method := "item/agentMessage/delta"
			params := map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "item-1", "delta": "new"}
			switch failure {
			case "missing-item":
				params["itemId"] = "missing"
			case "wrong-type":
				method = "item/plan/delta"
			case "overflow":
				params["delta"] = strings.Repeat("a", maxTextBytes)
			case "unknown":
				method = "item/futureProtocol/delta"
			case "gap":
				f.mu.Lock()
				f.sequence++
				f.mu.Unlock()
			}
			awaitSequence(t, s, f.event(method, params))
			select {
			case change := <-changes:
				if change.ThreadID != "a" || change.ContentOnly || change.Revision != snapshot(t, s, "a").Revision {
					t.Fatalf("failure misclassified as content: %+v", change)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("missing resync change")
			}
			if !snapshot(t, s, "a").ResyncRequired {
				t.Fatal("failure did not require resync")
			}
		})
	}
}

func TestExactSnapshotBoundaryBufferedDeltaAndCachedCopies(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method == "watch" {
			s.mu.Lock()
			installed := s.projections["a"] != nil && !s.projections["a"].ready
			s.mu.Unlock()
			if !installed {
				t.Error("watch sent before per-share buffer installed")
			}
		}
		if method != "snapshot" {
			return nil, nil, false
		}
		f.event("item/agentMessage/delta", map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "item-1", "delta": "base"})
		thread := testThread("a", true)
		thread.Turns[0].Items[0].Content = json.RawMessage(`[{"type":"text","text":"owned"}]`)
		result := f.sequenced(map[string]any{"thread": thread})
		last := f.event("item/agentMessage/delta", map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "item-1", "delta": " after"})
		awaitSequence(t, s, last)
		return result, nil, true
	})
	openThread(t, s, "a")
	got := snapshot(t, s, "a")
	if got.Thread.Turns[0].Items[0].Text != "base after" || got.ResyncRequired {
		t.Fatalf("bad projection %+v", got)
	}
	got.Thread.Turns[0].Items[0].Text = "tampered"
	got.Thread.Turns[0].Items[0].Content[0] = 'X'
	for range 5 {
		copy := snapshot(t, s, "a")
		if copy.Thread.Turns[0].Items[0].Text != "base after" || copy.Thread.Turns[0].Items[0].Content[0] == 'X' {
			t.Fatal("Snapshot aliases cache")
		}
	}
	if f.count("snapshot") != 1 || f.count("history") != 0 || f.count("pending/snapshot") != 1 || f.count("thread/read") != 1 {
		t.Fatal("snapshot reread history", f.calls)
	}
}

func TestInitialPendingHasIndependentBoundaryAndResolvedWithoutThread(t *testing.T) {
	for _, pendingAtRead := range []bool{false, true} {
		t.Run(fmt.Sprint(pendingAtRead), func(t *testing.T) {
			f := newFake()
			f.threads["a"] = testThread("a", true)
			s := connectedService(t, f)
			req := request(json.RawMessage(`9007199254740993`), "a")
			f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
				if method != "pending/snapshot" {
					return nil, nil, false
				}
				f.emitRaw(rawJSON(req))
				if !pendingAtRead {
					sequence := f.event("serverRequest/resolved", map[string]any{"requestId": req.ID})
					return rawJSON(map[string]any{"requests": []PendingRequest{}, "sequence": sequence}), nil, true
				}
				f.mu.Lock()
				sequence := f.sequence
				f.mu.Unlock()
				last := f.event("serverRequest/resolved", map[string]any{"requestId": req.ID})
				awaitSequence(t, s, last)
				return rawJSON(map[string]any{"requests": []PendingRequest{req}, "sequence": sequence}), nil, true
			})
			openThread(t, s, "a")
			if got := snapshot(t, s, "a"); len(got.Pending) != 0 {
				t.Fatal("resolved request resurrected", got.Pending)
			}
		})
	}
}

func TestPendingLosslessIDAndDesktopWinningResponse(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	f.pending = []PendingRequest{request(json.RawMessage(`9007199254740993`), "a"), request(json.RawMessage(`"9007199254740993"`), "a")}
	s := connectedService(t, f)
	openThread(t, s, "a")
	got := snapshot(t, s, "a")
	if len(got.Pending) != 2 || string(got.Pending[0].ID) != `9007199254740993` || got.Pending[0].DesktopOnly {
		t.Fatal(got.Pending)
	}
	got.Pending[0].ID[0] = '1'
	got.Pending[0].Params[0] = 'X'
	if !json.Valid(snapshot(t, s, "a").Pending[0].Params) {
		t.Fatal("pending aliases cache")
	}
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method == "respond" {
			return nil, attachment.ErrRequestResolved, true
		}
		return nil, nil, false
	})
	if _, err := s.Respond(context.Background(), "a", json.RawMessage(`9007199254740993`), "accept"); !errors.Is(err, ErrStateChanged) {
		t.Fatal(err)
	}
	got = snapshot(t, s, "a")
	if len(got.Pending) != 1 || string(got.Pending[0].ID) != `"9007199254740993"` || got.ResyncRequired {
		t.Fatal(got)
	}
	if f.count("respond") != 1 {
		t.Fatal("response replayed")
	}
	if _, err := s.Respond(context.Background(), "a", json.RawMessage(`9007199254740992`), "accept"); !errors.Is(err, ErrStateChanged) {
		t.Fatal(err)
	}
	last := f.event("serverRequest/resolved", map[string]any{"requestId": json.RawMessage(`"9007199254740993"`)})
	awaitSequence(t, s, last)
	if len(snapshot(t, s, "a").Pending) != 0 {
		t.Fatal("string ID resolution failed")
	}
}

func TestProjectionGenerationRejectsStaleContextAndClearIsPrompt(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	openThread(t, s, "a")
	generation := s.ProjectionGeneration("a")
	if generation == 0 {
		t.Fatal("missing generation")
	}
	ctx := WithProjectionGeneration(context.Background(), generation)
	unwatchEntered := make(chan struct{})
	release := make(chan struct{})
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "unwatch" {
			return nil, nil, false
		}
		close(unwatchEntered)
		select {
		case <-release:
			return rawJSON(map[string]bool{"ok": true}), nil, true
		case <-ctx.Done():
			return nil, ctx.Err(), true
		}
	})
	done := make(chan struct{})
	go func() { s.ClearProjections(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ClearProjections blocked on IPC")
	}
	if s.ProjectionGeneration("a") != 0 {
		t.Fatal("projection not synchronously cancelled")
	}
	select {
	case <-unwatchEntered:
	case <-time.After(time.Second):
		t.Fatal("unwatch not scheduled")
	}
	openThread(t, s, "a")
	if s.ProjectionGeneration("a") <= generation {
		t.Fatal("generation reused")
	}
	for _, call := range []func() error{
		func() error { _, err := s.Read(ctx, "a", ReadOptions{}); return err },
		func() error { _, err := s.Send(ctx, "a", "hello", "start", ""); return err },
		func() error { _, err := s.Interrupt(ctx, "a", "turn-1"); return err },
		func() error { _, err := s.Respond(ctx, "a", json.RawMessage(`1`), "accept"); return err },
	} {
		if err := call(); !errors.Is(err, ErrStateChanged) {
			t.Fatal(err)
		}
	}
	close(release)
	f.mu.Lock()
	var watches, unwatches []string
	for _, call := range f.calls {
		var v struct {
			WatchID string `json:"watchId"`
		}
		_ = json.Unmarshal(call.params, &v)
		if call.method == "watch" {
			watches = append(watches, v.WatchID)
		}
		if call.method == "unwatch" {
			unwatches = append(unwatches, v.WatchID)
		}
	}
	f.mu.Unlock()
	if len(watches) != 2 || len(unwatches) != 1 || watches[0] != unwatches[0] || watches[0] == watches[1] {
		t.Fatal(watches, unwatches)
	}
}

func TestUnshareDuringInitializationDoesNotAuthorizeOldResponse(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "snapshot" {
			return nil, nil, false
		}
		first := false
		once.Do(func() { first = true })
		if !first {
			return nil, nil, false
		}
		result := f.sequenced(map[string]any{"thread": testThread("a", false)})
		close(entered)
		<-release // emulate a stale response even after cancellation
		return result, nil, true
	})
	errCh := make(chan error, 1)
	go func() { errCh <- readError(s, context.Background(), "a", ReadOptions{}) }()
	<-entered
	s.ClearProjections()
	openThread(t, s, "a")
	newGen := s.ProjectionGeneration("a")
	close(release)
	if err := <-errCh; !errors.Is(err, ErrStateChanged) {
		t.Fatal(err)
	}
	if s.ProjectionGeneration("a") != newGen || snapshot(t, s, "a").ResyncRequired {
		t.Fatal("old initialization affected new share")
	}
}

func TestRefreshNeverReconnectsAndRejectsStaleAttachmentResponse(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	openThread(t, s, "a")
	generation := s.ProjectionGeneration("a")
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.ProjectionGeneration("a") != generation {
		t.Fatal("refresh reset share")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "status" {
			return nil, nil, false
		}
		close(entered)
		<-release
		return rawJSON(map[string]bool{"ready": true}), nil, true
	})
	errCh := make(chan error, 1)
	go func() { errCh <- s.Refresh(context.Background()) }()
	<-entered
	fresh := newFake()
	fresh.pid = 202
	fresh.threads = map[string]Thread{"b": testThread("b", false)}
	s.discover = func(context.Context, string) (attachmentClient, error) { return fresh, nil }
	if err := s.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errCh; !errors.Is(err, ErrNoAttachment) {
		t.Fatal(err)
	}
	page, err := s.ListThreads(context.Background(), ListOptions{})
	if err != nil || s.EnginePID() != 202 || len(page.Threads) != 1 || page.Threads[0].ID != "b" || s.ProjectionGeneration("a") != 0 {
		t.Fatal(page, err)
	}
}

func TestHistoryBoundedAndFallbackNeverMergesActiveRead(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		t.Run(fmt.Sprint(overlap), func(t *testing.T) {
			f := newFake()
			s := connectedService(t, f)
			f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
				switch method {
				case "snapshot":
					th := testThread("a", false)
					th.Turns = nil
					return f.sequenced(map[string]any{"thread": th}), nil, true
				case "history":
					if overlap {
						f.event("turn/started", map[string]any{"threadId": "a", "turn": Turn{ID: "turn-new", Status: "inProgress", Items: []Item{}}})
					}
					return f.sequenced(map[string]any{"data": []Turn{{ID: "old", Status: "completed", Items: []Item{}}}, "nextCursor": "older"}), nil, true
				}
				return nil, nil, false
			})
			err := readError(s, context.Background(), "a", ReadOptions{})
			if overlap {
				if !errors.Is(err, ErrResyncRequired) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := snapshot(t, s, "a"); !got.HistoryTruncated || len(got.Thread.Turns) != 1 {
				t.Fatal(got)
			}
			for range 3 {
				snapshot(t, s, "a")
			}
			if f.count("history") != 1 {
				t.Fatal("fallback repeated")
			}
		})
	}
	f := newFake()
	thread := testThread("a", false)
	thread.Turns = nil
	for i := 0; i < 35; i++ {
		thread.Turns = append(thread.Turns, Turn{ID: fmt.Sprint(i), Status: "completed", Items: []Item{}})
	}
	f.threads["a"] = thread
	s := connectedService(t, f)
	openThread(t, s, "a")
	got := snapshot(t, s, "a")
	if len(got.Thread.Turns) != 30 || got.Thread.Turns[0].ID != "5" || !got.HistoryTruncated {
		t.Fatal(got)
	}
}

func TestOrderingAndDataOverflowFailVisibly(t *testing.T) {
	for _, which := range []string{"gap", "duplicate", "delta", "item", "unknown"} {
		t.Run(which, func(t *testing.T) {
			f := newFake()
			f.threads["a"] = testThread("a", true)
			s := connectedService(t, f)
			openThread(t, s, "a")
			var last uint64
			switch which {
			case "gap":
				f.mu.Lock()
				f.sequence++
				f.mu.Unlock()
				last = f.event("thread/name/updated", map[string]any{"threadId": "a", "threadName": "bad"})
			case "duplicate":
				s.mu.Lock()
				sequence := s.conn.lastSequence
				s.mu.Unlock()
				f.events <- bridge.Event{Sequence: sequence, Message: rawJSON(map[string]any{"method": "thread/name/updated", "params": map[string]any{"threadId": "a"}})}
				time.Sleep(10 * time.Millisecond)
				last = sequence
			case "delta":
				last = f.event("item/agentMessage/delta", map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "item-1", "delta": strings.Repeat("a", maxTextBytes)})
			case "item":
				last = f.event("item/completed", map[string]any{"threadId": "a", "turnId": "turn-1", "item": Item{ID: "huge", Type: "agentMessage", Text: strings.Repeat("x", maxTextBytes+1)}})
			case "unknown":
				last = f.event("item/newProtocol/mutation", map[string]any{"threadId": "a"})
			}
			awaitSequence(t, s, last)
			got := snapshot(t, s, "a")
			if !got.ResyncRequired || got.ResyncReason == "" || len(got.Thread.Turns) != 0 {
				t.Fatal(got)
			}
			if _, err := s.Send(context.Background(), "a", "hello", "steer", "turn-1"); !errors.Is(err, ErrResyncRequired) {
				t.Fatal(err)
			}
		})
	}
}

func TestInitializationQueueOverflowFailsWithoutAuthorizing(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "snapshot" {
			return nil, nil, false
		}
		var last uint64
		for i := 0; i <= maxBufferedEvents; i++ {
			last = f.event("thread/name/updated", map[string]any{"threadId": "a", "threadName": "n"})
		}
		awaitSequence(t, s, last)
		return f.sequenced(map[string]any{"thread": testThread("a", false)}), nil, true
	})
	if err := readError(s, context.Background(), "a", ReadOptions{}); !errors.Is(err, ErrResyncRequired) {
		t.Fatal(err)
	}
	if s.ProjectionGeneration("a") != 0 {
		t.Fatal("overflow authorized share")
	}
}

func TestTurnInputGuardsAndCanceledQueuedWork(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	openThread(t, s, "a")
	for _, input := range []struct{ text, mode, expected string }{{" ", "steer", "turn-1"}, {"hello", "invalid", ""}, {"hello", "start", ""}, {"hello", "steer", "wrong"}, {strings.Repeat("a", 32769), "steer", "turn-1"}} {
		if _, err := s.Send(context.Background(), "a", input.text, input.mode, input.expected); err == nil {
			t.Fatal("invalid input allowed", input.mode)
		}
	}
	if _, err := s.Interrupt(context.Background(), "a", "wrong"); !errors.Is(err, ErrStateChanged) {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "a", "continue", "steer", "turn-1"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	var params json.RawMessage
	for _, c := range f.calls {
		if c.method == "turn/steer" {
			params = c.params
		}
	}
	f.mu.Unlock()
	var got map[string]any
	_ = json.Unmarshal(params, &got)
	if got["expectedTurnId"] != "turn-1" {
		t.Fatal(got)
	}
	<-s.commandGate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Interrupt(ctx, "a", "turn-1"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.commandGate <- struct{}{}
	if f.count("turn/interrupt") != 0 {
		t.Fatal("canceled work dispatched")
	}
}

func TestCommandsRejectStaleResultAcrossReconnect(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	openThread(t, s, "a")
	entered, release := make(chan struct{}), make(chan struct{})
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "turn/steer" {
			return nil, nil, false
		}
		close(entered)
		<-release
		return json.RawMessage(`{"private":"old attachment"}`), nil, true
	})
	errCh := make(chan error, 1)
	go func() {
		data, err := s.Send(context.Background(), "a", "hi", "steer", "turn-1")
		if len(data) != 0 {
			t.Error("stale data leaked")
		}
		errCh <- err
	}()
	<-entered
	fresh := newFake()
	s.discover = func(context.Context, string) (attachmentClient, error) { return fresh, nil }
	if err := s.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	openThread(t, s, "a")
	close(release)
	if err := <-errCh; !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if got := snapshot(t, s, "a"); got.ResyncRequired || got.Thread.Turns[0].Items[0].Text != "base" {
		t.Fatal(got)
	}
}

func TestActualProtocolReducerAndFinalAuthoritativeItems(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	openThread(t, s, "a")
	events := []struct {
		method string
		params any
	}{
		{"item/started", map[string]any{"threadId": "a", "turnId": "turn-1", "item": Item{ID: "cmd", Type: "commandExecution", Status: "inProgress", Command: "git status"}}},
		{"item/commandExecution/outputDelta", map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "cmd", "delta": "out"}},
		{"item/started", map[string]any{"threadId": "a", "turnId": "turn-1", "item": Item{ID: "reason", Type: "reasoning", Content: json.RawMessage(`[]`)}}},
		{"item/reasoning/summaryPartAdded", map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "reason", "summaryIndex": 0}},
		{"item/reasoning/summaryTextDelta", map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "reason", "summaryIndex": 0, "delta": "summary"}},
		{"item/reasoning/textDelta", map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "reason", "contentIndex": 0, "delta": "reasoning"}},
		{"item/started", map[string]any{"threadId": "a", "turnId": "turn-1", "item": Item{ID: "plan", Type: "plan", Text: "initial"}}},
		{"item/plan/delta", map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "plan", "delta": " proposal"}},
		{"item/completed", map[string]any{"threadId": "a", "turnId": "turn-1", "item": Item{ID: "plan", Type: "plan", Text: "authoritative"}}},
		{"turn/plan/updated", map[string]any{"threadId": "a", "turnId": "turn-1", "plan": []map[string]string{{"step": "verify", "status": "inProgress"}}, "explanation": "working"}},
		{"thread/status/changed", map[string]any{"threadId": "a", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}}}},
	}
	var last uint64
	for _, e := range events {
		last = f.event(e.method, e.params)
	}
	awaitSequence(t, s, last)
	got := snapshot(t, s, "a")
	if got.ResyncRequired {
		t.Fatal(got.ResyncReason)
	}
	items := got.Thread.Turns[0].Items
	if len(items) != 4 || items[1].Output != "out" || !reflect.DeepEqual(items[2].Summary, []string{"summary"}) || string(items[2].Content) != `["reasoning"]` || items[3].Text != "authoritative" {
		t.Fatal(items)
	}
	last = f.event("turn/completed", map[string]any{"threadId": "a", "turn": Turn{ID: "turn-1", Status: "completed", Items: []Item{}}})
	awaitSequence(t, s, last)
	got = snapshot(t, s, "a")
	if got.ActiveTurnID != "" || len(got.Thread.Turns[0].Items) != 4 {
		t.Fatal(got)
	}
	if _, err := s.Send(context.Background(), "a", "next", "start", ""); err != nil {
		t.Fatal(err)
	}
	if f.count("snapshot") != 1 {
		t.Fatal("commands reread snapshot")
	}
}

func TestCataloguePaginatesBeyond128WithoutHydratingHistory(t *testing.T) {
	f := newFake()
	f.threads = make(map[string]Thread)
	for i := range 257 {
		id := fmt.Sprintf("catalogue-%03d", i)
		thread := testThread(id, false)
		thread.CanAcceptDirectInput = nil
		thread.UpdatedAt = int64(i + 1)
		f.threads[id] = thread
	}
	s := connectedService(t, f)
	seen := make(map[string]bool)
	cursor := ""
	pages := 0
	for {
		page, err := s.ListThreads(context.Background(), ListOptions{Cursor: cursor, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if pages > 10 {
			t.Fatal("cursor failed to terminate")
		}
		for _, item := range page.Threads {
			if seen[item.ID] || item.CanAcceptDirectInput != nil || item.CWD != "C:/test" || item.UpdatedAt == 0 {
				t.Fatal("incorrect catalogue metadata", item)
			}
			seen[item.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatal("cursor repeated")
		}
		cursor = page.NextCursor
	}
	if len(seen) != 257 || f.count("thread/list") != 3 || f.count("snapshot") != 0 || f.count("history") != 0 || f.count("watch") != 0 {
		t.Fatal("catalogue truncated or hydrated transcripts", len(seen), f.calls)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.method != "thread/read" {
			continue
		}
		var params struct {
			IncludeTurns *bool `json:"includeTurns"`
		}
		if json.Unmarshal(call.params, &params) != nil || params.IncludeTurns == nil || *params.IncludeTurns {
			t.Fatal("loaded supplement requested transcript", string(call.params))
		}
	}
}

func TestCatalogueUsesDesktopDefaultSourcesAndProviderAndRejectsBadOptions(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "thread/list" {
			return nil, nil, false
		}
		var params map[string]json.RawMessage
		if json.Unmarshal(raw, &params) != nil {
			t.Fatal("invalid params")
		}
		if len(params["sourceKinds"]) != 0 || len(params["modelProviders"]) != 0 || string(params["archived"]) != "true" || string(params["limit"]) != "50" || string(params["cursor"]) != `"opaque"` || len(params["searchTerm"]) != 0 || string(params["sortKey"]) != `"updated_at"` || string(params["sortDirection"]) != `"desc"` {
			t.Fatal(string(raw))
		}
		for _, key := range []string{"projectId", "sectionId", "cwd", "parentThreadId", "ancestorThreadId", "originators"} {
			if _, exists := params[key]; exists {
				t.Fatalf("unwanted catalogue filter %s", key)
			}
		}
		thread := testThread("a", false)
		thread.CWD = `C:\repo\needle`
		return rawJSON(map[string]any{"data": []Thread{thread}}), nil, true
	})
	page, err := s.ListThreads(context.Background(), ListOptions{Cursor: "stored:opaque", Search: "needle", Archived: true})
	if err != nil || len(page.Threads) != 1 || !page.Threads[0].Archived || page.NextCursor != "" {
		t.Fatal(page, err)
	}
	before := f.count("thread/list")
	for _, options := range []ListOptions{{Limit: -1}, {Limit: 101}, {Search: strings.Repeat("a", 513)}, {Cursor: strings.Repeat("a", 4097)}, {Cursor: "unrecognized"}, {Cursor: "loaded:", Archived: true}} {
		if _, err := s.ListThreads(context.Background(), options); !errors.Is(err, ErrInvalidListOptions) {
			t.Fatal(options, err)
		}
	}
	if f.count("thread/list") != before {
		t.Fatal("invalid request reached attachment")
	}
}

func TestCatalogueErrorsNeverBecomeEmptySuccess(t *testing.T) {
	for _, raw := range []string{"", `{}`, `{"data":null}`, `{"data":[{"id":""}]}`, `{"data":[],"nextCursor":"repeat"}`} {
		t.Run(fmt.Sprint(len(raw)), func(t *testing.T) {
			f := newFake()
			s := connectedService(t, f)
			f.setHook(func(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error, bool) {
				if method == "thread/list" {
					return json.RawMessage(raw), nil, true
				}
				return nil, nil, false
			})
			if _, err := s.ListThreads(context.Background(), ListOptions{Cursor: "stored:repeat"}); err == nil {
				t.Fatal("invalid catalogue accepted")
			}
		})
	}
	f := newFake()
	s := connectedService(t, f)
	failure := errors.New("unsupported attachment operation")
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method == "thread/list" {
			return nil, failure, true
		}
		return nil, nil, false
	})
	if _, err := s.ListThreads(context.Background(), ListOptions{}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestCatalogueDoesNotSupplementFromUnfilteredLoadedThreads(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		switch method {
		case "thread/list":
			return rawJSON(map[string]any{"data": []Thread{}}), nil, true
		case "thread/loaded/list":
			return rawJSON(map[string]any{"data": []string{"ephemeral"}}), nil, true
		case "thread/read":
			return rawJSON(map[string]any{"thread": map[string]any{"id": "ephemeral", "name": "Live transient", "ephemeral": true, "status": map[string]string{"type": "active"}, "canAcceptDirectInput": false}}), nil, true
		}
		return nil, nil, false
	})
	first, err := s.ListThreads(context.Background(), ListOptions{})
	if err != nil || first.NextCursor != "" || len(first.Threads) != 0 {
		t.Fatal(first, err)
	}
	if _, err := s.ListThreads(context.Background(), ListOptions{Cursor: "loaded:"}); !errors.Is(err, ErrInvalidListOptions) {
		t.Fatal("legacy unfiltered cursor accepted", err)
	}
	if f.count("thread/loaded/list") != 0 || f.count("thread/read") != 0 || f.count("snapshot") != 0 {
		t.Fatal("loaded sessions outside Desktop's catalogue were queried")
	}
	changes, cancel := s.Subscribe()
	defer cancel()
	beforeList := f.count("thread/list")
	for _, method := range []string{"thread/started", "thread/name/updated", "thread/status/changed", "thread/archived", "thread/unarchived", "thread/deleted"} {
		awaitSequence(t, s, f.event(method, map[string]any{"threadId": "unprojected", "thread": map[string]string{"id": "unprojected"}, "threadName": "new", "status": map[string]string{"type": "idle"}}))
		select {
		case change := <-changes:
			if !change.CatalogueChanged || change.ThreadID != "" || change.ContentOnly || change.Disconnected {
				t.Fatal(change)
			}
		case <-time.After(time.Second):
			t.Fatal("missing unprojected metadata hint")
		}
	}
	awaitSequence(t, s, f.event("item/agentMessage/delta", map[string]any{"threadId": "unprojected", "turnId": "t", "itemId": "i", "delta": "token"}))
	select {
	case change := <-changes:
		t.Fatal("unprojected token invalidated catalogue", change)
	default:
	}
	if f.count("thread/list") != beforeList || s.ProjectionGeneration("unprojected") != 0 {
		t.Fatal("event triggered listing or eager projection")
	}
}

func TestReadonlyHistoryPreservesUnknownCapabilityAndNeverResumes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		archived   bool
		capability *bool
	}{
		{"archived-true", true, boolPtr(true)}, {"archived-unknown", true, nil}, {"explicit-false", false, boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			thread := testThread("a", false)
			thread.CanAcceptDirectInput = tc.capability
			f.threads["a"] = thread
			s := connectedService(t, f)
			got, err := s.Read(context.Background(), "a", ReadOptions{Archived: tc.archived})
			if err != nil || !got.ReadOnly || got.ReadOnlyReason == "" || len(got.Thread.Turns) != 1 || got.ActiveTurnID != "" || acceptsInput(got.Thread.CanAcceptDirectInput) {
				t.Fatal(got, err)
			}
			if (tc.capability == nil) != (got.Thread.CanAcceptDirectInput == nil) {
				t.Fatal("unknown capability collapsed to false")
			}
			if f.count("history") != 1 {
				t.Fatal("readonly bounded history missing")
			}
			if _, err := s.Send(context.Background(), "a", "not allowed", "start", ""); !errors.Is(err, ErrStateChanged) {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, call := range f.calls {
				if call.method == "turn/start" || call.method == "thread/resume" || call.method == "thread/unarchive" {
					t.Fatal("readonly issued lifecycle mutation", call.method)
				}
				if call.method == "snapshot" {
					var params struct {
						ReadOnly bool `json:"readOnly"`
					}
					if json.Unmarshal(call.params, &params) != nil || !params.ReadOnly {
						t.Fatal("snapshot may resume readonly thread")
					}
				}
			}
		})
	}
}

func TestUnavailableReadonlyHistoryReturnsReasonNotEmptySnapshot(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method == "history" {
			return nil, errors.New("thread turns unavailable"), true
		}
		return nil, nil, false
	})
	_, err := s.Read(context.Background(), "a", ReadOptions{Archived: true})
	if err == nil || !strings.Contains(err.Error(), "bounded history is unavailable") || s.ProjectionGeneration("a") != 0 {
		t.Fatal(err)
	}
}

func TestSequentialReadsRecycle32EntryLRUAndWaitForUnwatch(t *testing.T) {
	f := newFake()
	for i := range 80 {
		id := fmt.Sprintf("browse-%02d", i)
		f.threads[id] = testThread(id, false)
	}
	s := connectedService(t, f)
	var mu sync.Mutex
	active := make(map[string]string)
	peak, rejected := 0, 0
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "watch" && method != "unwatch" {
			return nil, nil, false
		}
		var params struct {
			ThreadID string `json:"threadId"`
			WatchID  string `json:"watchId"`
		}
		_ = json.Unmarshal(raw, &params)
		mu.Lock()
		defer mu.Unlock()
		if method == "watch" {
			if len(active) >= bridge.MaxWatchedThreads {
				rejected++
				return nil, errors.New("bridge capacity"), true
			}
			active[params.ThreadID] = params.WatchID
			if len(active) > peak {
				peak = len(active)
			}
		} else if active[params.ThreadID] == params.WatchID {
			delete(active, params.ThreadID)
		}
		return rawJSON(map[string]bool{"ok": true}), nil, true
	})
	var previous uint64
	for i := range 80 {
		id := fmt.Sprintf("browse-%02d", i)
		got, err := s.Read(context.Background(), id, ReadOptions{})
		if err != nil || got.Revision <= previous {
			t.Fatal(i, got.Revision, previous, err)
		}
		previous = got.Revision
	}
	if s.ProjectionGeneration("browse-00") != 0 || s.ProjectionGeneration("browse-79") == 0 {
		t.Fatal("LRU retained wrong end")
	}
	s.mu.Lock()
	count := len(s.projections)
	s.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if count != bridge.MaxWatchedThreads || peak != bridge.MaxWatchedThreads || rejected != 0 || len(active) != bridge.MaxWatchedThreads {
		t.Fatal(count, peak, rejected, len(active))
	}
}

func awaitPins(t *testing.T, s *Service, id string, minimum int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		s.mu.Lock()
		p := s.projections[id]
		ready := p != nil && p.pins >= minimum
		s.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-deadline:
			t.Fatal("projection was not pinned")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestQueuedCommandPinsIdentityAcrossLRUEvictionAndClear(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	for i := range 33 {
		id := fmt.Sprintf("other-%02d", i)
		f.threads[id] = testThread(id, false)
	}
	s := connectedService(t, f)
	openThread(t, s, "a")
	generation := s.ProjectionGeneration("a")
	ctx := WithProjectionGeneration(context.Background(), generation)
	for i := range 31 {
		openThread(t, s, fmt.Sprintf("other-%02d", i))
	}
	<-s.commandGate
	queued := make(chan error, 1)
	go func() { _, err := s.Interrupt(ctx, "a", "turn-1"); queued <- err }()
	awaitPins(t, s, "a", 1)
	openThread(t, s, "other-31")
	if s.ProjectionGeneration("a") != generation || s.ProjectionGeneration("other-00") != 0 {
		t.Fatal("queued command projection evicted")
	}
	if _, err := s.Read(context.Background(), "a", ReadOptions{Resync: true}); !errors.Is(err, ErrProjectionCapacity) {
		t.Fatal("resync replaced queued work", err)
	}
	s.ClearProjections()
	select {
	case err := <-queued:
		if !errors.Is(err, ErrStateChanged) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("clear did not cancel queued work")
	}
	s.commandGate <- struct{}{}
	openThread(t, s, "a")
	if s.ProjectionGeneration("a") <= generation {
		t.Fatal("projection generation reused")
	}
	if _, err := s.Interrupt(ctx, "a", "turn-1"); !errors.Is(err, ErrStateChanged) {
		t.Fatal(err)
	}
	if f.count("turn/interrupt") != 0 {
		t.Fatal("stale queued work dispatched")
	}
}

func TestAdmittedCommandPinsUntilOutcomeSettles(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	for i := range 33 {
		id := fmt.Sprintf("work-%02d", i)
		f.threads[id] = testThread(id, false)
	}
	s := connectedService(t, f)
	openThread(t, s, "a")
	generation := s.ProjectionGeneration("a")
	entered, release := make(chan struct{}), make(chan struct{})
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "turn/steer" {
			return nil, nil, false
		}
		close(entered)
		select {
		case <-release:
			return rawJSON(map[string]string{"turnId": "turn-1"}), nil, true
		case <-ctx.Done():
			return nil, ctx.Err(), true
		}
	})
	finished := make(chan error, 1)
	go func() { _, err := s.Send(context.Background(), "a", "once", "steer", "turn-1"); finished <- err }()
	<-entered
	for i := range 32 {
		openThread(t, s, fmt.Sprintf("work-%02d", i))
	}
	if s.ProjectionGeneration("a") != generation {
		t.Fatal("admitted command was evicted")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	openThread(t, s, "work-32")
	if s.ProjectionGeneration("a") != 0 || f.count("turn/steer") != 1 {
		t.Fatal("settled work did not unpin or replayed")
	}
}

func TestReadSingleFlightAndInitializationPins(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "snapshot" {
			return nil, nil, false
		}
		close(entered)
		select {
		case <-release:
			return f.sequenced(map[string]any{"thread": testThread("a", false)}), nil, true
		case <-ctx.Done():
			return nil, ctx.Err(), true
		}
	})
	finished := make(chan error, 2)
	go func() { finished <- readError(s, context.Background(), "a", ReadOptions{}) }()
	<-entered
	go func() { finished <- readError(s, context.Background(), "a", ReadOptions{Resync: true}) }()
	awaitPins(t, s, "a", 2)
	if s.ProjectionGeneration("a") != 0 || f.count("snapshot") != 1 {
		t.Fatal("initializing projection admitted or initialized twice")
	}
	close(release)
	for range 2 {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	if f.count("snapshot") != 1 || s.ProjectionGeneration("a") == 0 {
		t.Fatal("single flight failed")
	}
}

func TestAllInitializingProjectionsBoundCapacity(t *testing.T) {
	f := newFake()
	for i := range 33 {
		id := fmt.Sprintf("init-%02d", i)
		f.threads[id] = testThread(id, false)
	}
	s := connectedService(t, f)
	entered := make(chan struct{}, 32)
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "snapshot" {
			return nil, nil, false
		}
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err(), true
	})
	finished := make(chan error, 32)
	for i := range 32 {
		go func(i int) {
			finished <- readError(s, context.Background(), fmt.Sprintf("init-%02d", i), ReadOptions{})
		}(i)
	}
	for range 32 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("initialization did not start")
		}
	}
	if _, err := s.Read(context.Background(), "init-32", ReadOptions{}); !errors.Is(err, ErrProjectionCapacity) {
		t.Fatal(err)
	}
	s.ClearProjections()
	for range 32 {
		select {
		case err := <-finished:
			if err == nil {
				t.Fatal("clear returned old initialization")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("clear left initialization pinned")
		}
	}
	if f.count("snapshot") != 32 {
		t.Fatal("capacity allowed extra initialization")
	}
}

func TestExplicitResyncRebuildsFailureButNeverPretendsToRepairOrder(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	openThread(t, s, "a")
	generation := s.ProjectionGeneration("a")
	awaitSequence(t, s, f.event("item/newProtocol/mutation", map[string]string{"threadId": "a"}))
	failed := snapshot(t, s, "a")
	if !failed.ResyncRequired || s.ProjectionGeneration("a") != generation {
		t.Fatal("normal read silently recovered failure")
	}
	rebuilt, err := s.Read(context.Background(), "a", ReadOptions{Resync: true})
	if err != nil || rebuilt.ResyncRequired || rebuilt.Revision <= failed.Revision || s.ProjectionGeneration("a") <= generation {
		t.Fatal(rebuilt, err)
	}
	f.mu.Lock()
	f.sequence++
	f.mu.Unlock()
	awaitSequence(t, s, f.event("thread/name/updated", map[string]string{"threadId": "a", "threadName": "gap"}))
	before := f.count("snapshot")
	if err := s.Refresh(context.Background()); !errors.Is(err, ErrResyncRequired) {
		t.Fatal(err)
	}
	if _, err := s.Read(context.Background(), "a", ReadOptions{Resync: true}); !errors.Is(err, ErrResyncRequired) {
		t.Fatal(err)
	}
	if f.count("snapshot") != before {
		t.Fatal("ordering error silently reinitialized")
	}
}

func TestFullCacheReplacementWaitsForReleasedWatchAcknowledgement(t *testing.T) {
	f := newFake()
	for i := range 33 {
		id := fmt.Sprintf("wait-%02d", i)
		f.threads[id] = testThread(id, false)
	}
	s := connectedService(t, f)
	for i := range 32 {
		openThread(t, s, fmt.Sprintf("wait-%02d", i))
	}
	entered, release := make(chan struct{}), make(chan struct{})
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "unwatch" {
			return nil, nil, false
		}
		close(entered)
		select {
		case <-release:
			return rawJSON(map[string]bool{"ok": true}), nil, true
		case <-ctx.Done():
			return nil, ctx.Err(), true
		}
	})
	finished := make(chan error, 1)
	go func() { finished <- readError(s, context.Background(), "wait-32", ReadOptions{}) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("eviction did not schedule unwatch")
	}
	select {
	case err := <-finished:
		t.Fatal("new read completed before unwatch acknowledgement", err)
	default:
	}
	if f.count("watch") != 32 || s.ProjectionGeneration("wait-00") != 0 {
		t.Fatal("new watch preceded release acknowledgement")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if f.count("watch") != 33 {
		t.Fatal("replacement watch missing")
	}
}

func TestCapabilityPointersAreOwnedAndJSONNullIsPreserved(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	first := snapshot(t, s, "a")
	*first.Thread.CanAcceptDirectInput = false
	if !acceptsInput(snapshot(t, s, "a").Thread.CanAcceptDirectInput) {
		t.Fatal("capability aliases projection")
	}
	page, err := s.ListThreads(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	*page.Threads[0].CanAcceptDirectInput = false
	next, err := s.ListThreads(context.Background(), ListOptions{})
	if err != nil || !acceptsInput(next.Threads[0].CanAcceptDirectInput) {
		t.Fatal("capability aliases catalogue", err)
	}
	encoded := rawJSON(Summary{ID: "unloaded"})
	if !strings.Contains(string(encoded), `"canAcceptDirectInput":null`) || strings.Contains(string(encoded), `"shared"`) {
		t.Fatal(string(encoded))
	}
}
