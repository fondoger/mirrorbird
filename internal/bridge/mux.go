package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"sync"
)

var (
	ErrClosed                 = errors.New("bridge: closed")
	ErrNotReady               = errors.New("bridge: desktop initialization has not completed")
	ErrInitializationReserved = errors.New("bridge: initialization belongs to the desktop")
	ErrAlreadyInitialized     = errors.New("bridge: desktop initialization was already sent")
	ErrNoPendingRequest       = errors.New("bridge: server request is no longer pending")
	ErrCapacity               = errors.New("bridge: pending request capacity exceeded")
)

const (
	maxClientPending    = 4096
	maxServerPending    = 1024
	maxPendingBytes     = 64 << 20
	maxSubscribers      = 16
	maxSubscriberBuffer = 128
	maxSubscriberBytes  = MaxFrameBytes
)

// Event is an unmodified server message, including responses to local calls.
// Sequence is monotonic for this Mux, not across process restarts. Events and
// PendingRequests are privileged protocol data; an adapter must filter them
// before exposing them to a remote client.
type Event struct {
	Sequence uint64          `json:"sequence"`
	Message  json.RawMessage `json:"message"`
}

type Status struct {
	Ready           bool   `json:"ready"`
	Closed          bool   `json:"closed"`
	Sequence        uint64 `json:"sequence"`
	PendingDesktop  int    `json:"pendingDesktop"`
	PendingLocal    int    `json:"pendingLocal"`
	PendingServer   int    `json:"pendingServer"`
	Subscribers     int    `json:"subscribers"`
	WatchedThreads  int    `json:"watchedThreads"`
	RetainedThreads int    `json:"retainedThreads"`
}

type callResult struct {
	result   json.RawMessage
	sequence uint64
	err      error
}

type clientPending struct {
	originalID   json.RawMessage
	initialize   bool
	done         chan callResult // nil for Desktop
	bytes        int
	threadMethod string
	threadID     string
	threadOrder  uint64
	threadWatch  *threadWatch
}

type serverPending struct {
	id       json.RawMessage
	frame    json.RawMessage
	sequence uint64
	bytes    int
}

type subscription struct {
	ch    chan Event
	sizes []int
	bytes int
}

// Mux owns routing state, but does not own or close its writers. The process
// owner must close the underlying pipes to unblock an in-progress Write during
// shutdown. Upstream and Desktop writes are independently serialized. No state
// lock is held during I/O or caller-provided JSON marshaling.
//
// All client requests, including Desktop requests, receive unique upstream IDs.
// Server requests use a separate, untouched ID namespace. Only one response to
// a pending server request is accepted, regardless of who answers first.
type Mux struct {
	upstream io.Writer
	desktop  io.Writer
	upGate   chan struct{}
	downMu   sync.Mutex

	mu           sync.Mutex
	closed       bool
	closeErr     error
	closedCh     chan struct{}
	initialized  bool
	ready        bool
	nextID       uint64
	sequence     uint64
	pendingBytes int
	clients      map[string]*clientPending
	servers      map[string]*serverPending
	subscribers  map[*subscription]struct{}
	threads      map[string]*threadWatch
	threadOrder  uint64
}

func New(upstream io.Writer, desktop io.Writer) *Mux {
	m := &Mux{
		upstream:    upstream,
		desktop:     desktop,
		upGate:      make(chan struct{}, 1),
		closedCh:    make(chan struct{}),
		clients:     make(map[string]*clientPending),
		servers:     make(map[string]*serverPending),
		subscribers: make(map[*subscription]struct{}),
		threads:     make(map[string]*threadWatch),
	}
	m.upGate <- struct{}{}
	return m
}

