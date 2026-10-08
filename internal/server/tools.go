// The seven tool bodies, ported from 2de90d2:src/keepsake/server/tools.py. The tenant comes from
// the request's caller, so no tool takes one, and no tool returns a body the agent could not read.
package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

// relateAttempts bounds Relate's retries past concurrent writers; each round has one winner.
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

func hitOf(h store.Hit) searchHit {
	return searchHit{Path: h.Path, Type: h.Type, Title: h.Title, Description: h.Description, Score: h.Score}
}

// card is a search result: a searchHit, the passages that match, and the OKF §5 signals.
type card struct {
	searchHit
	Snippet string `json:"snippet"`
	okf.Signals
}

type grepHit struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet"`
}

type listing struct {
	Paths     []string `json:"paths"`
	Counts    *okf.Map `json:"counts"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated"`
}

type concept struct {
	Path                 string   `json:"path"`
	Type                 string   `json:"type"`
	Title                string   `json:"title"`
	Description          string   `json:"description"`
	Body                 string   `json:"body"`
	BodyChars            int      `json:"body_chars"`
	NextOffset           *int     `json:"next_offset"`
	Frontmatter          *okf.Map `json:"frontmatter"`
	FrontmatterTruncated bool     `json:"frontmatter_truncated"`
	Version              int      `json:"version"`
	Links                []string `json:"links"`
	LinksCount           int      `json:"links_count"`
	Backlinks            []string `json:"backlinks"`
	BacklinksCount       int      `json:"backlinks_count"`
	okf.Signals
}

// readOptions bounds a Read. A zero MaxChars means DefaultBodyChars.
type readOptions struct {
	Offset   int
	MaxChars int
	// Include names the provenanceKeys to return.
	Include []string
}

// textField reads an absent field as empty and refuses a non-string, so null cannot erase a field.
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

func (t *Tools) concept(path string, kw map[string]any, stamp bool) (okf.Concept, error) {
	body, err := textField(kw, "body")
	if err != nil {
		return okf.Concept{}, err
	}
	// As import does, so an exported bundle is LF-only however the body arrived.
	body = strings.ReplaceAll(body, "\r\n", "\n")
	// Absent means empty; anything present MUST be an object, or null would erase.
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
	// The server knows who wrote through it, so its stamp replaces any the caller sent.
	if stamp {
		// A copy, so the caller's map is left as it was.
		c.Frontmatter = frontmatter.Clone()
		c.Frontmatter.Set("generated", obj("by", t.actor, "at", time.Now().UTC().Format(time.RFC3339)))
	}
	if errs := okf.Validate(c); len(errs) > 0 {
		return okf.Concept{}, toolErr(strings.Join(errs, "; "))
	}
	return c, nil
}

func (t *Tools) Create(ctx context.Context, path string, kw map[string]any) (writeResult, error) {
	c, err := t.concept(path, kw, true)
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
	return t.write(ctx, *existing, path, expectedVersion, kw, true)
}

