package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func largeHistoryTurn(id string, count int) Turn {
	turn := Turn{ID: id, Status: "completed", ItemsView: "full",
		Items: []Item{{ID: "user", Type: "userMessage", Content: rawJSON([]map[string]string{{"type": "text", "text": "request"}})}}}
	for i := 0; i < count-2; i++ {
		turn.Items = append(turn.Items, Item{ID: fmt.Sprint("tool-", i), Type: "commandExecution", Output: strings.Repeat("x", 4200)})
	}
	turn.Items = append(turn.Items, Item{ID: "final", Type: "agentMessage", Phase: "final_answer", Text: "answer"})
	return turn
}

func TestLargeLegacySnapshotAndCompleteItemPagination(t *testing.T) {
	f := newFake()
	thread := testThread("a", false)
	large := largeHistoryTurn("large", 1086)
	thread.Turns = []Turn{largeHistoryTurn("older", 1086), large, largeHistoryTurn("recent", 1086)}
	raw := rawJSON(map[string]any{"thread": thread})
	if len(raw) <= maxRPCBytes || len(raw) >= 16<<20 || len(rawJSON(large)) <= maxProjectionBytes {
		t.Fatal("fixture must reproduce both oversized snapshot and single turn")
	}
	f.threads["a"] = thread
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method == "thread/read" {
			metadata := thread
			metadata.Turns = nil
			return rawJSON(map[string]any{"thread": metadata}), nil, true
		}
		if method != "thread/items/list" {
			return nil, nil, false
		}
		var params struct {
			ThreadID, TurnID, Cursor, SortDirection string
			Limit                                   int
		}
		_ = json.Unmarshal(raw, &params)
		if params.ThreadID != "a" || params.TurnID != large.ID || params.SortDirection != "asc" || params.Limit != 16 {
			t.Fatal(params)
		}
		start, _ := strconv.Atoi(params.Cursor)
		end := min(start+params.Limit, len(large.Items))
		entries := make([]map[string]any, 0, end-start)
		for _, item := range large.Items[start:end] {
			entries = append(entries, map[string]any{"turnId": large.ID, "item": item})
		}
		next := ""
		if end < len(large.Items) {
			next = strconv.Itoa(end)
		}
		return rawJSON(map[string]any{"data": entries, "nextCursor": next}), nil, true
	})
	s := connectedService(t, f)
	openThread(t, s, "a")
	value := snapshot(t, s, "a")
	if value.ResyncRequired || len(value.Thread.Turns) != 3 || len(rawJSON(value)) > maxProjectionBytes {
		t.Fatal("large history did not produce a bounded usable snapshot")
	}
	for _, turn := range value.Thread.Turns {
		if turn.ItemsView != "summary" || len(turn.Items) != 2 || turn.Items[0].ID != "user" || turn.Items[1].ID != "final" {
			t.Fatal("display summary lost the user/final messages")
		}
	}
	var items []Item
	cursor := ""
	for {
		page, err := s.Items(context.Background(), "a", large.ID, cursor, value.HistoryVersion)
		if err != nil || len(rawJSON(page)) > maxProjectionBytes {
			t.Fatal("item page", err)
		}
		items = append(items, page.Items...)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if !reflect.DeepEqual(items, large.Items) {
		t.Fatal("pagination skipped, duplicated or truncated persisted items")
	}
	if got := snapshot(t, s, "a"); !reflect.DeepEqual(got.Thread, value.Thread) || got.Revision != value.Revision {
		t.Fatal("display hydration mutated live authority")
	}
}

func TestSummarySnapshotReReadsActiveTurnAtItsOwnBoundary(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	calls := 0
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "snapshot" {
			return nil, nil, false
		}
		calls++
		var params struct {
			Summary bool
			Limit   int
		}
		_ = json.Unmarshal(raw, &params)
		thread := testThread("a", true)
		turn := thread.Turns[0]
		thread.Turns = nil
		page := turnsPage{Data: []Turn{turn}}
		if calls == 1 {
			if !params.Summary {
				t.Fatal("initial read must request summaries")
			}
			page.Data[0].ItemsView = "summary"
			page.Data = append(page.Data, Turn{ID: "older", Status: "completed", ItemsView: "summary", Items: []Item{}})
			page.NextCursor = "oldest"
		} else if calls == 2 {
			if params.Summary || params.Limit != 1 {
				t.Fatal("active read must be a bounded full snapshot")
			}
			page.Data[0].ItemsView = "full"
			page.Data[0].Items[0].Text = "current"
		} else {
			t.Fatal("unbounded resume loop")
		}
		return f.sequenced(resumeResult{Thread: thread, InitialTurnsPage: &page}), nil, true
	})
	openThread(t, s, "a")
	got := snapshot(t, s, "a")
	if calls != 2 || got.ActiveTurnID != "turn-1" || len(got.Thread.Turns) != 2 ||
		got.Thread.Turns[0].ID != "older" || got.Thread.Turns[1].Items[0].Text != "current" || f.count("history") != 0 {
		t.Fatal("active state or ordering lost")
	}
}

