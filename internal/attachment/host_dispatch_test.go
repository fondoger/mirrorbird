package attachment

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

type hostWire struct {
	host    *Host
	up      frameWriter
	conn    net.Conn
	encoder *json.Encoder
	decoder *json.Decoder
	done    <-chan struct{}
}

func newHostWire(t *testing.T) *hostWire {
	t.Helper()
	h, up := readyHost(t)
	metadata, err := NewMetadata("dispatch-test", "synthetic.exe", 1)
	if err != nil {
		t.Fatal(err)
	}
	h.Metadata = metadata
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.serveConnection(ctx, server)
	}()
	t.Cleanup(func() {
		client.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("host connection did not stop")
		}
	})
	wire := &hostWire{h, up, client, json.NewEncoder(client), json.NewDecoder(client), done}
	wire.send(t, "hello", "hello", map[string]string{"capability": metadata.Capability})
	if reply := wire.read(t, "hello"); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	return wire
}

func (w *hostWire) send(t *testing.T, id, method string, params any) {
	t.Helper()
	_ = w.conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := w.encoder.Encode(packet{ID: id, Method: method, Params: encode(params)}); err != nil {
		t.Fatal(err)
	}
}

func (w *hostWire) read(t *testing.T, id string) packet {
	t.Helper()
	_ = w.conn.SetDeadline(time.Now().Add(5 * time.Second))
	var reply packet
	if err := w.decoder.Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.ID != id {
		t.Fatalf("reply ID = %q, want %q", reply.ID, id)
	}
	return reply
}

func (w *hostWire) upstream(t *testing.T, method string) json.RawMessage {
	t.Helper()
	select {
	case raw := <-w.up:
		var call struct {
			ID     json.RawMessage
			Method string
		}
		if err := json.Unmarshal(raw, &call); err != nil {
			t.Fatal(err)
		}
		if call.Method != method {
			t.Fatalf("upstream method = %q, want %q", call.Method, method)
		}
		return call.ID
	case <-time.After(3 * time.Second):
		t.Fatal("upstream call did not arrive")
		return nil
	}
}

func TestHostControlsBypassSlowRPCWithoutReorderingBackendCalls(t *testing.T) {
	w := newHostWire(t)
	w.send(t, "watch", "watch", map[string]string{"threadId": "a", "watchId": "old"})
	if reply := w.read(t, "watch"); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	w.send(t, "slow", "snapshot", map[string]string{"threadId": "a"})
	first := w.upstream(t, "thread/resume")
	w.send(t, "next", "thread/list", map[string]int{"limit": 1})
	w.send(t, "last", "model/list", map[string]int{"limit": 1})
	for _, method := range []string{"unwatch", "status", "barrier", "pending", "pending/snapshot"} {
		w.send(t, method, method, map[string]string{"threadId": "a", "watchId": "old"})
		if reply := w.read(t, method); reply.Error != nil {
			t.Fatal(method, reply.Error)
		}
	}
	if status := w.host.Mux.Status(); status.WatchedThreads != 0 || status.PendingLocal != 1 {
		t.Fatal("control operations did not release the watch independently", status)
	}
	select {
	case raw := <-w.up:
		t.Fatalf("queued RPC overtook slow request: %s", raw)
	default:
	}
	hostRequest(t, w.host.Mux, `42`, simpleParams())
	w.send(t, "approval", "respond", map[string]any{"id": 42, "result": map[string]string{"decision": "accept"}})
	w.upstream(t, "")
	if reply := w.read(t, "approval"); reply.Error != nil {
		t.Fatal("approval was blocked by slow read", reply.Error)
	}
	if err := w.host.Mux.FromServer(encode(map[string]any{"id": first, "result": map[string]any{"thread": map[string]string{"id": "a"}}})); err != nil {
		t.Fatal(err)
	}
	if reply := w.read(t, "slow"); reply.Error != nil {
		t.Fatal(reply.Error)
	} else {
		var result struct{ Sequence uint64 }
		if json.Unmarshal(reply.Result, &result) != nil || result.Sequence != w.host.Mux.Status().Sequence {
			t.Fatal("snapshot response lost its actual sequence boundary")
		}
	}
	for _, call := range []struct{ id, method string }{{"next", "thread/list"}, {"last", "model/list"}} {
		id := w.upstream(t, call.method)
		if err := w.host.Mux.FromServer(encode(map[string]any{"id": id, "result": map[string]any{"data": []any{}}})); err != nil {
			t.Fatal(err)
		}
		if reply := w.read(t, call.id); reply.Error != nil {
			t.Fatal(reply.Error)
		}
	}
	w.send(t, "reused", "thread/list", map[string]int{"limit": 1})
	id := w.upstream(t, "thread/list")
	if err := w.host.Mux.FromServer(encode(map[string]any{"id": id, "result": map[string]any{"data": []any{}}})); err != nil {
		t.Fatal(err)
	}
	if reply := w.read(t, "reused"); reply.Error != nil {
		t.Fatal("completed calls did not return queue capacity", reply.Error)
	}
}

func TestHostRPCQueueBoundsAndDisconnectCleanup(t *testing.T) {
	for _, budget := range []string{"count", "bytes"} {
		t.Run(budget, func(t *testing.T) {
			w := newHostWire(t)
			w.send(t, "watch", "watch", map[string]string{"threadId": "a"})
			if reply := w.read(t, "watch"); reply.Error != nil {
				t.Fatal(reply.Error)
			}
			w.send(t, "slow", "snapshot", map[string]string{"threadId": "a"})
			w.upstream(t, "thread/resume")
			count, params := maxHostRequests-1, map[string]string{"threadId": "a"}
			if budget == "bytes" {
				count, params = 1, map[string]string{"synthetic": strings.Repeat("x", maxHostRequestBytes/2)}
			}
			for i := range count {
				w.send(t, fmt.Sprint("queued-", i), "thread/read", params)
			}
			w.send(t, "overflow", "thread/read", params)
			if reply := w.read(t, "overflow"); reply.Error == nil || reply.Error.Code != 429 {
				t.Fatal("unbounded RPC queue", reply.Error)
			}
			for _, method := range []string{"status", "unwatch"} {
				w.send(t, method, method, map[string]string{"threadId": "a"})
				if reply := w.read(t, method); reply.Error != nil {
					t.Fatal("queue saturation blocked local controls", reply.Error)
				}
			}
			w.send(t, "retained", "watch", map[string]string{"threadId": "b"})
			if reply := w.read(t, "retained"); reply.Error != nil {
				t.Fatal(reply.Error)
			}
			w.conn.Close()
			select {
			case <-w.done:
			case <-time.After(3 * time.Second):
				t.Fatal("disconnect blocked on queued RPCs")
			}
			deadline := time.Now().Add(3 * time.Second)
			for w.host.Mux.Status().PendingLocal != 0 {
				if time.Now().After(deadline) {
					t.Fatal("disconnected RPC not cancelled")
				}
				time.Sleep(time.Millisecond)
			}
			if w.host.Mux.Status().WatchedThreads != 0 {
				t.Fatal("disconnected peer retained watches")
			}
			select {
			case raw := <-w.up:
				t.Fatalf("disconnected queue forwarded abandoned work: %s", raw)
			default:
			}
		})
	}
}
