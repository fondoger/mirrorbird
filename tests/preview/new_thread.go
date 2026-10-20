package main

import (
	"context"

	"mirrorbird/internal/session"
)

func (b *previewBackend) NewThread(ctx context.Context) (session.NewThreadContext, error) {
	projects, err := b.Options(ctx, "projects", "", "")
	if err != nil {
		return session.NewThreadContext{}, err
	}
	models, err := b.Options(ctx, "models", "", "")
	if err != nil {
		return session.NewThreadContext{}, err
	}
	return session.NewThreadContext{
		Projects: projects.Projects, Models: models.Models,
		Defaults: session.NewThreadDefaults{Model: "demo-model", Effort: "medium"},
	}, nil
}
