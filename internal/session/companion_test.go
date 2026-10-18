package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"mirrorbird/internal/attachment"
)

func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestQueueSurvivesBrowserDisconnectAndRunsFIFO(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	openThread(t, s, "a")
	ctx, cancel := context.WithCancel(context.Background())
	for _, text := range []string{"first", "second"} {
		if _, err := s.Enqueue(ctx, "a", text, MessageInput{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	if f.count("turn/start") != 0 || len(snapshot(t, s, "a").Queue) != 2 {
		t.Fatal("queued messages started during active turn")
	}
	seq := f.event("turn/completed", map[string]any{"threadId": "a", "turn": Turn{ID: "turn-1", Status: "completed"}})
	awaitSequence(t, s, seq)
	waitFor(t, func() bool { return len(snapshot(t, s, "a").Queue) == 1 })
	value := snapshot(t, s, "a")
	if value.Queue[0].Text != "second" {
		t.Fatal(value.Queue)
	}
	f.mu.Lock()
	var first map[string]json.RawMessage
	for _, call := range f.calls {
		if call.method == "turn/start" {
			_ = json.Unmarshal(call.params, &first)
			break
		}
	}
	f.mu.Unlock()
	var content []struct{ Text string }
	_ = json.Unmarshal(first["input"], &content)
	if len(content) != 1 || content[0].Text != "first" {
		t.Fatal(content)
	}
	if _, err := s.Act(context.Background(), "a", ThreadAction{Kind: "queue-delete", QueueID: "second"}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	pins := s.projections["a"].pins
	s.mu.Unlock()
	if pins != 0 {
		t.Fatal("queue watch pin leaked", pins)
	}
}

func TestOnlyFirstQueueMessageCanGuideAndGuideStartsNewTurn(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	openThread(t, s, "a")
	for _, text := range []string{"first", "second"} {
		if _, err := s.Enqueue(context.Background(), "a", text, MessageInput{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Act(context.Background(), "a", ThreadAction{Kind: "queue-guide", QueueID: "second"}); !errors.Is(err, ErrStateChanged) {
		t.Fatal(err)
	}
	if _, err := s.Act(context.Background(), "a", ThreadAction{Kind: "queue-guide", QueueID: "first"}); err != nil {
		t.Fatal(err)
	}
	if f.count("turn/interrupt") != 1 || f.count("turn/start") != 1 || f.count("turn/steer") != 0 {
		t.Fatal("guide must interrupt, then start")
	}
	value := snapshot(t, s, "a")
	if len(value.Queue) != 1 || value.Queue[0].Text != "second" {
		t.Fatal(value.Queue)
	}
}

func TestFailedQueueIsNotReplayedByRefresh(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	openThread(t, s, "a")
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method == "turn/start" {
			return nil, errors.New("connection dropped"), true
		}
		return nil, nil, false
	})
	if _, err := s.Enqueue(context.Background(), "a", "one", MessageInput{Text: "only once"}); err != nil {
		t.Fatal(err)
	}
	awaitSequence(t, s, f.event("turn/completed", map[string]any{"threadId": "a", "turn": Turn{ID: "turn-1", Status: "completed"}}))
	waitFor(t, func() bool {
		value := snapshot(t, s, "a")
		return len(value.Queue) == 1 && value.Queue[0].Status == "failed"
	})
	f.mu.Lock()
	f.threads["a"] = testThread("a", false)
	f.mu.Unlock()
	value, err := s.Read(context.Background(), "a", ReadOptions{Resync: true})
	if err != nil || len(value.Queue) != 1 || value.Queue[0].Status != "failed" {
		t.Fatal(value, err)
	}
	if f.count("turn/start") != 1 {
		t.Fatal("uncertain queue was replayed")
	}
}

func TestHistoryUsesLiveBoundaryWithoutHydratingOlderState(t *testing.T) {
	f := newFake()
	var turns []Turn
	for i := 0; i < 35; i++ {
		turns = append(turns, Turn{ID: fmt.Sprintf("t-%02d", i), Status: "completed", Items: []Item{}})
	}
	thread := testThread("a", false)
	thread.Turns = turns
	f.threads["a"] = thread
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "thread/turns/list" {
			return nil, nil, false
		}
		var params struct{ Cursor string }
		_ = json.Unmarshal(raw, &params)
		start, end, next := 5, 35, "older"
		if params.Cursor == "older" {
			start, end, next = 0, 5, ""
		}
		page := append([]Turn{}, turns[start:end]...)
		reverseTurns(page)
		return rawJSON(map[string]any{"data": page, "nextCursor": next}), nil, true
	})
	s := connectedService(t, f)
	openThread(t, s, "a")
	value := snapshot(t, s, "a")
	if value.HistoryCursor != "before:t-05" || len(value.Thread.Turns) != 30 {
		t.Fatal(value.HistoryCursor, len(value.Thread.Turns))
	}
	page, err := s.History(context.Background(), "a", value.HistoryCursor, value.HistoryVersion)
	if err != nil || len(page.Turns) != 0 || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	page, err = s.History(context.Background(), "a", page.NextCursor, value.HistoryVersion)
	if err != nil || len(page.Turns) != 5 || page.Turns[0].ID != "t-00" {
		t.Fatal(page, err)
	}
	if len(snapshot(t, s, "a").Thread.Turns) != 30 {
		t.Fatal("history changed live projection")
	}
	if _, err = s.History(context.Background(), "a", value.HistoryCursor, value.HistoryVersion+1); !errors.Is(err, ErrStateChanged) {
		t.Fatal(err)
	}
}

func TestEditRevertsAndRehydratesBeforeStarting(t *testing.T) {
	f := newFake()
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method == "thread/revert" {
			f.mu.Lock()
			thread := f.threads["a"]
			thread.Turns = []Turn{}
			f.threads["a"] = thread
			f.mu.Unlock()
			f.event("thread/reverted", map[string]string{"threadId": "a"})
			return rawJSON(map[string]any{"thread": thread}), nil, true
		}
		return nil, nil, false
	})
	s := connectedService(t, f)
	openThread(t, s, "a")
	version := snapshot(t, s, "a").HistoryVersion
	_, err := s.Act(context.Background(), "a", ThreadAction{Kind: "edit", TurnID: "turn-1", MessageInput: MessageInput{Text: "replacement"}})
	if err != nil {
		t.Fatal(err)
	}
	value := snapshot(t, s, "a")
	if value.ActiveTurnID != "turn-new" || value.HistoryVersion == version || f.count("thread/revert") != 1 || f.count("watch") != 1 {
		t.Fatal(value)
	}
}

func TestSkillsResolveInstalledHandlesAndNeverBrowserPaths(t *testing.T) {
	f := newFake()
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method == "skills/list" {
			return json.RawMessage(`{"data":[{"skills":[{"name":"review","description":"Review changes","path":"C:\\repo\\SKILL.md","enabled":true},{"name":"disabled","path":"C:\\bad","enabled":false}],"errors":[]}]}`), nil, true
		}
		return nil, nil, false
	})
	s := connectedService(t, f)
	openThread(t, s, "a")
	options, err := s.Options(context.Background(), "skills", "a", "")
	if err != nil || len(options.Skills) != 1 {
		t.Fatal(options, err)
	}
	if _, err = s.resolveMessage(context.Background(), "a", MessageInput{Text: "test", SkillID: "C:\\arbitrary"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	input, err := s.resolveMessage(context.Background(), "a", MessageInput{Text: "test", SkillID: options.Skills[0].ID})
	if err != nil || input.SkillPath != `C:\repo\SKILL.md` || input.SkillName != "review" {
		t.Fatal(input, err)
	}
}

func TestQueueSurvivesFailedExplicitResync(t *testing.T) {
	f := newFake()
	f.threads["a"] = testThread("a", true)
	s := connectedService(t, f)
	openThread(t, s, "a")
	if _, err := s.Enqueue(context.Background(), "a", "saved", MessageInput{Text: "keep this"}); err != nil {
		t.Fatal(err)
	}
	f.setHook(func(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error, bool) {
		if method == "snapshot" {
			return nil, errors.New("temporary snapshot failure"), true
		}
		return nil, nil, false
	})
	if _, err := s.Read(context.Background(), "a", ReadOptions{Resync: true}); !errors.Is(err, ErrResyncRequired) {
		t.Fatal(err)
	}
	s.mu.Lock()
	kept := len(s.projections["a"].queue)
	s.mu.Unlock()
	if kept != 1 {
		t.Fatal("acknowledged queue was lost")
	}
	f.setHook(nil)
	value, err := s.Read(context.Background(), "a", ReadOptions{Resync: true})
	if err != nil || len(value.Queue) != 1 || value.Queue[0].Text != "keep this" {
		t.Fatal(value, err)
	}
	if f.count("turn/start") != 0 {
		t.Fatal("refresh sent a queued message into an active turn")
	}
}

func TestCompanionUnsupportedIsKnownRejection(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	openThread(t, s, "a")
	f.setHook(func(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error, bool) {
		if method == "thread/settings/update" || method == "model/list" {
			return nil, attachment.ErrUnsupportedOperation, true
		}
		return nil, nil, false
	})
	if _, err := s.Options(context.Background(), "models", "", ""); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := s.Act(context.Background(), "a", ThreadAction{Kind: "settings", MessageInput: MessageInput{Model: "test"}}); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if snapshot(t, s, "a").ResyncRequired {
		t.Fatal("known rejection must not corrupt the projection")
	}
}

func TestOldShimExplainsRestartWithoutInvalidatingProjection(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	openThread(t, s, "a")
	f.setHook(func(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error, bool) {
		switch method {
		case "project/list", "model/list", "skills/list", "thread/compact/start":
			return nil, attachment.ErrOutdatedShim, true
		}
		return nil, nil, false
	})
	for _, kind := range []string{"projects", "models", "skills"} {
		if _, err := s.Options(context.Background(), kind, "a", ""); !errors.Is(err, ErrAttachmentOutdated) {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	if _, err := s.Act(context.Background(), "a", ThreadAction{Kind: "compact"}); !errors.Is(err, ErrAttachmentOutdated) {
		t.Fatal(err)
	}
	if snapshot(t, s, "a").ResyncRequired {
		t.Fatal("old shim rejection invalidated the conversation")
	}
}

func TestCreateUsesConfiguredProjectAndQueuesInitialOptions(t *testing.T) {
	f := newFake()
	f.setHook(func(_ context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		switch method {
		case "project/list":
			return json.RawMessage(`{"data":[{"id":"project","name":"Project","roots":[{"path":"C:\\repo\\project"}]}]}`), nil, true
		case "thread/start":
			var params map[string]any
			if json.Unmarshal(raw, &params) != nil || params["projectId"] != "project" || params["cwd"] != `C:\repo\project` || params["model"] != "chosen" || len(params) != 3 {
				t.Errorf("creation options: %s", raw)
			}
			thread := testThread("created", false)
			thread.Turns = []Turn{}
			f.mu.Lock()
			f.threads["created"] = thread
			f.mu.Unlock()
			return rawJSON(map[string]any{"thread": thread}), nil, true
		}
		return nil, nil, false
	})
	s := connectedService(t, f)
	if _, err := s.CreateThread(context.Background(), "not-installed", MessageInput{Text: "hello"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	data, err := s.CreateThread(context.Background(), "project", MessageInput{Text: "hello", Model: "chosen", Effort: "high"})
	if err != nil || string(data) != `{"threadId":"created"}` {
		t.Fatal(string(data), err)
	}
	waitFor(t, func() bool { return f.count("turn/start") == 1 })
	f.mu.Lock()
	var params map[string]any
	for _, call := range f.calls {
		if call.method == "turn/start" {
			_ = json.Unmarshal(call.params, &params)
		}
	}
	f.mu.Unlock()
	if params["model"] != "chosen" || params["effort"] != "high" {
		t.Fatal(params)
	}
}

func TestCompanionRenameSettingsAndForkUseExactProtocol(t *testing.T) {
	f := newFake()
	f.setHook(func(_ context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		switch method {
		case "thread/name/set":
			f.event("thread/name/updated", map[string]string{"threadId": "a", "threadName": "renamed"})
			return json.RawMessage(`{}`), nil, true
		case "thread/settings/update":
			f.event("thread/settings/updated", map[string]any{"threadId": "a", "threadSettings": map[string]string{"model": "chosen", "effort": "high", "cwd": "C:/test"}})
			return json.RawMessage(`{}`), nil, true
		case "thread/fork":
			var params map[string]any
			_ = json.Unmarshal(raw, &params)
			if params["lastTurnId"] != "turn-1" || params["excludeTurns"] != true || params["deferGoalContinuation"] != true || len(params) != 4 {
				t.Errorf("fork options: %s", raw)
			}
			return json.RawMessage(`{"thread":{"id":"forked"}}`), nil, true
		}
		return nil, nil, false
	})
	s := connectedService(t, f)
	openThread(t, s, "a")
	if _, err := s.Act(context.Background(), "a", ThreadAction{Kind: "rename", Name: " renamed "}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Act(context.Background(), "a", ThreadAction{Kind: "settings", MessageInput: MessageInput{Model: "chosen", Effort: "high"}}); err != nil {
		t.Fatal(err)
	}
	value := snapshot(t, s, "a")
	if value.Thread.Name != "renamed" || value.Thread.Model != "chosen" || value.Thread.Effort != "high" {
		t.Fatal(value.Thread)
	}
	data, err := s.Act(context.Background(), "a", ThreadAction{Kind: "fork", TurnID: "turn-1"})
	if err != nil || string(data) != `{"threadId":"forked"}` {
		t.Fatal(string(data), err)
	}
}

func TestCatalogueSearchMatchesProjectPathAndSourceWithoutLosingPages(t *testing.T) {
	f := newFake()
	f.threads = map[string]Thread{}
	for i := 0; i < 40; i++ {
		thread := testThread(fmt.Sprintf("t-%02d", i), false)
		thread.CWD = `C:\repo\another`
		if i == 28 || i == 39 {
			thread.CWD = `C:\repo\needle`
			thread.Source = json.RawMessage(`"vscode"`)
		}
		f.threads[thread.ID] = thread
	}
	s := connectedService(t, f)
	for _, query := range []string{"needle", "Codex", "ChatGPT Desktop", "VS Code"} {
		cursor := ""
		ids := []string{}
		for pageCount := 0; pageCount < 30; pageCount++ {
			page, err := s.ListThreads(context.Background(), ListOptions{Search: query, Limit: 2, Cursor: cursor, Archived: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, thread := range page.Threads {
				ids = append(ids, thread.ID)
			}
			cursor = page.NextCursor
			if cursor == "" {
				break
			}
		}
		if cursor != "" || (query == "needle" && fmt.Sprint(ids) != "[t-28 t-39]") ||
			(query == "VS Code" && len(ids) != 0) || ((query == "Codex" || query == "ChatGPT Desktop") && len(ids) != 40) {
			t.Fatal(query, ids, cursor)
		}
	}
	if f.count("snapshot") != 0 || f.count("thread/resume") != 0 || f.count("history") != 0 {
		t.Fatal("search hydrated transcripts")
	}
}