// write writes kw over the concept the caller already read. Unless stamp is set it keeps the authorship stamp.
func (t *Tools) write(ctx context.Context, existing okf.Concept, path string, expectedVersion *int, kw map[string]any, stamp bool) (any, error) {
	merged := map[string]any{
		"type": existing.Type, "title": existing.Title, "description": existing.Description,
		"body": existing.Body, "frontmatter": existing.Frontmatter,
	}
	for k, v := range kw {
		merged[k] = v
	}
	c, err := t.concept(path, merged, stamp)
	if err != nil {
		return nil, err
	}
	version, conflict, err := t.c.Update(ctx, t.t, c, t.actor, expectedVersion)
	if errors.Is(err, store.ErrNotFound) {
		// Another tenant's row gets the same answer, or this would be a cross-tenant existence oracle.
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

func (t *Tools) Search(ctx context.Context, query string, limit int, prefix *string) ([]card, error) {
	hits, err := t.c.Search(ctx, t.t, query, limit, prefix)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return convert(hits, func(h store.Hit) card {
		return card{hitOf(h), clip(h.Snippet, SnippetBytes), okf.Derive(h.Frontmatter, now)}
	}), nil
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
	return convert(hits, func(h store.GrepHit) grepHit { return grepHit{h.Path, clip(h.Snippet, SnippetBytes)} }), nil
}

// List returns the first limit paths under prefix, with counts and a total over all of them.
func (t *Tools) List(ctx context.Context, prefix string, limit int) (listing, error) {
	rows, err := t.c.List(ctx, t.t, prefix)
	if err != nil {
		return listing{}, err
	}
	out := listing{Paths: []string{}, Counts: okf.NewMap(), Total: len(rows), Truncated: len(rows) > limit}
	for i, r := range rows {
		if i < limit {
			out.Paths = append(out.Paths, r.Path)
		}
		n, _ := out.Counts.Get(r.Type)
		count, _ := n.(int)
		out.Counts.Set(r.Type, count+1)
	}
	return out, nil
}

// Read returns one page of the concept at path, or nil when nothing is stored there.
func (t *Tools) Read(ctx context.Context, path string, o readOptions) (*concept, error) {
	c, backlinks, err := t.c.ReadWithBacklinks(ctx, t.t, path)
	if err != nil || c == nil {
		return nil, err
	}
	if o.MaxChars == 0 {
		o.MaxChars = DefaultBodyChars
	}
	// Characters, not bytes, so a page never splits one.
	body := []rune(c.Body)
	start := min(o.Offset, len(body))
	end := min(start+o.MaxChars, len(body))
	var next *int
	if end < len(body) {
		next = &end
	}
	frontmatter, truncated := boundFrontmatter(c.Frontmatter, o.Include)
	return &concept{c.Path, c.Type, c.Title, c.Description, string(body[start:end]), len(body), next,
		frontmatter, truncated, c.Version, c.Links[:min(len(c.Links), MaxLinks)], len(c.Links),
		backlinks[:min(len(backlinks), MaxLinks)], len(backlinks),
		okf.Derive(c.Frontmatter, time.Now())}, nil
}

// boundFrontmatter drops provenance not in include, then every key from the first that overruns
// FrontmatterBytes on. Identifying and included keys are always kept and never count.
func boundFrontmatter(fm *okf.Map, include []string) (*okf.Map, bool) {
	out, budget, truncated := okf.NewMap(), FrontmatterBytes, false
	for _, k := range fm.Keys() {
		v, _ := fm.Get(k)
		provenance := slices.Contains(provenanceKeys, k)
		if provenance && !slices.Contains(include, k) {
			continue
		}
		if !provenance && !slices.Contains(identifyingKeys, k) {
			if truncated {
				continue
			}
			text, _ := pyDumps(v)
			// Quotes, ": " and ", ", as the text content spells them.
			size := len(k) + len(text) + 6
			if truncated = size > budget; truncated {
				continue
			}
			budget -= size
		}
		out.Set(k, v)
	}
	return out, truncated
}

// clip cuts s to at most n bytes without splitting a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Relate appends the edge, retrying past concurrent writers, since appending a link commutes.
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
		body := strings.TrimRight(source.Body, "\n") + "\n\n[" + toPath + "](/" + toPath + ".md)\n"
		// A non-canonical to_path would append a link that never reads back as to_path, on every retry.
		if !slices.Contains(okf.ExtractLinks(body, fromPath), toPath) {
			return nil, toolErr("to_path " + okf.PyReprString(toPath) + " is not a concept path such as detect/dormant-rules")
		}
		version := source.Version
		// A link does not make its adder the author, and the revision still records who added it.
		result, err := t.write(ctx, *source, fromPath, &version, map[string]any{"body": body}, false)
		if err != nil {
			return nil, err
		}
		if _, conflict := result.(conflictResult); !conflict {
			return result, nil
		}
	}
	return nil, toolErr(fromPath + " is being rewritten faster than the link could be recorded")
}
