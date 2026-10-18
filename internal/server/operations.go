package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mirrorbird/internal/session"
)

const (
	maxOperations       = 512
	maxActiveOperations = 16
	maxOperationWaiters = 8
	operationsPerMinute = 30
	operationRetention  = 15 * time.Minute
)

type operation struct {
	done                 chan struct{}
	digest               [sha256.Size]byte
	projectionGeneration uint64
	waiters              int
	status               int
	body                 []byte
	expires              time.Time
}

type operationRate struct {
	start time.Time
	count int
}

func (s *Server) operation(w http.ResponseWriter, r *http.Request, a admission, id string, input any, run func(context.Context) (json.RawMessage, error)) {
	if len(id) < 16 || len(id) > 100 || strings.ContainsAny(id, "\r\n\x00") {
		failure(w, http.StatusBadRequest, "requestId must be a unique operation identifier", "invalid_request_id")
		return
	}
	payload, err := json.Marshal(input)
	if err != nil {
		failure(w, http.StatusBadRequest, "invalid operation input", "invalid_input")
		return
	}
	digest := sha256.Sum256(payload)
	// A projection generation is deliberately NOT part of the key: reusing an
	// old requestId after rebuilding that thread must conflict, not run again.
	key := strconv.FormatUint(a.generation, 10) + "\x00" + r.URL.Path + "\x00" + id
	now := time.Now()
	s.mu.Lock()
	if err := s.admissionErrorLocked(a); err != nil {
		s.mu.Unlock()
		if errors.Is(err, errAccessChanged) {
			accessChanged(w)
		} else {
			backendFailure(w, err, false)
		}
		return
	}
	for key, op := range s.requests {
		if !now.Before(op.expires) {
			select {
			case <-op.done:
				delete(s.requests, key)
			default:
			}
		}
	}
	if existing := s.requests[key]; existing != nil {
		if existing.digest != digest || existing.projectionGeneration != a.projectionGeneration {
			s.mu.Unlock()
			failure(w, http.StatusConflict, "requestId was already used for a different action or thread lifetime", "request_id_reused")
			return
		}
		if existing.waiters >= maxOperationWaiters {
			s.mu.Unlock()
			failure(w, http.StatusTooManyRequests, "too many requests waiting for this action", "rate_limited")
			return
		}
		existing.waiters++
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			existing.waiters--
			s.mu.Unlock()
		}()
		select {
		case <-existing.done:
			if !s.valid(a) {
				accessChangedAfterAdmission(w)
				return
			}
			writeOperation(w, existing)
		case <-a.stop:
			accessChangedAfterAdmission(w)
		case <-r.Context().Done():
		}
		return
	}
	if len(s.requests) >= maxOperations || s.active >= maxActiveOperations {
		s.mu.Unlock()
		failure(w, http.StatusTooManyRequests, "too many pending or recent actions", "rate_limited")
		return
	}
	if s.rate.start.IsZero() || now.Sub(s.rate.start) >= time.Minute {
		s.rate = operationRate{start: now}
	}
	if s.rate.count >= operationsPerMinute {
		s.mu.Unlock()
		failure(w, http.StatusTooManyRequests, "too many actions; wait before trying again", "rate_limited")
		return
	}
	s.rate.count++
	op := &operation{done: make(chan struct{}), digest: digest, projectionGeneration: a.projectionGeneration}
	s.requests[key] = op
	s.active++
	s.mu.Unlock()

	// A browser disconnect never cancels or retries an accepted command. Stop
	// cancels queued work, but cannot undo an RPC already transmitted. The backend
	// pins this exact projection before waiting for its command gate.
	ctx, cancel := context.WithTimeout(a.lifetime, 45*time.Second)
	if ctx.Err() != nil {
		err = ctx.Err()
	} else {
		_, err = run(session.WithProjectionGeneration(ctx, a.projectionGeneration))
	}
	cancel()
	status := http.StatusAccepted
	body := any(map[string]bool{"accepted": true})
	if err != nil {
		var message, code string
		status, message, code = backendError(err, true)
		body = map[string]string{"error": message, "code": code}
	}
	data, _ := json.Marshal(body)
	s.mu.Lock()
	op.status, op.body = status, data
	op.expires = time.Now().Add(operationRetention)
	s.active--
	close(op.done)
	s.mu.Unlock()
	// A successful RPC is an acknowledgement, not transcript data. Never send a
	// raw backend result. Even success is uncertain to this browser if access or
	// the bound projection changed after admission; do not present it as rejected.
	if !s.valid(a) {
		accessChangedAfterAdmission(w)
		return
	}
	writeOperation(w, op)
}

func accessChangedAfterAdmission(w http.ResponseWriter) {
	failure(w, http.StatusForbidden, "service or thread state changed after the action was admitted; it may have executed, so check Desktop before sending again", "operation_failed_or_outcome_unknown")
}

func writeOperation(w http.ResponseWriter, op *operation) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(op.status)
	_, _ = w.Write(op.body)
}
