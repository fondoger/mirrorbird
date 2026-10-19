package attachment

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"mirrorbird/internal/bridge"
)

// Host is local IPC only. The public HTTP API must not proxy arbitrary operations.
type Host struct {
	Mux      *bridge.Mux
	Metadata Metadata
}

const (
	maxHostRequests     = 128
	maxHostRequestBytes = bridge.MaxFrameBytes
)

func (h *Host) Serve(ctx context.Context, listener net.Listener) error {
	go func() { <-ctx.Done(); _ = listener.Close() }()
	limit := make(chan struct{}, 8)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case limit <- struct{}{}:
			go func() {
				defer func() { <-limit }()
				h.serveConnection(ctx, conn)
			}()
		default:
			_ = conn.Close()
		}
	}
}

func (h *Host) serveConnection(parent context.Context, conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() { <-ctx.Done(); _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var writeMu sync.Mutex
	write := func(p packet) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return json.NewEncoder(conn).Encode(p)
	}
	dispatch := func(p packet) error {
		result, err := h.dispatch(ctx, p.Method, p.Params)
		response := packet{ID: p.ID, Result: result}
		if err != nil {
			response.Result = nil
			response.Error = &wireError{Code: 502, Message: err.Error()}
			var rpc *bridge.RPCError
			if errors.As(err, &rpc) && rpc.Code == -32601 {
				response.Error = &wireError{Code: 501, Message: "unsupported app-server operation"}
			}
		}
		return write(response)
	}
	type queuedCall struct {
		packet packet
		bytes  int
	}
	calls := make(chan queuedCall, maxHostRequests)
	var callsMu sync.Mutex
	callCount, callBytes := 0, 0
	// Preserve backend RPC order without holding up local watch cleanup,
	// health checks or sequence fences while waiting for a backend response.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case call := <-calls:
				if ctx.Err() != nil {
					return
				}
				err := dispatch(call.packet)
				callsMu.Lock()
				callCount--
				callBytes -= call.bytes
				callsMu.Unlock()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	authenticated := false
	type watchKey struct{ threadID, watchID string }
	watches := make(map[watchKey]func())
	defer func() {
		for _, release := range watches {
			release()
		}
	}()
	var subscriptionCancel func()
	defer func() {
		if subscriptionCancel != nil {
			subscriptionCancel()
		}
	}()
	_ = bridge.ReadFrames(conn, bridge.MaxFrameBytes, func(frame []byte) error {
		var p packet
		if err := json.Unmarshal(frame, &p); err != nil || p.ID == "" {
			return errors.New("invalid local request")
		}
		if !authenticated {
			var params struct {
				Capability string `json:"capability"`
			}
			if p.Method != "hello" || json.Unmarshal(p.Params, &params) != nil ||
				subtle.ConstantTimeCompare([]byte(params.Capability), []byte(h.Metadata.Capability)) != 1 {
				_ = write(packet{ID: p.ID, Error: &wireError{Code: 401, Message: "unauthorized"}})
				return errors.New("unauthorized local connection")
			}
			authenticated = true
			_ = conn.SetReadDeadline(time.Time{})
			return write(packet{ID: p.ID, Result: encode(map[string]any{"ready": h.Mux.Ready(), "enginePid": h.Metadata.EnginePID, "shimPid": h.Metadata.ShimPID, "protocolVersion": ProtocolVersion})})
		}
		if p.Method == "watch" || p.Method == "unwatch" {
			var params struct {
				ThreadID string `json:"threadId"`
				WatchID  string `json:"watchId"`
			}
			if json.Unmarshal(p.Params, &params) != nil || params.ThreadID == "" || len(params.ThreadID) > 512 || len(params.WatchID) > 64 {
				return write(packet{ID: p.ID, Error: &wireError{Code: 400, Message: "valid threadId is required"}})
			}
			// Each projection lifetime owns its own watch. A delayed unwatch from
			// an evicted cache cannot release its replacement's watch.
			key := watchKey{params.ThreadID, params.WatchID}
			if p.Method == "watch" {
				if _, ok := watches[key]; !ok {
					if len(watches) >= 128 {
						return write(packet{ID: p.ID, Error: &wireError{Code: 409, Message: "watch capacity exceeded"}})
					}
					release := h.Mux.WatchThread(params.ThreadID)
					if release == nil {
						return write(packet{ID: p.ID, Error: &wireError{Code: 409, Message: "cannot watch thread now"}})
					}
					watches[key] = release
				}
			} else if release, ok := watches[key]; ok {
				release()
				delete(watches, key)
			}
			return write(packet{ID: p.ID, Result: encode(map[string]bool{"ok": true})})
		}
		if p.Method == "respond" {
			var params struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(p.Params, &params) != nil {
				return write(packet{ID: p.ID, Error: &wireError{Code: 400, Message: "invalid response"}})
			}
			if err := h.respond(params.ID, params.Result); err != nil {
				code := 409
				if errors.Is(err, ErrRequestResolved) || errors.Is(err, bridge.ErrNoPendingRequest) {
					code = 410
				}
				return write(packet{ID: p.ID, Error: &wireError{Code: code, Message: err.Error()}})
			}
			return write(packet{ID: p.ID, Result: encode(map[string]bool{"ok": true})})
		}
		if p.Method == "events" {
			if subscriptionCancel != nil {
				return write(packet{ID: p.ID, Error: &wireError{Code: 409, Message: "already subscribed"}})
			}
			events, unsubscribe := h.Mux.Subscribe(256)
			subscriptionCancel = unsubscribe
			if err := write(packet{ID: p.ID, Result: encode(map[string]any{"subscribed": true, "sequence": h.Mux.Status().Sequence})}); err != nil {
				return err
			}
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case event, ok := <-events:
						if !ok || compactResponseEvent(&event) != nil || write(packet{Method: "event", Params: encode(event)}) != nil {
							cancel()
							return
						}
					}
				}
			}()
			return nil
		}
		switch p.Method {
		case "status", "barrier", "pending", "pending/snapshot":
			return dispatch(p)
		}
		callsMu.Lock()
		if callCount >= maxHostRequests || len(frame) > maxHostRequestBytes-callBytes {
			callsMu.Unlock()
			return write(packet{ID: p.ID, Error: &wireError{Code: 429, Message: "attachment request capacity exceeded"}})
		}
		callCount++
		callBytes += len(frame)
		callsMu.Unlock()
		calls <- queuedCall{packet: p, bytes: len(frame)}
		return nil
	})
}

