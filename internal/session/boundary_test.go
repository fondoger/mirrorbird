package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStartResponseMayPrecedeLifecycleNotification(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	openThread(t, s, "a")
	responseReturned, releaseEvent := make(chan struct{}), make(chan struct{})
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "turn/start" {
			return nil, nil, false
		}
		close(responseReturned)
		go func() {
			<-releaseEvent
			f.event("turn/started", map[string]any{"threadId": "a", "turn": Turn{ID: "later", Status: "inProgress", Items: []Item{}}})
		}()
		return rawJSON(map[string]any{"turn": Turn{ID: "later", Status: "inProgress"}}), nil, true
	})
	result := make(chan error, 1)
	go func() { _, err := s.Send(context.Background(), "a", "hello", "start", ""); result <- err }()
	<-responseReturned
	select {
	case err := <-result:
		t.Fatal("start completed before lifecycle", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(releaseEvent)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle failed to release command")
	}
	if got := snapshot(t, s, "a"); got.ResyncRequired || got.ActiveTurnID != "later" {
		t.Fatal(got)
	}
	if f.count("turn/start") != 1 || f.count("snapshot") != 1 {
		t.Fatal("command or history replayed")
	}
}

func TestCancelledStartCannotLeaveInvisibleStartLatch(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	openThread(t, s, "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Send(ctx, "a", "hello", "start", ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.mu.Lock()
	pending := s.projections["a"].startPending
	s.mu.Unlock()
	if pending || f.count("turn/start") != 0 {
		t.Fatal("cancelled admission left a start latch")
	}
	if _, err := s.Send(context.Background(), "a", "hello", "start", ""); err != nil {
		t.Fatal(err)
	}
}

func TestPreResponseDeltaNotInSnapshotRequiresResync(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "snapshot" {
			return nil, nil, false
		}
		f.event("item/agentMessage/delta", map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "item-1", "delta": "uncovered"})
		return f.sequenced(map[string]any{"thread": testThread("a", true)}), nil, true
	})
	if err := readError(s, context.Background(), "a", ReadOptions{}); !errors.Is(err, ErrResyncRequired) {
		t.Fatal(err)
	}
	if s.ProjectionGeneration("a") != 0 {
		t.Fatal("uncertain snapshot authorized")
	}
}

func TestDeltaBudgetsIncrementallyWithoutScanningHistory(t *testing.T) {
	thread := testThread("a", true)
	for i := 0; i < 18; i++ {
		thread.Turns[0].Items = append(thread.Turns[0].Items, Item{ID: fmt.Sprint(i), Type: "commandExecution", Output: strings.Repeat("h", 200<<10)})
	}
	p := &projection{snapshot: Snapshot{Thread: thread}, pending: make(map[string]*pendingEntry)}
	if err := p.validateBounds(); err != nil {
		t.Fatal(err)
	}
	before := p.stateBytes
	key := itemBudgetKey("turn-1", "item-1")
	itemBefore := p.itemBytes[key]
	for range 50 {
		changed, err := p.reduce("item/agentMessage/delta", rawJSON(map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "item-1", "delta": "text"}))
		if err != nil || !changed {
			t.Fatal(changed, err)
		}
	}
	if p.stateBytes <= before || p.itemBytes[key] <= itemBefore {
		t.Fatal("delta budget not charged")
	}
	// The aggregate budget can reject a small delta even while its item has room.
	p.stateBytes = maxProjectionBytes - 1
	beforeText := p.snapshot.Thread.Turns[0].Items[0].Text
	if _, err := p.reduce("item/agentMessage/delta", rawJSON(map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "item-1", "delta": "x"})); !errors.Is(err, ErrResyncRequired) {
		t.Fatal(err)
	}
	if p.snapshot.Thread.Turns[0].Items[0].Text != beforeText {
		t.Fatal("overflow changed text before rejection")
	}
}

func TestBundledRecentPagePreservesActiveLiveResume(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "snapshot" {
			return nil, nil, false
		}
		thread := testThread("a", true)
		thread.Turns[0].Items[0].Text = "live"
		page := []Turn{{ID: "turn-1", Status: "inProgress", Items: []Item{{ID: "item-1", Type: "agentMessage", Text: "persisted"}}}, {ID: "older", Status: "completed", Items: []Item{}}}
		return f.sequenced(map[string]any{"thread": thread, "initialTurnsPage": map[string]any{"data": page, "nextCursor": "more"}}), nil, true
	})
	openThread(t, s, "a")
	got := snapshot(t, s, "a")
	if got.ActiveTurnID != "turn-1" || len(got.Thread.Turns) != 2 || got.Thread.Turns[1].Items[0].Text != "live" || !got.HistoryTruncated || f.count("history") != 0 {
		t.Fatal(got)
	}
}

