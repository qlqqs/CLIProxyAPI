// Package web owns the embedded static assets for the carpool console.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"mime"
	"path"
	"strings"
)

//go:embed assets/*
var assets embed.FS

// Asset is one immutable embedded browser resource.
type Asset struct {
	Body        []byte
	ContentType string
	ETag        string
}

// Read returns an embedded asset by its path relative to the assets directory.
func Read(name string) (Asset, error) {
	clean := path.Clean(strings.TrimPrefix(name, "/"))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || strings.Contains(clean, "\\") {
		return Asset{}, fmt.Errorf("carpool web: invalid asset path")
	}
	body, errRead := assets.ReadFile("assets/" + clean)
	if errRead != nil {
		return Asset{}, fmt.Errorf("carpool web: read asset %q: %w", clean, errRead)
	}
	contentType := mime.TypeByExtension(path.Ext(clean))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	digest := sha256.Sum256(body)
	return Asset{
		Body:        body,
		ContentType: contentType,
		ETag:        `"` + hex.EncodeToString(digest[:12]) + `"`,
	}, nil
}
