package server

import (
	"context"
	"net/http"
	"time"

	"mirrorbird/internal/session"
)

func (s *Server) newThread(w http.ResponseWriter, r *http.Request, a admission) {
	if _, err := boundedQuery(r); err != nil {
		failure(w, http.StatusBadRequest, "invalid new thread query", "invalid_input")
		return
	}
	backend, ok := s.Backend.(interface {
		NewThread(context.Context) (session.NewThreadContext, error)
	})
	if !ok {
		backendFailure(w, session.ErrUnsupported, false)
		return
	}
	ctx, cancel := requestContext(r.Context(), a.lifetime, 45*time.Second)
	defer cancel()
	out, err := backend.NewThread(ctx)
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