func TestDesktopPreludeStatusAndGoalNotificationsDoNotRequireResync(t *testing.T) {
	f := newFake()
	thread := testThread("a", false)
	for i := 2; i <= 9; i++ {
		thread.Turns = append(thread.Turns, Turn{ID: fmt.Sprintf("turn-%d", i), Status: "completed", Items: []Item{}})
	}
	f.threads["a"] = thread
	s := connectedService(t, f)
	openThread(t, s, "a")
	for _, event := range []struct {
		method string
		params any
	}{
		{"thread/goal/cleared", map[string]any{"threadId": "a"}},
		{"thread/status/changed", map[string]any{"threadId": "a", "status": map[string]any{"type": "active", "activeFlags": []string{}}}},
		{"turn/started", map[string]any{"threadId": "a", "turn": Turn{ID: "desktop-turn", Status: "inProgress", Items: []Item{}}}},
		{"item/started", map[string]any{"threadId": "a", "turnId": "desktop-turn", "item": Item{ID: "user", Type: "userMessage", Content: rawJSON([]map[string]string{{"type": "text", "text": "small test"}})}}},
		{"item/completed", map[string]any{"threadId": "a", "turnId": "desktop-turn", "item": Item{ID: "user", Type: "userMessage", Content: rawJSON([]map[string]string{{"type": "text", "text": "small test"}})}}},
		{"item/started", map[string]any{"threadId": "a", "turnId": "desktop-turn", "item": Item{ID: "agent", Type: "agentMessage", Text: ""}}},
		{"item/agentMessage/delta", map[string]any{"threadId": "a", "turnId": "desktop-turn", "itemId": "agent", "delta": "response"}},
		{"item/completed", map[string]any{"threadId": "a", "turnId": "desktop-turn", "item": Item{ID: "agent", Type: "agentMessage", Text: "response"}}},
		{"thread/tokenUsage/updated", map[string]any{"threadId": "a", "turnId": "desktop-turn", "tokenUsage": map[string]any{"total": map[string]int{"totalTokens": 100}}}},
		{"thread/status/changed", map[string]any{"threadId": "a", "status": map[string]string{"type": "idle"}}},
		{"turn/completed", map[string]any{"threadId": "a", "turn": Turn{ID: "desktop-turn", Status: "completed", Items: []Item{}}}},
		{"mcpServer/startupStatus/updated", map[string]any{"threadId": "a", "name": "test", "status": "ready", "error": nil, "failureReason": nil}},
		{"mcpServer/startupStatus/updated", map[string]any{"threadId": "a", "name": "test", "status": "failed", "error": "synthetic startup error", "failureReason": "reauthenticationRequired"}},
	} {
		awaitSequence(t, s, f.event(event.method, event.params))
		got := snapshot(t, s, "a")
		if got.ResyncRequired {
			t.Fatalf("%s required resync: %s", event.method, got.ResyncReason)
		}
	}
	got := snapshot(t, s, "a")
	if got.ActiveTurnID != "" || len(got.Thread.Turns) != 10 || got.Thread.Turns[9].Items[1].Text != "response" {
		t.Fatal(got)
	}
}

func TestMCPStartupMetadataDoesNotBypassIDBearingPendingRequests(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	openThread(t, s, "a")
	changes, cancel := s.Subscribe()
	defer cancel()
	params := map[string]string{"threadId": "a", "name": "test", "status": "ready"}
	awaitSequence(t, s, f.emitMessage(map[string]any{"id": "mcp-request", "method": "mcpServer/startupStatus/updated", "params": params}))
	got := snapshot(t, s, "a")
	if got.ResyncRequired || len(got.Pending) != 1 || !got.Pending[0].DesktopOnly {
		t.Fatal(got)
	}
	select {
	case change := <-changes:
		if change.ContentOnly || change.ThreadID != "a" {
			t.Fatal(change)
		}
	case <-time.After(time.Second):
		t.Fatal("ID-bearing request was ignored")
	}
}

func TestLiveFailureReasonUsesOnlyFixedMethodAndFailureStage(t *testing.T) {
	for _, tc := range []struct {
		name, method, want string
		params             any
	}{
		{"reducer", "item/agentMessage/delta", "live reducer rejected item/agentMessage/delta", map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "missing", "delta": "private payload"}},
		{"bounds", "turn/plan/updated", "live bounds exceeded after turn/plan/updated", map[string]any{"threadId": "a", "turnId": "turn-1", "plan": []any{}, "explanation": strings.Repeat("private", maxTextBytes)}},
		{"untrusted-method", "item/private-payload/mutation", "live reducer rejected unrecognized event", map[string]string{"threadId": "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			s := connectedService(t, f)
			openThread(t, s, "a")
			awaitSequence(t, s, f.event(tc.method, tc.params))
			got := snapshot(t, s, "a")
			if !got.ResyncRequired || got.ResyncReason != tc.want {
				t.Fatal(got.ResyncReason)
			}
		})
	}
}