// FromDesktop forwards notifications, remaps requests, and arbitrates replies
// to server requests. A stale/duplicate Desktop reply is consumed, not fatal.
func (m *Mux) FromDesktop(frame []byte) error {
	msg, err := parseMessage(frame)
	if err != nil {
		return err
	}
	threadMethod, threadID := threadMetadata(msg.method, msg.params)
	if err := m.acquireUp(context.Background()); err != nil {
		return err
	}
	holdsUp := true
	defer func() {
		if holdsUp {
			m.releaseUp()
		}
	}()

	m.mu.Lock()
	if m.closed {
		err := m.closeErr
		m.mu.Unlock()
		return err
	}
	out := frame
	if !msg.hasMethod {
		pending := m.servers[msg.idKey]
		if pending == nil {
			m.mu.Unlock()
			return nil
		}
		delete(m.servers, msg.idKey)
		m.pendingBytes -= pending.bytes
	} else if msg.id != nil {
		if threadMethod == "thread/unsubscribe" {
			if watch := m.threads[threadID]; watch != nil && watch.watchers > 0 {
				if len(msg.id)+48 > MaxFrameBytes {
					m.mu.Unlock()
					return ErrFrameTooLarge
				}
				m.suppressUnsubscribeLocked(threadID)
				m.mu.Unlock()
				// Never hold both directional write locks. No upstream bytes
				// are sent, so release its gate before acknowledging Desktop.
				holdsUp = false
				m.releaseUp()
				return m.acknowledgeUnsubscribe(msg.id)
			}
		}
		if msg.method == "initialize" && m.initialized {
			m.mu.Unlock()
			return ErrAlreadyInitialized
		}
		id, key, err := m.newIDLocked()
		if err != nil {
			m.mu.Unlock()
			return err
		}
		size := len(msg.id) + len(key) + len(threadID)
		if len(m.clients) >= maxClientPending || size > maxPendingBytes-m.pendingBytes {
			m.mu.Unlock()
			return ErrCapacity
		}
		if len(frame)-len(msg.id)+len(id) > MaxFrameBytes {
			m.mu.Unlock()
			return ErrFrameTooLarge
		}
		pending := &clientPending{originalID: clone(msg.id), initialize: msg.method == "initialize", bytes: size, threadMethod: threadMethod, threadID: threadID}
		m.beginThreadOperationLocked(pending)
		m.clients[key] = pending
		m.pendingBytes += size
		if pending.initialize {
			m.initialized = true
		}
		m.mu.Unlock()
		out = msg.replaceID(id)
		return m.writeUp(out)
	} else if msg.method == "initialize" {
		m.mu.Unlock()
		return ErrInvalidMessage // initialization must be a Desktop request
	} else if threadMethod == "thread/unsubscribe" && m.suppressUnsubscribeLocked(threadID) {
		// This method normally has a request ID, but a notification must not
		// bypass the union or manufacture a response where none was requested.
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	return m.writeUp(out)
}

// FromServer publishes every valid message without blocking on subscribers.
// Notifications and server requests reach Desktop byte-for-byte. A response
// with no live client mapping (including late or duplicate responses) is
// consumed: it cannot safely be attributed to any Desktop request.
func (m *Mux) FromServer(frame []byte) error {
	msg, err := parseMessage(frame)
	if err != nil {
		return err
	}
	m.downMu.Lock()
	defer m.downMu.Unlock()

	m.mu.Lock()
	if m.closed {
		err := m.closeErr
		m.mu.Unlock()
		return err
	}
	m.sequence++
	sequence := m.sequence
	forward, makeReady := true, false
	var restoreID json.RawMessage
	if !msg.hasMethod {
		pending := m.clients[msg.idKey]
		if pending == nil {
			forward = false
		} else {
			delete(m.clients, msg.idKey)
			m.pendingBytes -= pending.bytes
			m.completeThreadOperationLocked(pending, msg)
			m.settleThreadOperationLocked(pending)
			if pending.done != nil {
				forward = false
				result := callResult{result: clone(msg.result), sequence: sequence}
				if msg.rpcError != nil {
					result.err = decodeRPCError(msg.rpcError)
				}
				pending.done <- result
			} else {
				restoreID = pending.originalID
				makeReady = pending.initialize && msg.result != nil
			}
		}
	} else if msg.id != nil {
		if m.servers[msg.idKey] != nil {
			// A retransmission must not replace the live arbitration entry.
			forward = false
		} else {
			size := len(frame) + len(msg.id) + len(msg.idKey)
			if len(m.servers) >= maxServerPending || size > maxPendingBytes-m.pendingBytes {
				m.publishLocked(Event{Sequence: sequence, Message: frame})
				m.mu.Unlock()
				return ErrCapacity
			}
			m.servers[msg.idKey] = &serverPending{id: clone(msg.id), frame: clone(frame), sequence: sequence, bytes: size}
			m.pendingBytes += size
		}
	} else if msg.method == "serverRequest/resolved" {
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(msg.params, &params) == nil {
			if key, err := requestIDKey(params.RequestID); err == nil {
				if pending := m.servers[key]; pending != nil {
					delete(m.servers, key)
					m.pendingBytes -= pending.bytes
				}
			}
		}
	}
	m.publishLocked(Event{Sequence: sequence, Message: frame})
	m.mu.Unlock()
	if !forward {
		return nil
	}
	out := frame
	if restoreID != nil {
		if len(frame)-len(msg.id)+len(restoreID) > MaxFrameBytes {
			m.Close(ErrFrameTooLarge)
			return ErrFrameTooLarge
		}
		out = msg.replaceID(restoreID)
	}
	if err := writeFrame(m.desktop, out); err != nil {
		m.Close(err)
		return err
	}
	if makeReady {
		m.mu.Lock()
		if !m.closed {
			m.ready = true
		}
		m.mu.Unlock()
	}
	return nil
}

// Call injects a request into the existing Desktop-initialized connection.
// Cancellation never replays a request. If writing has begun, its execution
// outcome may be unknown; its eventual response is still consumed locally.
// A blocked io.Writer cannot prevent this method from honoring its context:
// at most one asynchronous local write can be in flight, and queued callers
// acquire the upstream write gate with their own cancellable contexts.
func (m *Mux) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	result, _, err := m.CallWithSequence(ctx, method, params)
	return result, err
}

