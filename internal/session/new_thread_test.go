package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"mirrorbird/internal/attachment"
)

func newThreadTestModel(name, effort string, isDefault bool) Model {
	return Model{ID: name + "-id", Model: name, Name: name, Default: isDefault, DefaultEffort: effort,
		Efforts: []ReasoningOption{{Effort: "low"}, {Effort: "high"}, {Effort: "ultra"}}}
}

func newThreadTestHook(defaults NewThreadDefaults) func(context.Context, string, json.RawMessage) (json.RawMessage, error, bool) {
	return func(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error, bool) {
		switch method {
		case "composer/defaults":
			return rawJSON(defaults), nil, true
		case "project/list":
			return rawJSON(map[string]any{"data": []Project{{ID: "project", Name: "Workspace"}}}), nil, true
		case "model/list":
			return rawJSON(map[string]any{"data": []Model{newThreadTestModel("model", "low", true)}}), nil, true
		default:
			return nil, nil, false
		}
	}
}

func TestNewThreadAggregatesAllPagesAndConfiguredDefaults(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint("configured=", configured), func(t *testing.T) {
			f := newFake()
			f.setHook(func(_ context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
				if method == "composer/defaults" {
					defaults := NewThreadDefaults{}
					if configured {
						defaults = NewThreadDefaults{Model: "selected", Effort: "ultra"}
					}
					return rawJSON(defaults), nil, true
				}
				if method != "project/list" && method != "model/list" {
					return nil, nil, false
				}
				var params struct {
					Limit  int
					Cursor string
				}
				if json.Unmarshal(raw, &params) != nil || params.Limit != 100 {
					t.Errorf("unexpected catalogue params: %s", raw)
				}
				if method == "project/list" {
					if params.Cursor == "" {
						return json.RawMessage(`{"data":[{"id":"first"}],"nextCursor":"projects-2"}`), nil, true
					}
					if params.Cursor != "projects-2" {
						t.Errorf("unexpected project cursor: %q", params.Cursor)
					}
					return json.RawMessage(`{"data":[{"id":"second"}],"nextCursor":null}`), nil, true
				}
				if params.Cursor == "" {
					return rawJSON(map[string]any{"data": []Model{newThreadTestModel("first", "low", false)}, "nextCursor": "models-2"}), nil, true
				}
				if params.Cursor != "models-2" {
					t.Errorf("unexpected model cursor: %q", params.Cursor)
				}
				return rawJSON(map[string]any{"data": []Model{newThreadTestModel("catalogue-default", "high", true), newThreadTestModel("selected", "low", false)}}), nil, true
			})
			s := connectedService(t, f)
			got, err := s.NewThread(context.Background())
			want := NewThreadDefaults{Model: "catalogue-default", Effort: "high"}
			if configured {
				want = NewThreadDefaults{Model: "selected", Effort: "ultra"}
			}
			if err != nil || len(got.Projects) != 2 || got.Projects[1].ID != "second" || len(got.Models) != 3 || got.Defaults != want {
				t.Fatal(got, err)
			}
			if f.count("project/list") != 2 || f.count("model/list") != 2 || f.count("composer/defaults") != 1 {
				t.Fatal("catalogues were not completely aggregated")
			}
			if f.count("thread/start") != 0 || f.count("snapshot") != 0 {
				t.Fatal("composer metadata created or hydrated a thread")
			}
		})
	}
}

func TestNewThreadDefaultEffortUsesSelectedModel(t *testing.T) {
	f := newFake()
	f.setHook(newThreadTestHook(NewThreadDefaults{Model: "model"}))
	s := connectedService(t, f)
	got, err := s.NewThread(context.Background())
	if err != nil || got.Defaults != (NewThreadDefaults{Model: "model", Effort: "low"}) {
		t.Fatal(got, err)
	}
}

