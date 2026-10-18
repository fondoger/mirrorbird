package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"mirrorbird/internal/bridge"
	"mirrorbird/internal/platform"
)

type Client struct {
	conn          net.Conn
	writeMu       sync.Mutex
	mu            sync.Mutex
	pending       map[string]chan packet
	events        chan bridge.Event
	done          chan struct{}
	once          sync.Once
	nextID        atomic.Uint64
	startSequence atomic.Uint64
	meta          Metadata
}

func Connect(ctx context.Context, m Metadata) (*Client, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	conn, err := platform.DialPipe(ctx, m.Pipe)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, pending: make(map[string]chan packet), events: make(chan bridge.Event, 512), done: make(chan struct{}), meta: m}
	go c.read()
	data, err := c.Call(ctx, "hello", map[string]string{"capability": m.Capability})
	if err != nil {
		c.Close()
		return nil, err
	}
	var hello struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if json.Unmarshal(data, &hello) != nil || hello.ProtocolVersion < 0 {
		c.Close()
		return nil, errors.New("invalid attachment handshake")
	}
	// Read the running shim's version, never infer it from the file on disk.
	c.meta.ProtocolVersion = hello.ProtocolVersion
	return c, nil
}

func (c *Client) Metadata() Metadata          { return c.meta }
func (c *Client) Events() <-chan bridge.Event { return c.events }
func (c *Client) Done() <-chan struct{}       { return c.done }

// SubscriptionSequence is the high-water mark observed after the host installed
// its subscription. Earlier events can still be queued; later ones must be
// contiguous. This is a transport fence, never a history snapshot boundary.
func (c *Client) SubscriptionSequence() uint64 { return c.startSequence.Load() }

func (c *Client) Subscribe(ctx context.Context) error {
	data, err := c.Call(ctx, "events", nil)
	if err != nil {
		return err
	}
	var reply struct {
		Sequence *uint64 `json:"sequence"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Sequence == nil {
		return errors.New("attachment does not support ordered subscriptions")
	}
	c.startSequence.Store(*reply.Sequence)
	return nil
}

func (c *Client) Close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := fmt.Sprintf("local-%d", c.nextID.Add(1))
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	response := make(chan packet, 1)
	c.mu.Lock()
	if len(c.pending) >= 128 {
		c.mu.Unlock()
		return nil, errors.New("too many pending attachment requests")
	}
	c.pending[id] = response
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, ErrDisconnected
	default:
	}
	c.writeMu.Lock()
	// A caller can be revoked/cancelled while queued behind another writer.
	// Recheck immediately before transmission, not just before taking the lock.
	select {
	case <-ctx.Done():
		c.writeMu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		c.writeMu.Unlock()
		return nil, ErrDisconnected
	default:
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = json.NewEncoder(c.conn).Encode(packet{ID: id, Method: method, Params: data})
	c.writeMu.Unlock()
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("attachment write failed; operation outcome may be unknown: %w", err)
	}
	select {
	case p := <-response:
		if p.Error != nil {
			return nil, p.Error
		}
		return p.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("attachment response timed out; operation outcome may be unknown: %w", ctx.Err())
	case <-c.done:
		return nil, ErrDisconnected
	}
}

const maxEventQueueBytes = 8 << 20

func (c *Client) read() {
	defer c.Close()
	defer close(c.events)
	// Only this goroutine writes events. Reconcile observed channel consumption
	// conservatively; an unread queue is bounded by bytes as well as item count.
	var sizes []int
	queuedBytes := 0
	_ = bridge.ReadFrames(c.conn, bridge.MaxFrameBytes, func(frame []byte) error {
		var p packet
		if err := json.Unmarshal(frame, &p); err != nil {
			return err
		}
		if p.Method == "event" {
			var event bridge.Event
			if err := json.Unmarshal(p.Params, &event); err != nil {
				return err
			}
			consumed := len(sizes) - len(c.events)
			for _, size := range sizes[:consumed] {
				queuedBytes -= size
			}
			sizes = sizes[consumed:]
			if len(event.Message) > maxEventQueueBytes-queuedBytes {
				return errors.New("event queue byte limit exceeded; reconnect required")
			}
			select {
			case c.events <- event:
				sizes = append(sizes, len(event.Message))
				queuedBytes += len(event.Message)
				return nil
			default:
				return errors.New("event consumer fell behind; reconnect required")
			}
		}
		c.mu.Lock()
		response := c.pending[p.ID]
		c.mu.Unlock()
		if response != nil {
			select {
			case response <- p:
			default:
			}
		}
		return nil
	})
}
