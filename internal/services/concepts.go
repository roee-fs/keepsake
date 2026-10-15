// The seven tool bodies, ported from 2de90d2:src/keepsake/server/tools.py. The tenant comes from
// the request's caller, so no tool takes one, and no tool returns a body the agent could not read.
package services

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/models"
	"github.com/roee-fs/keepsake/internal/repositories"
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

type ConceptService struct {
	c     *repositories.ConceptRepository
	t     uuid.UUID
	actor string
}

func NewConceptService(cs *repositories.ConceptRepository, tenant uuid.UUID, actor string) *ConceptService {
	return &ConceptService{c: cs, t: tenant, actor: actor}
}

// As returns a copy acting for tenant as actor.
func (t *ConceptService) As(tenant uuid.UUID, actor string) *ConceptService {
	scoped := *t
	scoped.t, scoped.actor = tenant, actor
	return &scoped
}

func (t *ConceptService) Tenant() uuid.UUID { return t.t }

func (t *ConceptService) Actor() string { return t.actor }

// logRead records a read for the console's reads chart. A failure costs a chart point, not the read.
func (t *ConceptService) logRead(ctx context.Context) {
	if err := t.c.LogRead(ctx, t.t); err != nil {
		slog.Warn("read not logged", "tenant", t.t, "err", err)
	}
}

// PoolSize is the most database connections the service can hold at once.
func (t *ConceptService) PoolSize() int { return t.c.PoolSize() }

type WriteResult struct {
	Path    string `json:"path"`
	Version int    `json:"version"`
}

type ConflictResult struct {
	Conflict       bool   `json:"conflict"`
	CurrentVersion int    `json:"current_version"`
	CurrentBody    string `json:"current_body"`
}

type SearchHit struct {
	Path        string  `json:"path"`
	Type        string  `json:"type"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
}

func hitOf(h models.Hit) SearchHit {
	return SearchHit{Path: h.Path, Type: h.Type, Title: h.Title, Description: h.Description, Score: h.Score}
}

// Card is a search result: a SearchHit, the passages that match, and the OKF §5 signals.
type Card struct {
	SearchHit
	Snippet string `json:"snippet"`
	okf.Signals
}

type GrepHit struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet"`
}

type Listing struct {
	Paths  []string `json:"paths"`
	Counts *okf.Map `json:"counts"`
}

type Concept struct {
	Path        string   `json:"path"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	Frontmatter *okf.Map `json:"frontmatter"`
	Version     int      `json:"version"`
	Links       []string `json:"links"`
	Backlinks   []string `json:"backlinks"`
	okf.Signals
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

func (t *ConceptService) concept(path string, kw map[string]any, stamp bool) (okf.Concept, error) {
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
		generated := okf.NewMap()
		generated.Set("by", t.actor)
		generated.Set("at", time.Now().UTC().Format(time.RFC3339))
		c.Frontmatter.Set("generated", generated)
	}
	if errs := okf.Validate(c); len(errs) > 0 {
		return okf.Concept{}, toolErr(strings.Join(errs, "; "))
	}
	return c, nil
}

func (t *ConceptService) Create(ctx context.Context, path string, kw map[string]any) (WriteResult, error) {
	c, err := t.concept(path, kw, true)
	if err != nil {
		return WriteResult{}, err
	}
	version, created, err := t.c.Create(ctx, t.t, c, t.actor)
	if err != nil {
		return WriteResult{}, err
	}
	if !created {
		return WriteResult{}, toolErr("concept already exists at " + path)
	}
	return WriteResult{path, version}, nil
}

// Update returns a WriteResult, or a ConflictResult when expectedVersion is stale.
func (t *ConceptService) Update(ctx context.Context, path string, expectedVersion *int, kw map[string]any) (any, error) {
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
func (t *ConceptService) write(ctx context.Context, existing okf.Concept, path string, expectedVersion *int, kw map[string]any, stamp bool) (any, error) {
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
	if errors.Is(err, models.ErrNotFound) {
		// Another tenant's row gets the same answer, or this would be a cross-tenant existence oracle.
		return nil, toolErr("no concept at " + path)
	}
	if err != nil {
		return nil, err
	}
	if conflict != nil {
		return ConflictResult{true, conflict.CurrentVersion, conflict.CurrentBody}, nil
	}
	return WriteResult{path, version}, nil
}

func (t *ConceptService) Search(ctx context.Context, query string, limit int, prefix *string) ([]Card, error) {
	hits, err := t.c.Search(ctx, t.t, query, limit, prefix)
	if err != nil {
		return nil, err
	}
	t.logRead(ctx)
	now := time.Now()
	return convert(hits, func(h models.Hit) Card {
		return Card{hitOf(h), h.Snippet, okf.Derive(h.Frontmatter, now)}
	}), nil
}

func (t *ConceptService) Grep(ctx context.Context, pattern string, limit int) ([]GrepHit, error) {
	hits, err := grep(ctx, t.c, t.t, pattern, limit)
	if err != nil {
		return nil, err
	}
	t.logRead(ctx)
	return hits, nil
}

// grep is Grep for both services: a pattern Postgres refuses is the caller's mistake.
func grep(ctx context.Context, r *repositories.ConceptRepository, tenant uuid.UUID, pattern string, limit int) ([]GrepHit, error) {
	hits, err := r.Grep(ctx, tenant, pattern, limit)
	var ge *models.GrepError
	if errors.As(err, &ge) {
		return nil, toolErr(ge.Msg)
	}
	if err != nil {
		return nil, err
	}
	return convert(hits, func(h models.GrepHit) GrepHit { return GrepHit(h) }), nil
}

func (t *ConceptService) List(ctx context.Context, prefix string) (Listing, error) {
	rows, err := t.c.List(ctx, t.t, prefix)
	if err != nil {
		return Listing{}, err
	}
	t.logRead(ctx)
	out := Listing{Paths: []string{}, Counts: okf.NewMap()}
	for _, r := range rows {
		out.Paths = append(out.Paths, r.Path)
		n, _ := out.Counts.Get(r.Type)
		count, _ := n.(int)
		out.Counts.Set(r.Type, count+1)
	}
	return out, nil
}

// Read returns nil when nothing is stored at path.
func (t *ConceptService) Read(ctx context.Context, path string) (*Concept, error) {
	c, backlinks, err := t.c.ReadWithBacklinks(ctx, t.t, path)
	if err != nil {
		return nil, err
	}
	t.logRead(ctx)
	if c == nil {
		return nil, nil
	}
	return &Concept{c.Path, c.Type, c.Title, c.Description, c.Body, c.Frontmatter, c.Version, c.Links, backlinks,
		okf.Derive(c.Frontmatter, time.Now())}, nil
}

// Relate appends the edge, retrying past concurrent writers, since appending a link commutes.
func (t *ConceptService) Relate(ctx context.Context, fromPath, toPath string) (any, error) {
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
			return WriteResult{fromPath, source.Version}, nil
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
		if _, conflict := result.(ConflictResult); !conflict {
			return result, nil
		}
	}
	return nil, toolErr(fromPath + " is being rewritten faster than the link could be recorded")
}
