// The seven tool bodies, ported from Tools in 2de90d2:src/keepsake/server/tools.py.
// No tool takes a tenant: the server binds one at startup, so an agent cannot name
// the wrong one. Nothing a tool returns to the agent carries a body it was not
// already entitled to read.
package server

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

// relateAttempts is how many times Relate re-reads and re-appends past a concurrent
// writer. Each round has exactly one winner.
const relateAttempts = 20

// ToolError is surfaced to the agent. It MUST NOT contain a concept body.
type ToolError struct{ Msg string }

func (e *ToolError) Error() string { return e.Msg }

func toolErr(msg string) error { return &ToolError{Msg: msg} }

// convert maps rows to their wire shape, never nil, so an empty answer is [] and not null.
func convert[S, T any](rows []S, f func(S) T) []T {
	out := make([]T, len(rows))
	for i, r := range rows {
		out[i] = f(r)
	}
	return out
}

type Tools struct {
	c     *store.ConceptStore
	t     uuid.UUID
	actor string
}

func NewTools(cs *store.ConceptStore, tenant uuid.UUID, actor string) *Tools {
	return &Tools{c: cs, t: tenant, actor: actor}
}

type writeResult struct {
	Path    string `json:"path"`
	Version int    `json:"version"`
}

type conflictResult struct {
	Conflict       bool   `json:"conflict"`
	CurrentVersion int    `json:"current_version"`
	CurrentBody    string `json:"current_body"`
}

type searchHit struct {
	Path        string  `json:"path"`
	Type        string  `json:"type"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
}

type grepHit struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet"`
}

type listing struct {
	Paths  []string `json:"paths"`
	Counts *okf.Map `json:"counts"`
}