// CallWithSequence is Call with the exact Event sequence assigned to its server
// response. Subscribe before requesting a snapshot, buffer events while waiting,
// and use this response sequence rather than a later Status().Sequence reading.
// This is a transport ordering boundary; the protocol adapter must still honor
// the server's snapshot semantics. RPC errors also carry their response sequence;
// errors without a server response (including cancellation/Close) return zero.
func (m *Mux) CallWithSequence(ctx context.Context, method string, params any) (json.RawMessage, uint64, error) {
	if method == "initialize" || method == "initialized" {
		return nil, 0, ErrInitializationReserved
	}
	if method == "thread/unsubscribe" {
		return nil, 0, ErrUnsubscribeReserved
	}
	if ctx == nil || method == "" {
		return nil, 0, ErrInvalidMessage
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	m.mu.Lock()
	err := m.callAllowedLocked()
	m.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	body, err := json.Marshal(struct {
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{method, params})
	if err != nil {
		return nil, 0, errors.New("bridge: cannot encode request parameters")
	}
	if len(body) > MaxFrameBytes {
		return nil, 0, ErrFrameTooLarge
	}
	var threadMethod, threadID string
	if method == "thread/resume" || method == "thread/start" {
		var request struct {
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &request) == nil {
			threadMethod, threadID = threadMetadata(method, request.Params)
		}
	}
	if err := m.acquireUp(ctx); err != nil {
		return nil, 0, err
	}
	if err := ctx.Err(); err != nil {
		m.releaseUp()
		return nil, 0, err
	}
	m.mu.Lock()
	if err := m.callAllowedLocked(); err != nil {
		m.mu.Unlock()
		m.releaseUp()
		return nil, 0, err
	}
	id, key, err := m.newIDLocked()
	if err != nil {
		m.mu.Unlock()
		m.releaseUp()
		return nil, 0, err
	}
	size := len(key) + len(threadID)
	if len(m.clients) >= maxClientPending || size > maxPendingBytes-m.pendingBytes {
		m.mu.Unlock()
		m.releaseUp()
		return nil, 0, ErrCapacity
	}
	if len(body)+len(id)+6 > MaxFrameBytes {
		m.mu.Unlock()
		m.releaseUp()
		return nil, 0, ErrFrameTooLarge
	}
	pending := &clientPending{done: make(chan callResult, 1), bytes: size, threadMethod: threadMethod, threadID: threadID}
	m.beginThreadOperationLocked(pending)
	m.clients[key] = pending
	m.pendingBytes += size
	m.mu.Unlock()

	frame := make([]byte, 0, len(body)+len(id)+6)
	frame = append(frame, `{"id":`...)
	frame = append(frame, id...)
	frame = append(frame, ',')
	frame = append(frame, body[1:]...)
	go func() {
		defer m.releaseUp()
		m.mu.Lock()
		live := !m.closed && m.clients[key] == pending
		m.mu.Unlock()
		if live && ctx.Err() == nil {
			_ = m.writeUp(frame) // writeUp closes the Mux and wakes all callers on error
		}
	}()
	select {
	case result := <-pending.done:
		return result.result, result.sequence, result.err
	case <-ctx.Done():
		m.mu.Lock()
		if m.clients[key] == pending {
			delete(m.clients, key)
			m.pendingBytes -= pending.bytes
			m.settleThreadOperationLocked(pending)
			m.mu.Unlock()
			return nil, 0, ctx.Err()
		}
		m.mu.Unlock()
		// A server response or Close won arbitration before cancellation.
		result := <-pending.done
		return result.result, result.sequence, result.err
	}
}

// Respond answers an unresolved server request using the server's original ID.
// It does not authorize methods or results: the adapter must restrict which
// requests a remote user may answer. A second response is rejected atomically.
func (m *Mux) Respond(requestID json.RawMessage, result any) error {
	key, err := requestIDKey(requestID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(result)
	if err != nil {
		return errors.New("bridge: cannot encode response result")
	}
	if err := m.acquireUp(context.Background()); err != nil {
		return err
	}
	defer m.releaseUp()
	m.mu.Lock()
	if m.closed {
		err := m.closeErr
		m.mu.Unlock()
		return err
	}
	pending := m.servers[key]
	if pending == nil {
		m.mu.Unlock()
		return ErrNoPendingRequest
	}
	if len(body)+len(pending.id)+17 > MaxFrameBytes {
		m.mu.Unlock()
		return ErrFrameTooLarge
	}
	delete(m.servers, key)
	m.pendingBytes -= pending.bytes
	m.mu.Unlock()
	frame := make([]byte, 0, len(body)+len(pending.id)+17)
	frame = append(frame, `{"id":`...)
	frame = append(frame, pending.id...)
	frame = append(frame, `,"result":`...)
	frame = append(frame, body...)
	frame = append(frame, '}')
	return m.writeUp(frame)
}

// Subscribe returns owned event copies. Buffer is clamped to [0, 128]. There are
// at most 16 subscribers, each with at most 16 MiB of unread event payloads.
// A subscriber that cannot accept an event is removed and its channel closed;
// after draining it the caller must obtain a new snapshot before resubscribing.
// If already closed or at capacity, Subscribe returns a closed channel.
func (m *Mux) Subscribe(buffer int) (events <-chan Event, cancel func()) {
	if buffer < 0 {
		buffer = 0
	}
	if buffer > maxSubscriberBuffer {
		buffer = maxSubscriberBuffer
	}
	sub := &subscription{ch: make(chan Event, buffer)}
	m.mu.Lock()
	if m.closed || len(m.subscribers) >= maxSubscribers {
		close(sub.ch)
	} else {
		m.subscribers[sub] = struct{}{}
	}
	m.mu.Unlock()
	return sub.ch, func() {
		m.mu.Lock()
		if _, ok := m.subscribers[sub]; ok {
			delete(m.subscribers, sub)
			close(sub.ch)
		}
		m.mu.Unlock()
	}
}

func (m *Mux) publishLocked(event Event) {
	for sub := range m.subscribers {
		// Channel readers run without our lock. Reconcile previously recorded
		// sizes with the number still queued; concurrent reads can only make
		// this estimate conservative, never exceed the memory budget.
		consumed := len(sub.sizes) - len(sub.ch)
		for _, size := range sub.sizes[:consumed] {
			sub.bytes -= size
		}
		sub.sizes = sub.sizes[consumed:]
		if len(event.Message) > maxSubscriberBytes-sub.bytes {
			delete(m.subscribers, sub)
			close(sub.ch)
			continue
		}
		select {
		case sub.ch <- Event{Sequence: event.Sequence, Message: clone(event.Message)}:
			sub.sizes = append(sub.sizes, len(event.Message))
			sub.bytes += len(event.Message)
		default:
			delete(m.subscribers, sub)
			close(sub.ch)
		}
	}
}

func (m *Mux) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready && !m.closed
}

func (m *Mux) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{Ready: m.ready && !m.closed, Closed: m.closed, Sequence: m.sequence, PendingServer: len(m.servers), Subscribers: len(m.subscribers)}
	for _, pending := range m.clients {
		if pending.done == nil {
			status.PendingDesktop++
		} else {
			status.PendingLocal++
		}
	}
	for _, watch := range m.threads {
		if watch.watchers > 0 {
			status.WatchedThreads++
		} else {
			status.RetainedThreads++
		}
	}
	return status
}

// PendingRequests returns copies of complete unresolved server request frames,
// in arrival order, not just their IDs. Taking it does not consume requests.
func (m *Mux) PendingRequests() []json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := make([]*serverPending, 0, len(m.servers))
	for _, request := range m.servers {
		pending = append(pending, request)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].sequence < pending[j].sequence })
	frames := make([]json.RawMessage, 0, len(pending))
	for _, request := range pending {
		frames = append(frames, clone(request.frame))
	}
	return frames
}