func TestOlderHistoryUsesSummariesAndKeepsDetailsAccessible(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	openThread(t, s, "a")
	value := snapshot(t, s, "a")
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "thread/turns/list" {
			return nil, nil, false
		}
		var params struct {
			ItemsView, SortDirection string
			Limit                    int
		}
		_ = json.Unmarshal(raw, &params)
		if params.ItemsView != "summary" || params.SortDirection != "desc" || params.Limit != maxTurns {
			t.Fatal("history requested unbounded full turns", params)
		}
		return rawJSON(turnsPage{Data: []Turn{
			{ID: "turn-1", Status: "completed", ItemsView: "summary", Items: []Item{}},
			{ID: "older", Status: "completed", ItemsView: "summary", Items: []Item{{ID: "answer", Type: "agentMessage", Text: "answer"}}},
		}, NextCursor: "more"}), nil, true
	})
	page, err := s.History(context.Background(), "a", "before:turn-1", value.HistoryVersion)
	if err != nil || len(page.Turns) != 1 || page.Turns[0].ItemsView != "summary" || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	if got := snapshot(t, s, "a"); !reflect.DeepEqual(got.Thread, value.Thread) {
		t.Fatal("older display history changed live projection")
	}
}

func TestItemPagesRejectStaleAndInvalidData(t *testing.T) {
	for _, scenario := range []string{"version", "wrong-turn", "duplicate", "cursor-loop", "empty-loop", "oversized-item", "history-changed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFake()
			s := connectedService(t, f)
			openThread(t, s, "a")
			value := snapshot(t, s, "a")
			version := value.HistoryVersion
			if scenario == "version" {
				version++
			}
			f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
				if method != "thread/items/list" {
					return nil, nil, false
				}
				item := Item{ID: "item", Type: "agentMessage", Text: "safe"}
				turnID, next := "turn-1", ""
				if scenario == "wrong-turn" {
					turnID = "elsewhere"
				}
				if scenario == "oversized-item" {
					item.Text = strings.Repeat("x", maxTextBytes+1)
				}
				entries := []map[string]any{{"turnId": turnID, "item": item}}
				switch scenario {
				case "duplicate":
					entries = append(entries, entries[0])
				case "cursor-loop":
					next = "cursor"
				case "empty-loop":
					entries, next = []map[string]any{}, "next"
				case "history-changed":
					s.mu.Lock()
					s.projections["a"].snapshot.HistoryVersion++
					s.mu.Unlock()
				}
				return rawJSON(map[string]any{"data": entries, "nextCursor": next}), nil, true
			})
			_, err := s.Items(context.Background(), "a", "turn-1", "cursor", version)
			want := ErrResyncRequired
			if scenario == "version" || scenario == "history-changed" {
				want = ErrStateChanged
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			if scenario == "version" && f.count("thread/items/list") != 0 {
				t.Fatal("stale request reached app-server")
			}
		})
	}
}

func TestItemPageShrinksWithoutAdvancingCursor(t *testing.T) {
	f := newFake()
	s := connectedService(t, f)
	openThread(t, s, "a")
	var limits []int
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method != "thread/items/list" {
			return nil, nil, false
		}
		var params struct {
			Limit  int
			Cursor string
		}
		_ = json.Unmarshal(raw, &params)
		limits = append(limits, params.Limit)
		if params.Cursor != "same" {
			t.Fatal("read retry skipped data")
		}
		entries := []map[string]any{}
		for i := 0; i < params.Limit; i++ {
			item := Item{ID: strconv.Itoa(i), Type: "commandExecution", Output: strings.Repeat("x", 250<<10), Command: strings.Repeat("y", 50<<10)}
			entries = append(entries, map[string]any{"turnId": "turn-1", "item": item})
		}
		return rawJSON(map[string]any{"data": entries, "nextCursor": "next"}), nil, true
	})
	page, err := s.Items(context.Background(), "a", "turn-1", "same", snapshot(t, s, "a").HistoryVersion)
	if err != nil || !reflect.DeepEqual(limits, []int{16, 8}) || len(page.Items) != 8 || len(rawJSON(page)) > maxProjectionBytes {
		t.Fatal(limits, err)
	}
}