type concept struct {
	Path        string   `json:"path"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	Frontmatter *okf.Map `json:"frontmatter"`
	Version     int      `json:"version"`
	Links       []string `json:"links"`
	Backlinks   []string `json:"backlinks"`
}

// textField reads an absent field as empty. A present one must already be a string, so
// that null cannot overwrite what is stored.
func textField(kw map[string]any, name string) (string, error) {
	v, ok := kw[name]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", toolErr(name + " must be a string")
	}
	return s, nil
}

func (t *Tools) concept(path string, kw map[string]any) (okf.Concept, error) {
	body, err := textField(kw, "body")
	if err != nil {
		return okf.Concept{}, err
	}
	// Absent means empty; anything present must be an object, or null would erase.
	frontmatter := okf.NewMap()
	if v, ok := kw["frontmatter"]; ok {
		if frontmatter, ok = v.(*okf.Map); !ok || frontmatter == nil {
			return okf.Concept{}, toolErr("frontmatter must be an object")
		}
	}
	c := okf.Concept{Path: path, Body: body, Frontmatter: frontmatter}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"type", &c.Type}, {"title", &c.Title}, {"description", &c.Description}} {
		if *f.dst, err = textField(kw, f.name); err != nil {
			return okf.Concept{}, err
		}
	}
	// Derived, never taken from the caller: a `links` argument is deliberately ignored.
	c.Links = okf.ExtractLinks(body, path)
	if errs := okf.Validate(c); len(errs) > 0 {
		return okf.Concept{}, toolErr(strings.Join(errs, "; "))
	}
	return c, nil
}

func (t *Tools) Create(ctx context.Context, path string, kw map[string]any) (writeResult, error) {
	c, err := t.concept(path, kw)
	if err != nil {
		return writeResult{}, err
	}
	version, created, err := t.c.Create(ctx, t.t, c, t.actor)
	if err != nil {
		return writeResult{}, err
	}
	if !created {
		return writeResult{}, toolErr("concept already exists at " + path)
	}
	return writeResult{path, version}, nil
}

// Update returns a writeResult, or a conflictResult when expectedVersion is stale.
func (t *Tools) Update(ctx context.Context, path string, expectedVersion *int, kw map[string]any) (any, error) {
	existing, err := t.c.Read(ctx, t.t, path)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, toolErr("no concept at " + path)
	}
	return t.write(ctx, *existing, path, expectedVersion, kw)
}

// write writes kw over the concept the caller already read.
func (t *Tools) write(ctx context.Context, existing okf.Concept, path string, expectedVersion *int, kw map[string]any) (any, error) {
	merged := map[string]any{
		"type": existing.Type, "title": existing.Title, "description": existing.Description,
		"body": existing.Body, "frontmatter": existing.Frontmatter,
	}
	for k, v := range kw {
		merged[k] = v
	}
	c, err := t.concept(path, merged)
	if err != nil {
		return nil, err
	}
	version, conflict, err := t.c.Update(ctx, t.t, c, t.actor, expectedVersion)
	if errors.Is(err, store.ErrNotFound) {
		// Another tenant's row is hidden too: a distinguishable answer here would be
		// a cross-tenant existence oracle.
		return nil, toolErr("no concept at " + path)
	}
	if err != nil {
		return nil, err
	}
	if conflict != nil {
		return conflictResult{true, conflict.CurrentVersion, conflict.CurrentBody}, nil
	}
	return writeResult{path, version}, nil
}

func (t *Tools) Search(ctx context.Context, query string, limit int, prefix *string) ([]searchHit, error) {
	hits, err := t.c.Search(ctx, t.t, query, limit, prefix)
	if err != nil {
		return nil, err
	}
	return convert(hits, func(h store.Hit) searchHit { return searchHit(h) }), nil
}

func (t *Tools) Grep(ctx context.Context, pattern string, limit int) ([]grepHit, error) {
	hits, err := t.c.Grep(ctx, t.t, pattern, limit)
	var ge *store.GrepError
	if errors.As(err, &ge) {
		return nil, toolErr(ge.Msg)
	}
	if err != nil {
		return nil, err
	}
	return convert(hits, func(h store.GrepHit) grepHit { return grepHit(h) }), nil
}

func (t *Tools) List(ctx context.Context, prefix string) (listing, error) {
	rows, err := t.c.List(ctx, t.t, prefix)
	if err != nil {
		return listing{}, err
	}
	out := listing{Paths: []string{}, Counts: okf.NewMap()}
	for _, r := range rows {
		out.Paths = append(out.Paths, r.Path)
		n, _ := out.Counts.Get(r.Type)
		count, _ := n.(int)
		out.Counts.Set(r.Type, count+1)
	}
	return out, nil
}

// Read returns nil when nothing is stored at path.
func (t *Tools) Read(ctx context.Context, path string) (*concept, error) {
	c, backlinks, err := t.c.ReadWithBacklinks(ctx, t.t, path)
	if err != nil || c == nil {
		return nil, err
	}
	return &concept{c.Path, c.Type, c.Title, c.Description, c.Body, c.Frontmatter, c.Version, c.Links, backlinks}, nil
}

// Relate appends the edge, retrying past concurrent writers: appending a link
// commutes, so a compare-and-swap conflict here is nobody's decision to make.
func (t *Tools) Relate(ctx context.Context, fromPath, toPath string) (any, error) {
	for range relateAttempts {
		source, err := t.c.Read(ctx, t.t, fromPath)
		if err != nil {
			return nil, err
		}
		if source == nil {
			return nil, toolErr("no concept at " + fromPath)
		}
		if slices.Contains(source.Links, toPath) {
			// Idempotent: a retrying agent MUST NOT append the link twice.
			return writeResult{fromPath, source.Version}, nil
		}
		// Rooted, not relative: a bare to_path would resolve against the source's own directory.
		body := source.Body + "\n\n[" + toPath + "](/" + toPath + ".md)\n"
		version := source.Version
		result, err := t.write(ctx, *source, fromPath, &version, map[string]any{"body": body})
		if err != nil {
			return nil, err
		}
		if _, conflict := result.(conflictResult); !conflict {
			return result, nil
		}
	}
	return nil, toolErr(fromPath + " is being rewritten faster than the link could be recorded")
}
