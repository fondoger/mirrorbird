package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"mirrorbird/internal/bridge"
	"mirrorbird/internal/diagnostics"
)

type countingConn struct {
	net.Conn
	mu     sync.Mutex
	writes int
}

func (c *countingConn) Write(data []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return len(data), nil
}
func (c *countingConn) SetWriteDeadline(time.Time) error { return nil }

func testClient(conn net.Conn) *Client {
	return &Client{conn: conn, pending: make(map[string]chan packet), events: make(chan bridge.Event, 512), done: make(chan struct{})}
}

func TestClientCanceledWriterQueueNeverTransmits(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := &countingConn{Conn: left}
	c := testClient(conn)
	defer c.Close()
	c.writeMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Call(ctx, "turn/start", map[string]string{"threadId": "a"}); done <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("call never queued")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	c.writeMu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	conn.mu.Lock()
	writes := conn.writes
	conn.mu.Unlock()
	if writes != 0 {
		t.Fatal("canceled queued mutation transmitted")
	}
}

func TestClientSubscriptionRequiresOrderingFence(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "ordered"}[valid], func(t *testing.T) {
			left, right := net.Pipe()
			c := testClient(left)
			defer c.Close()
			defer right.Close()
			go c.read()
			go func() {
				var request packet
				_ = json.NewDecoder(right).Decode(&request)
				result := map[string]any{"subscribed": true}
				if valid {
					result["sequence"] = uint64(42)
				}
				_ = json.NewEncoder(right).Encode(packet{ID: request.ID, Result: encode(result)})
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := c.Subscribe(ctx)
			if valid {
				if err != nil || c.SubscriptionSequence() != 42 {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("legacy unsequenced subscription accepted")
			}
		})
	}
}

func TestClientEventQueueIsByteBounded(t *testing.T) {
	stopLog, err := diagnostics.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stopLog()
	logPath := diagnostics.Path()
	left, right := net.Pipe()
	c := testClient(left)
	defer c.Close()
	defer right.Close()
	go c.read()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer right.Close()
		for i := 1; i <= 10; i++ {
			message := encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"delta": strings.Repeat("x", maxEventQueueBytes/8)}})
			if err := json.NewEncoder(right).Encode(packet{Method: "event", Params: encode(bridge.Event{Sequence: uint64(i), Message: message})}); err != nil {
				return
			}
		}
	}()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("byte-overflow did not disconnect")
	}
	<-finished
	bytes := 0
	events := 0
	for event := range c.Events() {
		bytes += len(event.Message)
		events++
	}
	if bytes > maxEventQueueBytes || events >= 10 {
		t.Fatal("unbounded event queue", bytes, events)
	}
	stopLog()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Reason       string `json:"reason"`
		EventBytes   int    `json:"event_bytes"`
		QueuedBytes  int    `json:"queued_bytes"`
		QueuedEvents int    `json:"queued_events"`
		LimitBytes   int    `json:"limit_bytes"`
	}
	if json.Unmarshal(data, &recorded) != nil || recorded.Reason != "event_queue_byte_limit" ||
		recorded.LimitBytes != maxEventQueueBytes || recorded.QueuedEvents != events ||
		recorded.EventBytes+recorded.QueuedBytes <= recorded.LimitBytes {
		t.Fatal("overflow diagnostics do not describe the bounded backlog", string(data))
	}
	if strings.Contains(string(data), strings.Repeat("x", 64)) {
		t.Fatal("event contents leaked into diagnostics")
	}
}
