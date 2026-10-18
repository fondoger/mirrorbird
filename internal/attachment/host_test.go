package attachment

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	for _, method := range []string{"initialize", "command/exec", "config/value/write", "account/read", "project/remove"} {
		if _, err := h.dispatch(context.Background(), method, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("allowed %q", method)
		}
	}
}

func TestHostForwardsCompanionMethods(t *testing.T) {
	for _, method := range []string{"project/list", "model/list", "skills/list", "thread/start", "thread/fork", "thread/name/set", "thread/revert", "thread/compact/start", "thread/settings/update"} {
		t.Run(method, func(t *testing.T) {
			h, up := readyHost(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := h.dispatch(ctx, method, json.RawMessage(`{}`)); done <- err }()
			var call struct {
				ID     json.RawMessage
				Method string
			}
			select {
			case frame := <-up:
				if json.Unmarshal(frame, &call) != nil || call.Method != method {
					t.Fatal(string(frame))
				}
			case <-ctx.Done():
				t.Fatal("RPC was not forwarded")
			}
			if err := h.Mux.FromServer(encode(map[string]any{"id": call.ID, "result": map[string]any{}})); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLegacyShimUnsupportedErrorIsRecognizedExactly(t *testing.T) {
	if !errors.Is(&wireError{Code: 502, Message: "unsupported attachment operation"}, ErrUnsupportedOperation) {
		t.Fatal("old shim is not recognized")
	}
	if !errors.Is(&wireError{Code: 502, Message: "unsupported attachment operation"}, ErrOutdatedShim) ||
		errors.Is(&wireError{Code: 501, Message: "unsupported app-server operation"}, ErrOutdatedShim) {
		t.Fatal("old shim and unsupported Desktop RPC must remain distinct")
	}
	if errors.Is(&wireError{Code: 502, Message: "operation may have failed"}, ErrUnsupportedOperation) {
		t.Fatal("unknown outcome was misclassified")
	}
}

func TestUnsupportedRPCSurvivesAttachmentWire(t *testing.T) {
	h, up := readyHost(t)
	metadata, err := NewMetadata("test", "codex.exe", 1)
	if err != nil {
		t.Fatal(err)
	}
	h.Metadata = metadata
	server, client := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	go h.serveConnection(context.Background(), server)
	encoder, reader := json.NewEncoder(client), bufio.NewReader(client)
	readReply := func() packet {
		t.Helper()
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var reply packet
		if err := json.Unmarshal(line, &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if err := encoder.Encode(packet{ID: "1", Method: "hello", Params: encode(map[string]string{"capability": metadata.Capability})}); err != nil {
		t.Fatal(err)
	}
	reply := readReply()
	if reply.Error != nil {
		t.Fatal(reply.Error)
	}
	var hello struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if json.Unmarshal(reply.Result, &hello) != nil || hello.ProtocolVersion != ProtocolVersion {
		t.Fatal("hello must identify the running shim protocol", string(reply.Result))
	}
	if err := encoder.Encode(packet{ID: "2", Method: "thread/settings/update", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var call struct{ ID json.RawMessage }
	if err := json.Unmarshal(<-up, &call); err != nil {
		t.Fatal(err)
	}
	if err := h.Mux.FromServer(encode(map[string]any{"id": call.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}})); err != nil {
		t.Fatal(err)
	}
	reply = readReply()
	if !errors.Is(reply.Error, ErrUnsupportedOperation) {
		t.Fatal(reply.Error)
	}
}
