//go:build windows

package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/bridge"
	"mirrorbird/internal/platform"
)

func TestCreateMaterializesFirstTurnBeforeHydrationOverIPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	frames := make(chan []byte, 64)
	mux := bridge.New(ipcTestWriter{frames, ctx.Done()}, io.Discard)
	defer mux.Close(nil)
	if err := mux.FromDesktop(rawJSON(map[string]any{"id": 0, "method": "initialize", "params": map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	var initial struct{ ID json.RawMessage }
	if err := json.Unmarshal(<-frames, &initial); err != nil {
		t.Fatal(err)
	}
	if err := mux.FromServer(rawJSON(map[string]any{"id": initial.ID, "result": map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	metadata, err := attachment.NewMetadata(t.TempDir(), "synthetic-backend.exe", 1)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := platform.ListenPipe(metadata.Pipe)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host := &attachment.Host{Mux: mux, Metadata: metadata}
	hostDone := make(chan error, 1)
	go func() { hostDone <- host.Serve(ctx, listener) }()

	var callsMu sync.Mutex
	var calls []string
	backendDone := make(chan error, 1)
	go func() {
		backendDone <- func() error {
			thread := testThread("created", false)
			thread.Turns = []Turn{}
			started := false
			for {
				select {
				case <-ctx.Done():
					return nil
				case frame := <-frames:
					var call struct {
						ID     json.RawMessage
						Method string
						Params map[string]json.RawMessage
					}
					if err := json.Unmarshal(frame, &call); err != nil {
						return err
					}
					callsMu.Lock()
					calls = append(calls, call.Method)
					callsMu.Unlock()
					var result any
					switch call.Method {
					case "project/list":
						result = json.RawMessage(`{"data":[{"id":"project","name":"Project","roots":[{"path":"C:/test"}]}]}`)
					case "thread/start":
						if string(call.Params["projectId"]) != `"project"` || string(call.Params["cwd"]) != `"C:/test"` || len(call.Params) != 2 {
							return fmt.Errorf("unexpected creation options: %s", frame)
						}
						result = map[string]any{"thread": thread}
					case "turn/start":
						var input []struct{ Type, Text string }
						if started || string(call.Params["threadId"]) != `"created"` || json.Unmarshal(call.Params["input"], &input) != nil ||
							len(input) != 1 || input[0].Type != "text" || input[0].Text != "first message" || len(call.Params) != 2 {
							return fmt.Errorf("initial message must be sent exactly once with default options: %s", frame)
						}
						// Desktop may switch away while the first message is being sent.
						// The temporary creation watch must keep this unsubscribe local.
						if err := mux.FromDesktop(rawJSON(map[string]any{"id": "desktop-unsubscribe", "method": "thread/unsubscribe", "params": map[string]string{"threadId": "created"}})); err != nil {
							return err
						}
						select {
						case detached := <-frames:
							_ = mux.FromServer(rawJSON(map[string]any{"id": call.ID, "error": map[string]any{"code": -32603, "message": "thread was unsubscribed"}}))
							return fmt.Errorf("Desktop unsubscribe reached backend during initial turn: %s", detached)
						default:
						}
						started = true
						thread = testThread("created", true)
						thread.Turns[0].Items = append([]Item{{ID: "user-1", Type: "userMessage", Content: call.Params["input"]}}, thread.Turns[0].Items...)
						if err := mux.FromServer(rawJSON(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": thread.ID, "turn": thread.Turns[0]}})); err != nil {
							return err
						}
						// A fast first turn may request approval before the browser's first GET.
						if err := mux.FromServer(rawJSON(request(json.RawMessage(`"approval-1"`), thread.ID))); err != nil {
							return err
						}
						result = map[string]any{"turn": thread.Turns[0]}
					case "thread/read":
						metadata := thread
						metadata.Turns = nil
						result = map[string]any{"thread": metadata}
					case "thread/resume", "thread/turns/list":
						// Real app-server can read metadata before a first turn, but has no rollout to resume.
						if !started {
							if err := mux.FromServer(rawJSON(map[string]any{"id": call.ID, "error": map[string]any{"code": -32603, "message": "no rollout found for thread"}})); err != nil {
								return err
							}
							continue
						}
						if call.Method == "thread/resume" {
							result = map[string]any{"thread": thread, "initialTurnsPage": turnsPage{Data: thread.Turns}}
						} else {
							result = turnsPage{Data: thread.Turns}
						}
					default:
						return fmt.Errorf("unexpected backend request: %s", call.Method)
					}
					if err := mux.FromServer(rawJSON(map[string]any{"id": call.ID, "result": result})); err != nil {
						return err
					}
				}
			}
		}()
	}()
	s := New(t.TempDir())
	defer s.Close()
	defer func() {
		s.Close()
		cancel()
		if err := <-hostDone; err != nil {
			t.Error(err)
		}
		if err := <-backendDone; err != nil {
			t.Error(err)
		}
	}()
	s.discover = func(ctx context.Context, _ string) (attachmentClient, error) {
		return attachment.Connect(ctx, metadata)
	}
	if err := s.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := s.CreateThread(ctx, "project", MessageInput{Text: "first message"})
	if err != nil || string(data) != `{"threadId":"created"}` {
		t.Fatalf("creation must start the first turn before attempting hydration: result=%s, err=%v", data, err)
	}
	callsMu.Lock()
	creationCalls := slices.Clone(calls)
	callsMu.Unlock()
	if !slices.Equal(creationCalls, []string{"project/list", "thread/start", "turn/start"}) {
		t.Fatalf("creation resumed or read history before sending the first message: %v", creationCalls)
	}
	waitFor(t, func() bool { return mux.Status().WatchedThreads == 0 })
	s.mu.Lock()
	watchSlots := len(s.conn.watchSlots)
	s.mu.Unlock()
	if watchSlots != 0 {
		t.Fatalf("creation leaked %d temporary watch slots", watchSlots)
	}
	value, err := s.Read(ctx, "created", ReadOptions{})
	if err != nil || value.ResyncRequired || value.ReadOnly || value.ActiveTurnID != "turn-1" || len(value.Thread.Turns) != 1 || len(value.Thread.Turns[0].Items) != 2 {
		t.Fatalf("first browser read did not hydrate the running turn: %+v, err=%v", value, err)
	}
	if len(value.Pending) != 1 || string(value.Pending[0].ID) != `"approval-1"` || value.Pending[0].DesktopOnly {
		t.Fatalf("approval emitted before hydration was lost: %+v", value.Pending)
	}
	if watched := mux.Status().WatchedThreads; watched != 1 {
		t.Fatalf("browser hydration did not acquire its own watch: %d", watched)
	}
	if err := mux.FromServer(rawJSON(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{
		"threadId": "created", "turnId": "turn-1", "itemId": "item-1", "delta": " after hydration",
	}})); err != nil {
		t.Fatal(err)
	}
	awaitSequence(t, s, mux.Status().Sequence)
	value, err = s.Read(ctx, "created", ReadOptions{})
	if err != nil || value.ResyncRequired || value.Thread.Turns[0].Items[1].Text != "base after hydration" {
		t.Fatalf("new thread did not receive live events: %+v, err=%v", value, err)
	}
	callsMu.Lock()
	turnStarts := 0
	for _, method := range calls {
		if method == "turn/start" {
			turnStarts++
		}
	}
	callsMu.Unlock()
	if turnStarts != 1 {
		t.Fatalf("hydration replayed the initial message %d times", turnStarts)
	}
}
