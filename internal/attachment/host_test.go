package attachment

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"mirrorbird/internal/bridge"
)

func TestHostRequiresCapability(t *testing.T) {
	m, err := NewMetadata("test", "codex.exe", 1)
	if err != nil {
		t.Fatal(err)
	}
	h := &Host{Mux: bridge.New(io.Discard, io.Discard), Metadata: m}
	server, client := net.Pipe()
	defer client.Close()
	go h.serveConnection(context.Background(), server)
	if err := json.NewEncoder(client).Encode(packet{ID: "1", Method: "hello", Params: encode(map[string]string{"capability": "wrong"})}); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	var reply packet
	if err := json.NewDecoder(client).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error == nil || reply.Error.Code != 401 {
		t.Fatal("unauthorized local connection accepted")
	}
}

func TestHostDoesNotExposeArbitraryRPC(t *testing.T) {
	m, err := NewMetadata("test", "codex.exe", 1)
	if err != nil {
		t.Fatal(err)
	}
	h := &Host{Mux: bridge.New(io.Discard, io.Discard), Metadata: m}
	for _, method := range []string{"initialize", "command/exec", "config/value/write", "account/read", "thread/start"} {
		if _, err := h.dispatch(context.Background(), method, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("allowed %q", method)
		}
	}
}
