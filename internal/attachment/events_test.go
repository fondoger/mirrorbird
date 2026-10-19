package attachment

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"mirrorbird/internal/bridge"
)

func TestCompactResponseEventPreservesStateAndApprovals(t *testing.T) {
	for _, tc := range []struct {
		message string
		fence   bool
	}{
		{`{"id":1,"result":{"method":"nested data is not a notification"}}`, true},
		{`{"id":"1","error":{"code":-1,"message":"private RPC error"}}`, true},
		{`{}`, true},
		{`{"method":"thread/started","params":{"thread":{"id":"a"}}}`, false},
		{`{"id":9007199254740993,"method":"item/commandExecution/requestApproval","params":{"threadId":"a"}}`, false},
		{`{"method":"future/notification","params":{"text":"<preserve>&bytes"}}`, false},
	} {
		event := bridge.Event{Sequence: 42, Message: json.RawMessage(tc.message)}
		if err := compactResponseEvent(&event); err != nil {
			t.Fatal(err)
		}
		expected := tc.message
		if tc.fence {
			expected = `{}`
		}
		if event.Sequence != 42 || string(event.Message) != expected {
			t.Fatal("event semantics changed")
		}
	}
	event := bridge.Event{Sequence: 43, Message: json.RawMessage(`["invalid message"]`)}
	if compactResponseEvent(&event) == nil {
		t.Fatal("malformed event accepted as an ordering fence")
	}
}

func largeRPCResult() json.RawMessage {
	return encode(map[string]string{"text": strings.Repeat("x", (8<<20)+1)})
}

func TestClientAcceptsOneValidLargeNotification(t *testing.T) {
	left, right := net.Pipe()
	c := testClient(left)
	defer c.Close()
	defer right.Close()
	_ = right.SetWriteDeadline(time.Now().Add(10 * time.Second))
	go c.read()
	message := encode(map[string]any{
		"method": "item/commandExecution/outputDelta",
		"params": map[string]string{"threadId": "a", "delta": strings.Repeat("x", 9<<20)},
	})
	wireDone := make(chan error, 1)
	go func() {
		wireDone <- json.NewEncoder(right).Encode(packet{Method: "event", Params: encode(bridge.Event{Sequence: 1, Message: message})})
	}()
	select {
	case event, ok := <-c.Events():
		if !ok || event.Sequence != 1 || !bytes.Equal(event.Message, message) {
			t.Fatal("valid notification larger than the old 8 MiB limit was lost")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("notification did not reach the consumer")
	}
	if err := <-wireDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
		t.Fatal("one valid notification disconnected the shared attachment")
	default:
	}
}

func TestClientLargeResponseEventKeepsConnectionAndRPCResult(t *testing.T) {
	left, right := net.Pipe()
	c := testClient(left)
	defer c.Close()
	defer right.Close()
	_ = right.SetDeadline(time.Now().Add(10 * time.Second))
	go c.read()
	result := largeRPCResult()
	notification := json.RawMessage(`{"method":"thread/name/updated","params":{"threadId":"a","threadName":"new"}}`)
	wireDone := make(chan error, 1)
	go func() {
		encoder := json.NewEncoder(right)
		// Simulate an already-running, older shim that broadcasts the full reply.
		for _, event := range []bridge.Event{
			{Sequence: 1, Message: encode(map[string]any{"id": 42, "result": result})},
			{Sequence: 2, Message: notification},
		} {
			if err := encoder.Encode(packet{Method: "event", Params: encode(event)}); err != nil {
				wireDone <- err
				return
			}
		}
		var request packet
		if err := json.NewDecoder(right).Decode(&request); err != nil {
			wireDone <- err
			return
		}
		wireDone <- encoder.Encode(packet{ID: request.ID, Result: result})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := c.Call(ctx, "snapshot", nil)
	if err != nil {
		t.Fatal("a response-only event disconnected the shared attachment:", err)
	}
	if !bytes.Equal(got, result) {
		t.Fatal("RPC result was altered while compacting its event")
	}
	for sequence := uint64(1); sequence <= 2; sequence++ {
		select {
		case event := <-c.Events():
			if event.Sequence != sequence {
				t.Fatal("response compaction lost the ordering fence", event.Sequence)
			}
			if sequence == 1 && !bytes.Equal(event.Message, []byte(`{}`)) {
				t.Fatal("response body was retained in the event queue")
			}
			if sequence == 2 && !bytes.Equal(event.Message, notification) {
				t.Fatal("state notification changed")
			}
		case <-ctx.Done():
			t.Fatal("event missing", ctx.Err())
		}
	}
	if err := <-wireDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
		t.Fatal("attachment closed after a valid large RPC response")
	default:
	}
}

func TestHostCompactsResponseEventsBeforeIPC(t *testing.T) {
	h, _ := readyHost(t)
	h.Metadata.Capability = "test-capability"
	server, client := net.Pipe()
	defer client.Close()
	go h.serveConnection(context.Background(), server)
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	encoder, reader := json.NewEncoder(client), bufio.NewReader(client)
	for _, request := range []packet{
		{ID: "1", Method: "hello", Params: encode(map[string]string{"capability": h.Metadata.Capability})},
		{ID: "2", Method: "events"},
	} {
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		var response packet
		if err != nil || json.Unmarshal(line, &response) != nil || response.Error != nil {
			t.Fatal("subscription failed", err)
		}
	}
	if err := h.Mux.FromServer(encode(map[string]any{"id": 42, "result": largeRPCResult()})); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > 256 {
		t.Fatalf("an irrelevant RPC body crossed IPC: %d bytes", len(line))
	}
	var envelope packet
	var event bridge.Event
	if json.Unmarshal(line, &envelope) != nil || json.Unmarshal(envelope.Params, &event) != nil ||
		event.Sequence != 2 || !bytes.Equal(event.Message, []byte(`{}`)) {
		t.Fatal("invalid response ordering fence")
	}
}