func TestNewThreadFailureNeverReturnsPartialContext(t *testing.T) {
	pageFailure := errors.New("temporary page failure")
	for _, tc := range []struct {
		name, method string
		reply        json.RawMessage
		failure      error
		want         error
	}{
		{name: "project unavailable", method: "project/list", failure: pageFailure, want: pageFailure},
		{name: "model unavailable after projects", method: "model/list", failure: pageFailure, want: pageFailure},
		{name: "outdated shim", method: "composer/defaults", failure: attachment.ErrOutdatedShim, want: ErrAttachmentOutdated},
		{name: "unsupported configuration", method: "composer/defaults", failure: attachment.ErrUnsupportedOperation, want: ErrUnsupported},
		{name: "null defaults", method: "composer/defaults", reply: json.RawMessage(`null`), want: ErrResyncRequired},
		{name: "unknown configured model", method: "composer/defaults", reply: json.RawMessage(`{"model":"missing","effort":"high"}`), want: ErrResyncRequired},
		{name: "unsupported configured effort", method: "composer/defaults", reply: json.RawMessage(`{"model":"model","effort":"unknown"}`), want: ErrResyncRequired},
		{name: "malformed page", method: "model/list", reply: json.RawMessage(`{"data":null}`), want: ErrResyncRequired},
		{name: "oversized page", method: "model/list", reply: rawJSON(map[string]any{"data": make([]Model, 101)}), want: ErrResyncRequired},
		{name: "oversized cursor", method: "model/list", reply: rawJSON(map[string]any{"data": []Model{}, "nextCursor": strings.Repeat("x", 4097)}), want: ErrResyncRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			base := newThreadTestHook(NewThreadDefaults{})
			f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
				if method == tc.method {
					return tc.reply, tc.failure, true
				}
				return base(ctx, method, raw)
			})
			s := connectedService(t, f)
			got, err := s.NewThread(context.Background())
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(got, NewThreadContext{}) {
				t.Fatal(got, err)
			}
		})
	}
}

func TestNewThreadBoundsCursorCyclesAndPageCount(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		t.Run(fmt.Sprint("cycle=", cycle), func(t *testing.T) {
			f := newFake()
			base := newThreadTestHook(NewThreadDefaults{})
			pages := 0
			f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
				if method == "project/list" {
					pages++
					next := fmt.Sprint(pages)
					if cycle && pages == 3 {
						next = "1"
					}
					return rawJSON(map[string]any{"data": []Project{{ID: next}}, "nextCursor": next}), nil, true
				}
				return base(ctx, method, raw)
			})
			s := connectedService(t, f)
			got, err := s.NewThread(context.Background())
			wantPages := newThreadMaxPages
			if cycle {
				wantPages = 3
			}
			if !errors.Is(err, ErrResyncRequired) || !reflect.DeepEqual(got, NewThreadContext{}) || pages != wantPages {
				t.Fatal(got, err, pages)
			}
		})
	}
}

func TestNewThreadBoundsCombinedResponseBytes(t *testing.T) {
	f := newFake()
	base := newThreadTestHook(NewThreadDefaults{})
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method == "project/list" {
			// Each response fits individually; their total must still be bounded.
			return rawJSON(map[string]any{"data": []Project{{ID: "project", Name: strings.Repeat("x", maxRPCBytes-100)}}}), nil, true
		}
		return base(ctx, method, raw)
	})
	s := connectedService(t, f)
	got, err := s.NewThread(context.Background())
	if !errors.Is(err, ErrResyncRequired) || !reflect.DeepEqual(got, NewThreadContext{}) || f.count("model/list") != 1 {
		t.Fatal(got.Defaults, err, f.count("model/list"))
	}
}

func TestNewThreadRejectsAttachmentReplacementBetweenCatalogues(t *testing.T) {
	f, replacement := newFake(), newFake()
	s := connectedService(t, f)
	base := newThreadTestHook(NewThreadDefaults{})
	f.setHook(func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error, bool) {
		if method == "project/list" {
			s.discover = func(context.Context, string) (attachmentClient, error) { return replacement, nil }
			if err := s.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		return base(ctx, method, raw)
	})
	got, err := s.NewThread(context.Background())
	if !errors.Is(err, ErrNoAttachment) || !reflect.DeepEqual(got, NewThreadContext{}) ||
		f.count("model/list") != 0 || replacement.count("model/list") != 0 {
		t.Fatal(got, err)
	}
}

func TestNewThreadDisconnectCancelsPendingRead(t *testing.T) {
	f := newFake()
	started := make(chan struct{})
	f.setHook(func(ctx context.Context, method string, _ json.RawMessage) (json.RawMessage, error, bool) {
		if method != "composer/defaults" {
			return nil, nil, false
		}
		close(started)
		<-ctx.Done()
		return nil, ctx.Err(), true
	})
	s := connectedService(t, f)
	done := make(chan error, 1)
	go func() { _, err := s.NewThread(context.Background()); done <- err }()
	<-started
	s.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoAttachment) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not cancel the context read")
	}
}
