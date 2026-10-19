package attachment

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"mirrorbird/internal/bridge"
)

type frameWriter chan []byte

func (w frameWriter) Write(b []byte) (int, error) { w <- append([]byte(nil), b...); return len(b), nil }
func readyHost(t *testing.T) (*Host, frameWriter) {
	t.Helper()
	up := make(frameWriter, 64)
	mux := bridge.New(up, io.Discard)
	t.Cleanup(func() { mux.Close(nil) })
	if err := mux.FromDesktop([]byte(`{"id":0,"method":"initialize","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	var first struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(<-up, &first)
	if err := mux.FromServer([]byte(fmt.Sprintf(`{"id":%s,"result":{}}`, first.ID))); err != nil {
		t.Fatal(err)
	}
	return &Host{Mux: mux}, up
}
func hostRequest(t *testing.T, mux *bridge.Mux, id string, params map[string]any) {
	t.Helper()
	frame := encode(map[string]any{"id": json.RawMessage(id), "method": "item/commandExecution/requestApproval", "params": params})
	if err := mux.FromServer(frame); err != nil {
		t.Fatal(err)
	}
}
func simpleParams() map[string]any {
	return map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "cmd", "command": "git status", "cwd": "C:/test"}
}

func TestHostSnapshotPreservesExactResponseSequence(t *testing.T) {
	h, up := readyHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type outcome struct {
		data json.RawMessage
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		data, err := h.dispatch(ctx, "snapshot", json.RawMessage(`{"threadId":"a"}`))
		done <- outcome{data, err}
	}()
	var call struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ThreadID     string `json:"threadId"`
			ExcludeTurns bool   `json:"excludeTurns"`
			Page         struct {
				Limit     int    `json:"limit"`
				ItemsView string `json:"itemsView"`
				Direction string `json:"sortDirection"`
			} `json:"initialTurnsPage"`
		} `json:"params"`
	}
	select {
	case raw := <-up:
		if err := json.Unmarshal(raw, &call); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if call.Method != "thread/resume" || call.Params.ThreadID != "a" || !call.Params.ExcludeTurns || call.Params.Page.Limit != 30 || call.Params.Page.ItemsView != "full" || call.Params.Page.Direction != "desc" {
		t.Fatal(call)
	}
	if err := h.Mux.FromServer(encode(map[string]any{"id": call.ID, "result": map[string]any{"thread": map[string]string{"id": "a"}}})); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := h.Mux.FromServer([]byte(`{"method":"thread/name/updated","params":{"threadId":"a","threadName":"new"}}`)); err != nil {
			t.Fatal(err)
		}
	}
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	var response struct {
		Sequence uint64          `json:"sequence"`
		Result   json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(out.data, &response)
	if response.Sequence != 2 || h.Mux.Status().Sequence != 7 || len(response.Result) == 0 {
		t.Fatal(string(out.data))
	}
}

func TestHostSnapshotRequestsBoundedSummaryAndActivePages(t *testing.T) {
	for _, tc := range []struct {
		params, view string
		limit        int
	}{
		{`{"threadId":"a","summary":true}`, "summary", 30},
		{`{"threadId":"a","limit":1}`, "full", 1},
	} {
		h, up := readyHost(t)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := h.dispatch(ctx, "snapshot", json.RawMessage(tc.params))
			done <- err
		}()
		var call struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Page struct {
					Limit     int    `json:"limit"`
					ItemsView string `json:"itemsView"`
				} `json:"initialTurnsPage"`
			} `json:"params"`
		}
		select {
		case data := <-up:
			if err := json.Unmarshal(data, &call); err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if call.Params.Page.Limit != tc.limit || call.Params.Page.ItemsView != tc.view {
			t.Fatal(call.Params.Page)
		}
		if err := h.Mux.FromServer(encode(map[string]any{"id": call.ID, "result": map[string]any{}})); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostPendingSnapshotUsesOwnWatermark(t *testing.T) {
	h, _ := readyHost(t)
	hostRequest(t, h.Mux, `9007199254740993`, simpleParams())
	data, err := h.dispatch(context.Background(), "pending/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Sequence uint64            `json:"sequence"`
		Requests []json.RawMessage `json:"requests"`
	}
	_ = json.Unmarshal(data, &result)
	if result.Sequence != 2 || len(result.Requests) != 1 || !strings.Contains(string(result.Requests[0]), "9007199254740993") {
		t.Fatal(string(data))
	}
	if err := h.Mux.FromServer([]byte(`{"method":"serverRequest/resolved","params":{"requestId":9007199254740993}}`)); err != nil {
		t.Fatal(err)
	}
	data, err = h.dispatch(context.Background(), "pending/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(data, &result)
	if result.Sequence != 3 || len(result.Requests) != 0 {
		t.Fatal(string(data))
	}
}

func TestHostWatchGenerationReleaseDoesNotDetachNewWatch(t *testing.T) {
	metadata, err := NewMetadata("test", "codex.exe", 1)
	if err != nil {
		t.Fatal(err)
	}
	h, up := readyHost(t)
	h.Metadata = metadata
	server, client := net.Pipe()
	defer client.Close()
	go h.serveConnection(context.Background(), server)
	encoder, reader := json.NewEncoder(client), bufio.NewReader(client)
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	id := 0
	call := func(method string, params any) {
		t.Helper()
		id++
		if err := encoder.Encode(packet{ID: fmt.Sprint(id), Method: method, Params: encode(params)}); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var response packet
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatal(err)
		}
		if response.Error != nil {
			t.Fatal(response.Error)
		}
	}
	call("hello", map[string]string{"capability": metadata.Capability})
	call("watch", map[string]string{"threadId": "a", "watchId": "old"})
	call("watch", map[string]string{"threadId": "a", "watchId": "new"})
	call("unwatch", map[string]string{"threadId": "a", "watchId": "old"})
	if h.Mux.Status().WatchedThreads != 1 {
		t.Fatal("old watch released new share")
	}
	if err := h.Mux.FromDesktop([]byte(`{"id":4,"method":"thread/unsubscribe","params":{"threadId":"a"}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-up:
		t.Fatal("new share unprotected", string(data))
	default:
	}
	call("unwatch", map[string]string{"threadId": "a", "watchId": "new"})
	if h.Mux.Status().WatchedThreads != 0 {
		t.Fatal("new watch not released")
	}
}

func TestHostLosslessApprovalAndDesktopWinner(t *testing.T) {
	h, up := readyHost(t)
	hostRequest(t, h.Mux, `9007199254740993`, simpleParams())
	hostRequest(t, h.Mux, `"9007199254740993"`, simpleParams())
	if err := h.respond(json.RawMessage(`9007199254740993.0`), json.RawMessage(`{"decision":"accept"}`)); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(<-up, &response)
	if string(response.ID) != `9007199254740993` {
		t.Fatal("ID precision/type changed", string(response.ID))
	}
	if err := h.Mux.FromDesktop([]byte(`{"id":"9007199254740993","result":{"decision":"decline"}}`)); err != nil {
		t.Fatal(err)
	}
	<-up
	if err := h.respond(json.RawMessage(`"9007199254740993"`), json.RawMessage(`{"decision":"accept"}`)); !errors.Is(err, ErrRequestResolved) {
		t.Fatal(err)
	}
	select {
	case data := <-up:
		t.Fatal("duplicate approval", string(data))
	default:
	}
}

func TestPhoneApprovalPolicyRejectsUnexplainedScopes(t *testing.T) {
	for _, field := range []string{"additionalPermissions", "networkApprovalContext", "environmentId", "kind", "availableDecisions"} {
		t.Run(field, func(t *testing.T) {
			params := simpleParams()
			switch field {
			case "kind":
				params[field] = "stdin"
			case "availableDecisions":
				params[field] = []string{"acceptForSession", "decline"}
			case "environmentId":
				params[field] = "remote"
			default:
				params[field] = map[string]bool{"scope": true}
			}
			if PhoneApprovalSupported("item/commandExecution/requestApproval", encode(params)) {
				t.Fatal("rich approval allowed")
			}
			h, _ := readyHost(t)
			hostRequest(t, h.Mux, `1`, params)
			if err := h.respond(json.RawMessage(`1`), json.RawMessage(`{"decision":"accept"}`)); err == nil {
				t.Fatal("host bypasses display policy")
			}
		})
	}
	params := simpleParams()
	params["availableDecisions"] = []string{"accept", "decline"}
	if !PhoneApprovalSupported("item/commandExecution/requestApproval", encode(params)) {
		t.Fatal("simple approval rejected")
	}
	delete(params, "command")
	if PhoneApprovalSupported("item/commandExecution/requestApproval", encode(params)) {
		t.Fatal("blind command approval")
	}
	file := map[string]any{"threadId": "a", "turnId": "turn-1", "itemId": "file"}
	if !PhoneApprovalSupported("item/fileChange/requestApproval", encode(file)) {
		t.Fatal("simple file approval rejected")
	}
	file["grantRoot"] = "C:/"
	if PhoneApprovalSupported("item/fileChange/requestApproval", encode(file)) {
		t.Fatal("session root grant allowed")
	}
}

func TestHostReadonlySnapshotAndHistoryNeverResumeOrRequestAllTurns(t *testing.T) {
	h, up := readyHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, method := range []string{"snapshot", "history"} {
		done := make(chan error, 1)
		go func() {
			_, err := h.dispatch(ctx, method, json.RawMessage(`{"threadId":"archived","readOnly":true}`))
			done <- err
		}()
		var call struct {
			ID     json.RawMessage            `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		select {
		case raw := <-up:
			if err := json.Unmarshal(raw, &call); err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if string(call.Params["threadId"]) != `"archived"` {
			t.Fatal(call)
		}
		if method == "snapshot" {
			if call.Method != "thread/read" || string(call.Params["includeTurns"]) != "false" || len(call.Params) != 2 {
				t.Fatal("readonly snapshot may hydrate or resume", call)
			}
		} else if call.Method != "thread/turns/list" || string(call.Params["limit"]) != "30" || string(call.Params["sortDirection"]) != `"desc"` || string(call.Params["itemsView"]) != `"summary"` {
			t.Fatal("history was not a bounded page", call)
		}
		if err := h.Mux.FromServer(encode(map[string]any{"id": call.ID, "result": map[string]any{"thread": map[string]string{"id": "archived"}, "data": []any{}}})); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostAllowsExplicitThreadCatalogueRPC(t *testing.T) {
	h, up := readyHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	params := json.RawMessage(`{"limit":50,"archived":false,"sourceKinds":["cli","vscode","exec","appServer","subAgent","subAgentReview","subAgentCompact","subAgentThreadSpawn","subAgentOther","unknown"],"modelProviders":[]}`)
	done := make(chan error, 1)
	go func() { _, err := h.dispatch(ctx, "thread/list", params); done <- err }()
	var call struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	select {
	case raw := <-up:
		if err := json.Unmarshal(raw, &call); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if call.Method != "thread/list" || string(call.Params) != string(params) {
		t.Fatal("catalogue parameters altered", call)
	}
	if err := h.Mux.FromServer(encode(map[string]any{"id": call.ID, "result": map[string]any{"data": []any{}}})); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
