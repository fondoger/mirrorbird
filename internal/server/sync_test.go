package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"mirrorbird/internal/session"
)

func syncServer(t *testing.T) (*Server, *fakeBackend) {
	t.Helper()
	s, b, _ := testServer(t)
	// Match the production Read contract: a caller owns an immutable copy.
	b.onRead = func(_ context.Context, id string, options session.ReadOptions) (session.Snapshot, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		raw, err := json.Marshal(b.value)
		if err != nil {
			return session.Snapshot{}, err
		}
		var value session.Snapshot
		if err = json.Unmarshal(raw, &value); err != nil {
			return value, err
		}
		if options.Resync {
			b.epochs[id]++
		}
		value.Thread.ID = id
		value.ProjectionGeneration = b.epochs[id]
		return value, nil
	}
	return s, b
}

func getSync(t *testing.T, s *Server, cursor string) (syncResponse, int) {
	t.Helper()
	w := request(s, "", "GET", "/api/threads/thread/sync?cursor="+cursor, "")
	if w.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", w.Code, w.Body)
	}
	var result syncResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result, w.Body.Len()
}

func jsonTree(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err = json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	return tree
}

func applyTestPatch(t *testing.T, tree any, patch snapshotPatch) {
	t.Helper()
	var edit func(any, int) any
	edit = func(node any, depth int) any {
		if depth == len(patch.Path) {
			var value any
			if patch.Value != nil {
				if err := json.Unmarshal(patch.Value, &value); err != nil {
					t.Fatal(err)
				}
			}
			switch patch.Op {
			case "set":
				return value
			case "append":
				return node.(string) + value.(string)
			case "splice":
				op := value.(map[string]any)
				index, count := int(op["index"].(float64)), int(op["deleteCount"].(float64))
				old := node.([]any)
				result := append([]any{}, old[:index]...)
				result = append(result, op["items"].([]any)...)
				return append(result, old[index+count:]...)
			default:
				t.Fatalf("unknown patch: %s", patch.Op)
			}
		}
		switch key := patch.Path[depth].(type) {
		case string:
			object := node.(map[string]any)
			if depth == len(patch.Path)-1 && patch.Op == "remove" {
				delete(object, key)
			} else {
				object[key] = edit(object[key], depth+1)
			}
		case float64:
			list := node.([]any)
			list[int(key)] = edit(list[int(key)], depth+1)
		default:
			t.Fatalf("unexpected path component: %#v", key)
		}
		return node
	}
	edit(tree, 0)
}

func largeSyncSnapshot() session.Snapshot {
	value := session.Snapshot{Revision: 1, ProjectionGeneration: 1, Thread: session.Thread{ID: "thread", Turns: []session.Turn{}}, Pending: []session.PendingRequest{}, Queue: []session.QueuedMessage{}}
	for i := 0; i < 12; i++ {
		value.Thread.Turns = append(value.Thread.Turns, session.Turn{
			ID: fmt.Sprintf("turn-%d", i), Status: "completed",
			Items: []session.Item{{ID: fmt.Sprintf("answer-%d", i), Type: "agentMessage", Text: strings.Repeat("History.\n", 12_000)}},
		})
	}
	value.Thread.Turns[11].Status = "inProgress"
	value.Thread.Turns[11].Items = append(value.Thread.Turns[11].Items, session.Item{
		ID: "command", Type: "commandExecution", Status: "inProgress", Output: strings.Repeat("output\n", 8_000),
	})
	value.ActiveTurnID = "turn-11"
	return value
}

func TestSyncTextAndOutputTransferOnlySuffixes(t *testing.T) {
	s, b := syncServer(t)
	b.value = largeSyncSnapshot()
	full, fullBytes := getSync(t, s, "")
	if full.Mode != "snapshot" || fullBytes < 1_000_000 {
		t.Fatalf("expected large initial snapshot, got %s / %d bytes", full.Mode, fullBytes)
	}
	tree, cursor, transferred := jsonTree(t, full.Snapshot), full.Cursor, 0
	for i := 0; i < 20; i++ {
		b.value.Thread.Turns[11].Items[0].Text += "追加 😀 \"<text>\"\n"
		b.value.Thread.Turns[11].Items[1].Output += fmt.Sprintf("progress %d\n", i)
		b.value.Revision++
		next, size := getSync(t, s, cursor)
		if next.Mode != "delta" || next.BaseCursor != cursor || next.Patch == nil || size > 1024 {
			t.Fatalf("expected small delta, got %s / %d bytes", next.Mode, size)
		}
		for _, patch := range *next.Patch {
			applyTestPatch(t, tree, patch)
		}
		if !reflect.DeepEqual(tree, jsonTree(t, browserSnapshot(b.value))) {
			t.Fatal("delta did not reconstruct current snapshot")
		}
		cursor, transferred = next.Cursor, transferred+size
	}
	if len(s.syncBases) != 1 || s.syncBytes > maxSyncBytes {
		t.Fatal("streaming accumulated baselines")
	}
	t.Logf("initial=%d bytes; 20 updates=%d bytes; full-snapshot equivalent>%d bytes", fullBytes, transferred, fullBytes*20)
}