// Close is idempotent, wakes local callers and closes all event channels. It
// never blocks on a writer and does not close resources it did not create.
func (m *Mux) Close(err error) {
	if err == nil {
		err = ErrClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed, m.ready, m.closeErr = true, false, err
	close(m.closedCh)
	for _, pending := range m.clients {
		if pending.done != nil {
			pending.done <- callResult{err: err}
		}
	}
	clear(m.clients)
	clear(m.servers)
	clear(m.threads)
	m.pendingBytes = 0
	for sub := range m.subscribers {
		close(sub.ch)
		delete(m.subscribers, sub)
	}
}

func (m *Mux) callAllowedLocked() error {
	if m.closed {
		return m.closeErr
	}
	if !m.ready {
		return ErrNotReady
	}
	return nil
}

func (m *Mux) newIDLocked() (json.RawMessage, string, error) {
	if m.nextID == ^uint64(0) {
		return nil, "", ErrCapacity
	}
	m.nextID++
	id := "mirrorbird:" + strconv.FormatUint(m.nextID, 10)
	return json.RawMessage(strconv.Quote(id)), "s:" + id, nil
}

func (m *Mux) acquireUp(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.closedCh:
		m.mu.Lock()
		err := m.closeErr
		m.mu.Unlock()
		return err
	case <-m.upGate:
		return nil
	}
}

func (m *Mux) releaseUp() {
	m.upGate <- struct{}{}
}

// The caller holds the upstream write gate, never the state mutex.
func (m *Mux) writeUp(frame []byte) error {
	if err := writeFrame(m.upstream, frame); err != nil {
		m.Close(err)
		return err
	}
	return nil
}
