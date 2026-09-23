package okf

import (
	"fmt"
	"slices"
	"strings"
)

// ReservedPaths are the files a bundle export writes at its root, so no concept may hold one.
var ReservedPaths = map[string]bool{"index": true, "log": true}

// Postgres limits in bytes, refused in advance so the agent gets a sentence instead of a driver error.
const (
	MaxPath  = 1024
	MaxBody  = 256 * 1024
	MaxTitle = 4096
)

func hasControl(s string) bool {
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			return true
		}
	}
	return false
}

// Validate returns every rule c breaks. An empty result means valid.
func Validate(c Concept) []string {
	var errors []string
	if strings.TrimFunc(c.Type, isPySpace) == "" {
		errors = append(errors, "type is required")
	}
	if strings.HasPrefix(c.Path, "/") {
		errors = append(errors, "path must be relative, not absolute")
	}
	if slices.Contains(strings.Split(c.Path, "/"), "..") {
		errors = append(errors, "path must not traverse upward")
	}
	pathStripped := strings.TrimFunc(c.Path, isPySpace)
	if pathStripped == "" {
		errors = append(errors, "path is required")
	}
	if ReservedPaths[c.Path] {
		errors = append(errors, fmt.Sprintf("path '%s' is reserved for a generated bundle file", c.Path))
	} else if pathStripped != "" && slices.Contains(strings.Split(strings.TrimPrefix(c.Path, "/"), "/"), "") {
		// A trailing or doubled slash names a concept the export cannot write as a file.
		errors = append(errors, "path must not have an empty segment")
	}
	if strings.Contains(c.Path, "\x00") {
		errors = append(errors, "path must not contain a NUL byte")
	} else if hasControl(c.Path) {
		errors = append(errors, "path must not contain a control character")
	}
	if size := len(c.Path); size > MaxPath {
		errors = append(errors, fmt.Sprintf("path is too long: %d bytes, at most %d", size, MaxPath))
	}

	for _, f := range []struct {
		name  string
		value string
		limit int
	}{
		{"title", c.Title, MaxTitle},
		{"description", c.Description, MaxTitle},
		{"body", c.Body, MaxBody},
		{"type", c.Type, MaxTitle},
	} {
		// Postgres text holds no NUL, which would otherwise fail the write after validation.
		if strings.Contains(f.value, "\x00") {
			errors = append(errors, f.name+" must not contain a NUL byte")
		}
		if size := len(f.value); size > f.limit {
			errors = append(errors, fmt.Sprintf("%s is too long: %d bytes, at most %d", f.name, size, f.limit))
		}
	}
	return errors
}
