package web

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadEmbeddedAssets(t *testing.T) {
	for _, name := range []string{"index.html", "app.css", "app.js"} {
		asset, errRead := Read(name)
		if errRead != nil {
			t.Fatalf("Read(%q) error = %v", name, errRead)
		}
		if len(asset.Body) == 0 || asset.ETag == "" || asset.ContentType == "" {
			t.Fatalf("Read(%q) = %#v", name, asset)
		}
	}
}

func TestIndexUsesExternalResourcesOnly(t *testing.T) {
	asset, errRead := Read("index.html")
	if errRead != nil {
		t.Fatalf("Read(index.html) error = %v", errRead)
	}
	for _, forbidden := range [][]byte{[]byte("<script>"), []byte("<style>"), []byte("http://"), []byte("https://")} {
		if bytes.Contains(asset.Body, forbidden) {
			t.Fatalf("index.html contains forbidden value %q", forbidden)
		}
	}
	if !strings.Contains(string(asset.Body), "/assets/app.js") {
		t.Fatal("index.html does not load embedded application module")
	}
}

func TestReadRejectsTraversalAndMissingFiles(t *testing.T) {
	for _, name := range []string{"../index.html", "missing.js", `..\\index.html`} {
		if _, errRead := Read(name); errRead == nil {
			t.Fatalf("Read(%q) unexpectedly succeeded", name)
		}
	}
}
