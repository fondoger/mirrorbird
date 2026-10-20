package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"mirrorbird/internal/attachment"
)

const (
	newThreadPageSize = 100
	newThreadMaxPages = 100
)

type NewThreadDefaults struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

type NewThreadContext struct {
	Projects []Project         `json:"projects"`
	Models   []Model           `json:"models"`
	Defaults NewThreadDefaults `json:"defaults"`
}

// NewThread returns one complete composer context from a single attachment
// lifetime. A failed page never turns an unavailable catalogue into an empty one.
func (s *Service) NewThread(ctx context.Context) (NewThreadContext, error) {
	c, err := s.currentConnection()
	if err != nil {
		return NewThreadContext{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()

	remaining := maxRPCBytes
	data, err := s.newThreadCall(ctx, c, "composer/defaults", map[string]any{}, &remaining)
	if err != nil {
		return NewThreadContext{}, err
	}
	var defaults *NewThreadDefaults
	if json.Unmarshal(data, &defaults) != nil || defaults == nil ||
		len(defaults.Model) > 512 || len(defaults.Effort) > 32 {
		return NewThreadContext{}, ErrResyncRequired
	}
	projects, err := newThreadCatalogue[Project](ctx, s, c, "project/list", &remaining)
	if err != nil {
		return NewThreadContext{}, err
	}
	models, err := newThreadCatalogue[Model](ctx, s, c, "model/list", &remaining)
	if err != nil {
		return NewThreadContext{}, err
	}
	resolved, err := resolveNewThreadDefaults(models, *defaults)
	if err != nil {
		return NewThreadContext{}, err
	}
	if err := s.newThreadConnection(ctx, c); err != nil {
		return NewThreadContext{}, err
	}
	return NewThreadContext{Projects: projects, Models: models, Defaults: resolved}, nil
}

func (s *Service) newThreadConnection(ctx context.Context, c *connection) error {
	if current, err := s.currentConnection(); err != nil || current != c {
		return ErrNoAttachment
	}
	return ctx.Err()
}

func (s *Service) newThreadCall(ctx context.Context, c *connection, method string, params any, remaining *int) (json.RawMessage, error) {
	if err := s.newThreadConnection(ctx, c); err != nil {
		return nil, err
	}
	data, err := c.client.Call(ctx, method, params)
	if currentErr := s.newThreadConnection(ctx, c); currentErr != nil {
		return nil, currentErr
	}
	if errors.Is(err, attachment.ErrOutdatedShim) {
		return nil, ErrAttachmentOutdated
	}
	if errors.Is(err, attachment.ErrUnsupportedOperation) {
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, err
	}
	if len(data) > *remaining || !utf8.Valid(data) {
		return nil, ErrResyncRequired
	}
	*remaining -= len(data)
	return data, nil
}

func newThreadCatalogue[T any](ctx context.Context, s *Service, c *connection, method string, remaining *int) ([]T, error) {
	items := []T{}
	cursor := ""
	seen := map[string]bool{}
	for range newThreadMaxPages {
		params := map[string]any{"limit": newThreadPageSize}
		if cursor != "" {
			params["cursor"] = cursor
		}
		data, err := s.newThreadCall(ctx, c, method, params, remaining)
		if err != nil {
			return nil, err
		}
		var page struct {
			Data []T    `json:"data"`
			Next string `json:"nextCursor"`
		}
		if json.Unmarshal(data, &page) != nil || page.Data == nil || len(page.Data) > newThreadPageSize ||
			len(page.Next) > 4096 || !utf8.ValidString(page.Next) || (page.Next != "" && seen[page.Next]) {
			return nil, ErrResyncRequired
		}
		items = append(items, page.Data...)
		if page.Next == "" {
			return items, nil
		}
		seen[page.Next] = true
		cursor = page.Next
	}
	return nil, ErrResyncRequired
}

func resolveNewThreadDefaults(models []Model, defaults NewThreadDefaults) (NewThreadDefaults, error) {
	for _, model := range models {
		if (defaults.Model != "" && model.Model == defaults.Model) || (defaults.Model == "" && model.Default) {
			if model.Model == "" || len(model.Model) > 512 || len(model.DefaultEffort) > 32 {
				return NewThreadDefaults{}, ErrResyncRequired
			}
			defaults.Model = model.Model
			if defaults.Effort == "" {
				defaults.Effort = model.DefaultEffort
			}
			if defaults.Effort != "" {
				supported := false
				for _, effort := range model.Efforts {
					supported = supported || effort.Effort == defaults.Effort
				}
				if !supported {
					return NewThreadDefaults{}, ErrResyncRequired
				}
			}
			return defaults, nil
		}
	}
	if len(models) == 0 && defaults.Model == "" && defaults.Effort == "" {
		return defaults, nil
	}
	return NewThreadDefaults{}, ErrResyncRequired
}
