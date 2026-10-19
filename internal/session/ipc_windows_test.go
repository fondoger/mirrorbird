//go:build windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/bridge"
	"mirrorbird/internal/platform"
)

type ipcTestWriter struct {
	frames chan []byte
	done   <-chan struct{}
}

func (w ipcTestWriter) Write(data []byte) (int, error) {
	select {
	case w.frames <- append([]byte(nil), data...):
		return len(data), nil
	case <-w.done:
		return 0, context.Canceled
	}
}

func TestWatchCleanupSurvivesSlowAndCancelledRPC(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "slow-reader"
		if cancelled {
			name = "cancelled-reader"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
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

			blocked, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			backendDone := make(chan error, 1)
			go func() {
				for {
					select {
					case <-ctx.Done():
						backendDone <- nil
						return
					case frame := <-frames:
						var call struct {
							ID     json.RawMessage
							Method string
						}
						if err := json.Unmarshal(frame, &call); err != nil {
							backendDone <- err
							return
						}
						thread := testThread("a", false)
						var result any
						switch call.Method {
						case "thread/read":
							thread.Turns = nil
							result = map[string]any{"thread": thread}
						case "thread/resume":
							result = map[string]any{"thread": thread}
						case "thread/turns/list":
							close(blocked)
							select {
							case <-release:
							case <-ctx.Done():
								backendDone <- nil
								return
							}
							result = map[string]any{"data": []Turn{}}
						default:
							backendDone <- errors.New("unexpected backend mutation or request: " + call.Method)
							return
						}
						if err := mux.FromServer(rawJSON(map[string]any{"id": call.ID, "result": result})); err != nil {
							backendDone <- err
							return
						}
					}
				}
			}()
			s := New(t.TempDir())
			defer s.Close()
			s.discover = func(ctx context.Context, _ string) (attachmentClient, error) {
				return attachment.Connect(ctx, metadata)
			}
			testCtx, testCancel := context.WithTimeout(ctx, 15*time.Second)
			defer testCancel()
			if err := s.Connect(testCtx); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Read(testCtx, "a", ReadOptions{}); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			c := s.conn
			old := s.projections["a"]
			s.mu.Unlock()
			readCtx, readCancel := context.WithCancel(testCtx)
			defer readCancel()
			readDone := make(chan error, 1)
			go func() {
				_, err := c.client.Call(readCtx, "thread/turns/list", map[string]any{"threadId": "a", "limit": 30})
				readDone <- err
			}()
			select {
			case <-blocked:
			case <-testCtx.Done():
				t.Fatal("backend read did not start")
			}
			if cancelled {
				readCancel()
			}
			reloaded := make(chan error, 1)
			expectedWatches := 0
			if cancelled {
				s.ClearProjections()
			} else {
				expectedWatches = 1
				go func() {
					value, err := s.Read(testCtx, "a", ReadOptions{Resync: true})
					if err == nil && (value.ResyncRequired || len(value.Thread.Turns) != 1) {
						err = errors.New("resync did not return a usable snapshot")
					}
					reloaded <- err
				}()
			}
			select {
			case <-old.ctx.Done():
			case <-testCtx.Done():
				t.Fatal("old projection was not retired")
			}
			deadline := time.After(3 * time.Second)
			for {
				s.mu.Lock()
				replacement := s.projections["a"]
				ready := cancelled || replacement != nil && replacement != old && replacement.watchAttempted
				s.mu.Unlock()
				if ready && mux.Status().WatchedThreads == expectedWatches && len(c.watchSlots) == expectedWatches {
					break
				}
				select {
				case <-c.client.Done():
					t.Fatal("watch cleanup disconnected healthy IPC while a backend read was still pending")
				case <-deadline:
					t.Fatal("watch cleanup waited for the slow backend read")
				case <-time.After(time.Millisecond):
				}
			}
			// Keep the RPC blocked longer than releaseWatches' actual deadline.
			select {
			case <-c.client.Done():
				t.Fatal("late cleanup timeout closed IPC")
			case <-time.After(2200 * time.Millisecond):
			}
			if mux.Status().PendingLocal != 1 || !s.Connected() {
				t.Fatal("cleanup cancelled unrelated backend work")
			}
			if err := mux.FromServer(rawJSON(map[string]any{"method": "thread/name/updated", "params": map[string]string{"threadId": "a", "threadName": "updated"}})); err != nil {
				t.Fatal(err)
			}
			awaitSequence(t, s, mux.Status().Sequence)
			unblock()
			if err := <-readDone; cancelled && !errors.Is(err, context.Canceled) || !cancelled && err != nil {
				t.Fatal("unexpected slow-read outcome", err)
			}
			if !cancelled {
				if err := <-reloaded; err != nil {
					t.Fatal("resync failed after delayed read", err)
				}
			}
			value, err := s.Read(testCtx, "a", ReadOptions{})
			if err != nil || value.ResyncRequired || len(value.Thread.Turns) != 1 {
				t.Fatal("cannot reload after delayed read", err)
			}
			s.Close()
			cancel()
			if err := <-hostDone; err != nil {
				t.Fatal(err)
			}
			if err := <-backendDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}