func (h *Host) dispatch(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if method == "status" {
		return encode(map[string]any{"ready": h.Mux.Ready(), "enginePid": h.Metadata.EnginePID, "shimPid": h.Metadata.ShimPID}), nil
	}
	if method == "barrier" {
		return encode(map[string]uint64{"sequence": h.Mux.Status().Sequence}), nil
	}
	if method == "pending" {
		return encode(h.Mux.PendingRequests()), nil
	}
	if method == "pending/snapshot" {
		requests, sequence := h.Mux.PendingRequestsWithSequence()
		return encode(map[string]any{"requests": requests, "sequence": sequence}), nil
	}
	if method == "snapshot" {
		var input struct {
			ThreadID string `json:"threadId"`
			ReadOnly bool   `json:"readOnly"`
			Summary  bool   `json:"summary"`
			Limit    int    `json:"limit"`
		}
		if json.Unmarshal(params, &input) != nil || input.ThreadID == "" || input.Limit < 0 || input.Limit > 30 {
			return nil, errors.New("invalid snapshot parameters")
		}
		if input.Limit == 0 {
			input.Limit = 30
		}
		view := "full"
		if input.Summary {
			view = "summary"
		}
		callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		method := "thread/resume"
		args := map[string]any{
			"threadId": input.ThreadID, "excludeTurns": true,
			"initialTurnsPage": map[string]any{"limit": input.Limit, "sortDirection": "desc", "itemsView": view},
		}
		if input.ReadOnly {
			// Archived/noninteractive history must never resume/unarchive. A
			// separate bounded turns page follows this metadata boundary.
			method = "thread/read"
			args = map[string]any{"threadId": input.ThreadID, "includeTurns": false}
		}
		result, sequence, err := h.Mux.CallWithSequence(callCtx, method, args)
		if err != nil {
			return nil, err
		}
		return encode(map[string]any{"result": result, "sequence": sequence}), nil
	}
	if method == "history" {
		var input struct {
			ThreadID string `json:"threadId"`
		}
		if json.Unmarshal(params, &input) != nil || input.ThreadID == "" {
			return nil, errors.New("threadId required")
		}
		callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		result, sequence, err := h.Mux.CallWithSequence(callCtx, "thread/turns/list", map[string]any{
			"threadId": input.ThreadID, "limit": 30, "sortDirection": "desc", "itemsView": "summary",
		})
		if err != nil {
			return nil, err
		}
		// Deliberately a separate boundary. The session adapter may only merge
		// completed history, never label a later active-turn read as resume state.
		return encode(map[string]any{"result": result, "sequence": sequence}), nil
	}
	// Keep even the authenticated local protocol small; never forward arbitrary
	// account/configuration or command-execution methods from a UI adapter.
	switch method {
	case "thread/list", "thread/loaded/list", "thread/read", "thread/resume", "thread/turns/list", "thread/items/list",
		"turn/start", "turn/steer", "turn/interrupt", "project/list", "model/list", "skills/list",
		"thread/start", "thread/fork", "thread/name/set", "thread/revert", "thread/compact/start", "thread/settings/update":
		callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		return h.Mux.Call(callCtx, method, params)
	default:
		return nil, errors.New("unsupported attachment operation")
	}
}

func (h *Host) respond(id, result json.RawMessage) error {
	key, err := bridge.RequestIDKey(id)
	if err != nil {
		return errors.New("invalid request ID")
	}
	for _, frame := range h.Mux.PendingRequests() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(frame, &request) != nil {
			continue
		}
		pendingKey, err := bridge.RequestIDKey(request.ID)
		if err != nil || pendingKey != key {
			continue
		}
		if !PhoneApprovalSupported(request.Method, request.Params) {
			return errors.New("this request must be answered in Desktop")
		}
		var response struct {
			Decision string `json:"decision"`
		}
		if json.Unmarshal(result, &response) != nil || (response.Decision != "accept" && response.Decision != "decline") {
			return errors.New("invalid approval decision")
		}
		// A Desktop response between the read above and Respond wins atomically
		// in the Mux. Never retry or consume a different request on failure.
		return h.Mux.Respond(request.ID, map[string]string{"decision": response.Decision})
	}
	return ErrRequestResolved
}