func TestSyncStructuralChangesAndBrowserFiltering(t *testing.T) {
	s, b := syncServer(t)
	b.value = largeSyncSnapshot()
	full, _ := getSync(t, s, "")
	tree, cursor := jsonTree(t, full.Snapshot), full.Cursor
	changes := []func(){
		func() {
			b.value.Pending = []session.PendingRequest{{ID: json.RawMessage(`900719925474099312345`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"thread","command":"pwd","token":"never-forward-this"}`)}}
			b.value.Queue = []session.QueuedMessage{{ID: "queued", Status: "queued", MessageInput: session.MessageInput{Text: "next"}}}
		},
		func() {
			b.value.Queue[0].Status = "failed"
			b.value.Queue[0].Error = "state_changed"
			b.value.Thread.Model = "new-model"
		},
		func() {
			b.value.Thread.Turns[11].Items[0].Text = "replacement"
			b.value.Thread.Turns[11].Items[1].Output = ""
			b.value.Thread.Turns[11].Items[1].Status = "completed"
			exit := 0
			b.value.Thread.Turns[11].Items[1].ExitCode = &exit
			b.value.ActiveTurnID = ""
			b.value.Thread.Turns[11].Status = "completed"
		},
		func() {
			b.value.Thread.Turns = append(b.value.Thread.Turns[1:], session.Turn{ID: "new", Status: "inProgress", Items: []session.Item{{ID: "user", Type: "userMessage", Content: json.RawMessage(`[{"type":"text","text":"continue"}]`)}}})
			b.value.HistoryTruncated, b.value.HistoryCursor = true, "before:turn-1"
			b.value.Pending, b.value.Queue = []session.PendingRequest{}, []session.QueuedMessage{}
		},
		func() {
			turn := &b.value.Thread.Turns[11]
			turn.Items = append([]session.Item{{ID: "new-first", Type: "reasoning", Summary: []string{"one", "two"}}}, turn.Items...)
			turn.Diff, turn.Explanation = "patch", "plan"
			turn.Plan = json.RawMessage(`{"steps":["one","two"]}`)
		},
		func() {
			items := b.value.Thread.Turns[11].Items
			b.value.Thread.Turns[11].Items = []session.Item{items[1], items[0]}
			b.value.HistoryVersion++
		},
		func() {
			b.value.Thread.Turns[11].Items = nil
			b.value.ResyncRequired = true
			b.value.ResyncReason = "gap"
		},
	}
	for i, change := range changes {
		change()
		b.value.Revision++
		next, _ := getSync(t, s, cursor)
		if next.Mode != "delta" || next.Patch == nil {
			t.Fatalf("change %d was not incremental: %#v", i, next)
		}
		for _, patch := range *next.Patch {
			if strings.Contains(string(patch.Value), "never-forward-this") {
				t.Fatal("approval secret in delta")
			}
			applyTestPatch(t, tree, patch)
		}
		if !reflect.DeepEqual(tree, jsonTree(t, browserSnapshot(b.value))) {
			t.Fatalf("structural change %d diverged", i)
		}
		cursor = next.Cursor
	}
}

func TestSyncBaselinesBoundedIsolatedAndReset(t *testing.T) {
	s, b := syncServer(t)
	a, _ := getSync(t, s, "")
	other, _ := getSync(t, s, "")
	advanced, _ := getSync(t, s, a.Cursor)
	if advanced.Mode != "delta" || len(*advanced.Patch) != 0 {
		t.Fatal("unchanged state should be an empty delta")
	}
	stillValid, _ := getSync(t, s, other.Cursor)
	if stillValid.Mode != "delta" {
		t.Fatal("one tab consumed another tab's baseline")
	}
	missed, _ := getSync(t, s, a.Cursor)
	if missed.ResetReason != "baseline_missing" {
		t.Fatal("lost response did not reset explicitly")
	}
	b.epochs["thread"]++
	replaced, _ := getSync(t, s, missed.Cursor)
	if replaced.ResetReason != "projection_changed" {
		t.Fatal("new projection reused old baseline")
	}
	s.syncBases[replaced.Cursor].used = time.Now().Add(-syncBaseTTL - time.Second)
	expired, _ := getSync(t, s, replaced.Cursor)
	if expired.ResetReason != "baseline_missing" {
		t.Fatal("expired baseline used")
	}
	for i := 0; i < maxSyncBases+4; i++ {
		getSync(t, s, "")
	}
	if len(s.syncBases) != maxSyncBases {
		t.Fatal("baseline count unbounded")
	}
	b.value = largeSyncSnapshot()
	for i := 0; i < 18; i++ {
		getSync(t, s, "")
	}
	if s.syncBytes > maxSyncBytes || len(s.syncBases) >= maxSyncBases {
		t.Fatal("byte budget not enforced")
	}
	s.SetSharing(false)
	if len(s.syncBases) != 0 || s.syncBytes != 0 {
		t.Fatal("Stop retained browser state")
	}
	w := request(s, "", "GET", "/api/threads/thread/sync?cursor="+expired.Cursor, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("stopped sync admitted: %d", w.Code)
	}
	s.SetSharing(true)
	restarted, _ := getSync(t, s, expired.Cursor)
	if restarted.ResetReason != "baseline_missing" {
		t.Fatal("sharing restart reused a cursor")
	}
}

func TestSyncAdmissionAndExplicitResync(t *testing.T) {
	s, b := syncServer(t)
	full, _ := getSync(t, s, "")
	for _, query := range []string{"cursor=" + strings.Repeat("x", 129), "cursor=a&cursor=b", "resync=yes", "archived=true"} {
		if w := request(s, "", "GET", "/api/threads/thread/sync?"+query, ""); w.Code != http.StatusBadRequest {
			t.Fatalf("accepted query %q: %d", query, w.Code)
		}
	}
	w := request(s, "", "GET", "/api/threads/thread/sync?cursor="+full.Cursor+"&resync=true", "")
	var reset syncResponse
	if err := json.Unmarshal(w.Body.Bytes(), &reset); err != nil || reset.ResetReason != "resync" {
		t.Fatalf("explicit resync: %s", w.Body)
	}
	b.onRead = func(context.Context, string, session.ReadOptions) (session.Snapshot, error) {
		s.SetSharing(false)
		s.SetSharing(true)
		return b.value, nil
	}
	w = request(s, "", "GET", "/api/threads/thread/sync", "")
	if w.Code != http.StatusForbidden || len(s.syncBases) != 0 {
		t.Fatalf("late state crossed lifetime: %d", w.Code)
	}
}

func TestSyncChangeBudgetAndOversizedBaseline(t *testing.T) {
	s, b := syncServer(t)
	b.value.Thread.Turns = make([]session.Turn, 30)
	for i := range b.value.Thread.Turns {
		turn := &b.value.Thread.Turns[i]
		turn.ID = fmt.Sprint("turn-", i)
		turn.Items = make([]session.Item, 100)
		for j := range turn.Items {
			turn.Items[j] = session.Item{ID: fmt.Sprint("item-", j), Type: "agentMessage", Text: "before"}
		}
	}
	full, _ := getSync(t, s, "")
	for i := range b.value.Thread.Turns {
		for j := range b.value.Thread.Turns[i].Items {
			b.value.Thread.Turns[i].Items[j].Text = "after"
		}
	}
	b.value.Revision++
	changed, _ := getSync(t, s, full.Cursor)
	if changed.Mode != "snapshot" || changed.ResetReason != "change_limit" {
		t.Fatal("oversized patch did not reset explicitly")
	}
	// Browser approval display copies can exceed a projection's own budget.
	b.value.Thread.Turns = []session.Turn{{ID: "large", Items: []session.Item{
		{ID: "large-item", Type: "agentMessage", Text: strings.Repeat("x", maxSyncBytes)},
	}}}
	capacity, _ := getSync(t, s, changed.Cursor)
	if capacity.Mode != "snapshot" || capacity.ResetReason != "capacity" || capacity.Cursor != "" || s.syncBytes > maxSyncBytes {
		t.Fatal("unretainable baseline was cached or misrepresented")
	}
}

func BenchmarkSnapshotDelta(b *testing.B) {
	before := browserSnapshot(largeSyncSnapshot())
	after := browserSnapshot(largeSyncSnapshot())
	after.Thread.Turns[11].Items[0].Text += "appended"
	after.Revision++
	b.Run("incremental", func(b *testing.B) {
		for b.Loop() {
			patch := []snapshotPatch{}
			if err := diffSnapshot(reflect.ValueOf(before), reflect.ValueOf(after), nil, &patch); err != nil {
				b.Fatal(err)
			}
			_ = syncWeight(reflect.ValueOf(after))
			if _, err := json.Marshal(patch); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("full", func(b *testing.B) {
		for b.Loop() {
			if _, err := json.Marshal(after); err != nil {
				b.Fatal(err)
			}
		}
	})
}
