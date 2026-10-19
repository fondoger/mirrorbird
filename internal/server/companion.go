package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"mirrorbird/internal/session"
)

type companionBackend interface {
	Options(context.Context, string, string, string) (session.ComposerOptions, error)
	History(context.Context, string, string, uint64) (session.HistoryPage, error)
	CreateThread(context.Context, string, session.MessageInput) (json.RawMessage, error)
	Enqueue(context.Context, string, string, session.MessageInput) (json.RawMessage, error)
	Act(context.Context, string, session.ThreadAction) (json.RawMessage, error)
}

func (s *Server) companion(w http.ResponseWriter) (companionBackend, bool) {
	backend, ok := s.Backend.(companionBackend)
	if !ok {
		backendFailure(w, session.ErrUnsupported, false)
	}
	return backend, ok
}

func (s *Server) options(w http.ResponseWriter, r *http.Request, a admission) {
	values, err := boundedQuery(r, "kind", "threadId", "cursor")
	if err != nil {
		failure(w, 400, "invalid options query", "invalid_input")
		return
	}
	backend, ok := s.companion(w)
	if !ok {
		return
	}
	ctx, cancel := requestContext(r.Context(), a.lifetime, 20*time.Second)
	defer cancel()
	out, err := backend.Options(ctx, values.Get("kind"), values.Get("threadId"), values.Get("cursor"))
	if !s.valid(a) {
		accessChanged(w)
		return
	}
	if err != nil {
		backendFailure(w, err, false)
		return
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request, a admission) {
	values, err := boundedQuery(r, "cursor", "version")
	version, versionErr := strconv.ParseUint(values.Get("version"), 10, 64)
	if err != nil || versionErr != nil {
		failure(w, 400, "invalid history query", "invalid_input")
		return
	}
	backend, ok := s.companion(w)
	if !ok {
		return
	}
	ctx, cancel := requestContext(r.Context(), a.lifetime, 45*time.Second)
	defer cancel()
	out, err := backend.History(ctx, a.threadID, values.Get("cursor"), version)
	if !s.valid(a) {
		accessChanged(w)
		return
	}
	if err != nil {
		backendFailure(w, err, false)
		return
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) items(w http.ResponseWriter, r *http.Request, a admission) {
	values, err := boundedQuery(r, "cursor", "version")
	version, versionErr := strconv.ParseUint(values.Get("version"), 10, 64)
	if err != nil || versionErr != nil {
		failure(w, http.StatusBadRequest, "invalid items query", "invalid_input")
		return
	}
	backend, ok := s.Backend.(interface {
		Items(context.Context, string, string, string, uint64) (session.ItemsPage, error)
	})
	if !ok {
		backendFailure(w, session.ErrUnsupported, false)
		return
	}
	ctx, cancel := requestContext(r.Context(), a.lifetime, 45*time.Second)
	defer cancel()
	out, err := backend.Items(ctx, a.threadID, r.PathValue("turnId"), values.Get("cursor"), version)
	if !s.valid(a) {
		accessChanged(w)
		return
	}
	if err != nil {
		backendFailure(w, err, false)
		return
	}
	reply(w, http.StatusOK, out)
}

func (s *Server) createThread(w http.ResponseWriter, r *http.Request, a admission) {
	var input struct {
		RequestID string `json:"requestId"`
		ProjectID string `json:"projectId"`
		session.MessageInput
	}
	if !decode(w, r, &input) {
		return
	}
	backend, ok := s.companion(w)
	if !ok {
		return
	}
	s.operation(w, r, a, input.RequestID, input, func(ctx context.Context) (json.RawMessage, error) {
		return backend.CreateThread(ctx, input.ProjectID, input.MessageInput)
	})
}

func (s *Server) enqueueMessage(w http.ResponseWriter, r *http.Request, a admission) {
	var input struct {
		RequestID string `json:"requestId"`
		session.MessageInput
	}
	if !decode(w, r, &input) {
		return
	}
	backend, ok := s.companion(w)
	if !ok {
		return
	}
	s.operation(w, r, a, input.RequestID, input, func(ctx context.Context) (json.RawMessage, error) {
		return backend.Enqueue(ctx, a.threadID, input.RequestID, input.MessageInput)
	})
}

func (s *Server) threadAction(w http.ResponseWriter, r *http.Request, a admission) {
	var input struct {
		RequestID string `json:"requestId"`
		session.ThreadAction
	}
	if !decode(w, r, &input) {
		return
	}
	backend, ok := s.companion(w)
	if !ok {
		return
	}
	s.operation(w, r, a, input.RequestID, input, func(ctx context.Context) (json.RawMessage, error) {
		return backend.Act(ctx, a.threadID, input.ThreadAction)
	})
}
