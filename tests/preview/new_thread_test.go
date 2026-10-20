package main

import (
	"encoding/json"
	"testing"

	"mirrorbird/internal/session"
)

func TestPreviewNewThreadReturnsProjectsModelsAndDefaults(t *testing.T) {
	h, b := previewFixture(t)
	w := previewRequest(h, "", "GET", "/api/new-thread", "")
	var result session.NewThreadContext
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Projects) != 2 || len(result.Models) != 2 ||
		result.Defaults != (session.NewThreadDefaults{Model: "demo-model", Effort: "medium"}) {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(b.snapshots) != 0 {
		t.Fatal("new thread metadata loaded an existing transcript")
	}
}
