package server

import (
	"encoding/json"
	"strings"
	"testing"

	"mirrorbird/internal/session"
)

func TestFileApprovalRequiresMatchedTranscriptPatch(t *testing.T) {
	patch := json.RawMessage(`[{"path":"main.go","kind":{"type":"update"},"diff":"-old\n+new"}]`)
	value := session.Snapshot{
		Thread:  session.Thread{ID: "thread", Turns: []session.Turn{{ID: "turn", Items: []session.Item{{ID: "item", Type: "fileChange", Changes: patch}}}}},
		Pending: []session.PendingRequest{{ID: json.RawMessage(`7`), Method: "item/fileChange/requestApproval", Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","itemId":"item","reason":"apply fix"}`)}},
	}
	response := browserSnapshot(value)
	if len(response.Pending) != 1 || response.Pending[0].DesktopOnly || string(response.Pending[0].Params.Changes) != string(patch) {
		t.Fatalf("missing matched patch: %+v", response.Pending)
	}
	value.Pending[0].DesktopOnly = true
	if !browserSnapshot(value).Pending[0].DesktopOnly {
		t.Fatal("restricted scope became actionable")
	}
	value.Pending[0].DesktopOnly = false
	value.Pending[0].Params = json.RawMessage(`{"threadId":"thread","turnId":"different","itemId":"item","changes":[{"diff":"fake"}]}`)
	response = browserSnapshot(value)
	if !response.Pending[0].DesktopOnly || len(response.Pending[0].Params.Changes) != 0 {
		t.Fatal("unmatched or caller-provided patch accepted")
	}
}

func TestSnapshotForwardsResyncAndDesktopOnlyFlags(t *testing.T) {
	value := session.Snapshot{HistoryTruncated: true, ResyncRequired: true, ResyncReason: "refresh snapshot", ReadOnly: true, Pending: []session.PendingRequest{{ID: json.RawMessage(`"request"`), Method: "item/commandExecution/requestApproval", DesktopOnly: true, Params: json.RawMessage(`{"command":"test"}`)}}}
	data, err := json.Marshal(browserSnapshot(value))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"historyTruncated":true`, `"resyncRequired":true`, `"desktopOnly":true`, `"readOnly":true`} {
		if !strings.Contains(string(data), fragment) {
			t.Fatal(string(data))
		}
	}
}
