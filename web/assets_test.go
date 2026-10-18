package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestProductionAssetsAreComplete(t *testing.T) {
	assets := Assets()
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(index), "/src/") {
		t.Fatal("embedded index is a development entry point, not a production build")
	}
	refs := regexp.MustCompile(`(?:src|href)="(/[^"?#]+)"`).FindAllStringSubmatch(string(index), -1)
	if len(refs) < 3 {
		t.Fatal("production index should reference a script, stylesheet, and favicon")
	}
	for _, ref := range refs {
		data, readErr := fs.ReadFile(assets, strings.TrimPrefix(ref[1], "/"))
		if readErr != nil || len(data) == 0 {
			t.Fatalf("missing or empty asset %s: %v", ref[1], readErr)
		}
	}
}
