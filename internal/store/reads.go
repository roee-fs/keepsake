// Concept reads, ported from 8f2af2e:src/keepsake/store/concepts.py with its SQL verbatim.
package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/roee-fs/keepsake/okf"
)

// readCols is _READ_COLS: path, then _FIELDS, then version — Concept's own field order.
var readCols = "path, " + strings.Join(fields, ", ") + ", version"

const summaryCols = "path, type, title, description, version, updated_at, tenant_id"

// grepTimeoutMS bounds a grep's statement timeout; a var so a test can shrink it.
var grepTimeoutMS = 5000

// word finds runs of letters, digits and underscore: Python's \w under re.UNICODE.
var word = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// Revision is one entry in the revision log, without the snapshot that holds the body.
type Revision struct {
	Path      string
	Version   int
	Op        string
	UpdatedBy string
	CreatedAt time.Time
	TenantID  uuid.UUID
	// Day is CreatedAt's date in the session TimeZone, as psycopg dates it.
	Day string
}

// Hit is one search result. It carries no body: the agent searches, chooses, then reads.
type Hit struct {
	Path        string
	Type        string
	Title       string
	Description string
	Score       float64
	Status      string // "deprecated", "stale", or empty
}

// Summary is a concept table row: Hit without the score, plus TenantID for mixed-tenant pages.
type Summary struct {
	Path        string
	Type        string
	Title       string
	Description string
	Version     int
	UpdatedAt   time.Time
	TenantID    uuid.UUID
}

// Totals are corpus-wide counts for the admin console's summary tiles.
type Totals struct {
	Concepts  int
	ByType    map[string]int
	Revisions int
	Links     int
	Orphans   int
}

// PathType is one (path, type) pair, as List returns.
type PathType struct {
	Path string
	Type string
}

// DailyWrite is one day's concept-revision count.
type DailyWrite struct {
	Date  time.Time
	Count int
}

// TenantCount is one tenant and how many concepts it holds.
type TenantCount struct {
	TenantID uuid.UUID
	Count    int
}

// GrepHit is one (path, snippet) match; the snippet is a card, not a body.
type GrepHit struct {
	Path    string
	Snippet string
}

// GraphRow is (path, type, title, links) for one concept, as Graph returns.
type GraphRow struct {
	Path  string
	Type  string
	Title string
	Links []string
}

// GrepError reports a pattern Postgres could not compile or match within the timeout.
type GrepError struct{ Msg string }

func (e *GrepError) Error() string { return e.Msg }

func tsquery(query string) string {
	return strings.Join(word.FindAllString(query, -1), " | ")
}

// connect dispatches tenant=nil to AdminScope and any other tenant to Scope, as Python's _connect.
func (cs *ConceptStore) connect(ctx context.Context, tenant *uuid.UUID, fn func(pgx.Tx) error) error {
	if tenant == nil {
		return cs.s.AdminScope(ctx, fn)
	}
	return cs.s.Scope(ctx, *tenant, fn)
}

// scanConcept scans readCols, then extra.
func scanConcept(row pgx.Row, extra ...any) (okf.Concept, error) {
	var c okf.Concept
	var fm []byte
	dest := append([]any{&c.Path, &c.Type, &c.Title, &c.Description, &c.Body, &fm, &c.Links, &c.Version}, extra...)
	if err := row.Scan(dest...); err != nil {
		return okf.Concept{}, err
	}
	c.Frontmatter = okf.NewMap()
	if err := json.Unmarshal(fm, c.Frontmatter); err != nil {
		return okf.Concept{}, err
	}
	c.Links = links(c.Links)
	return c, nil
}

// backlinksSQL is _BACKLINKS: sequential, since arraycontains is not leakproof under forced RLS.
const backlinksSQL = "ARRAY(SELECT b.path FROM concept b WHERE $2 = ANY(b.links) ORDER BY b.path)"

// collect runs sql and scans every row into a T by column position.
func collect[T any](ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]T, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[T])
}

// Read returns the whole concept, body included. nil means no such path.
func (cs *ConceptStore) Read(ctx context.Context, tenant uuid.UUID, path string) (*okf.Concept, error) {
	var out *okf.Concept
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		c, err := scanConcept(tx.QueryRow(ctx, "SELECT "+readCols+" FROM concept WHERE path = $1", path))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = &c
		return nil
	})
	return out, err
}

// readWithBacklinks reads the concept and its backlinks in one statement, so from one snapshot.
func readWithBacklinks(ctx context.Context, tx pgx.Tx, path string) (*okf.Concept, []string, error) {
	var bl []string
	c, err := scanConcept(tx.QueryRow(ctx,
		"SELECT "+readCols+", "+backlinksSQL+" FROM concept WHERE path = $1", path, path), &bl)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &c, links(bl), nil
}

// ReadWithBacklinks returns the concept and the paths linking to it. nil means no such path.
func (cs *ConceptStore) ReadWithBacklinks(ctx context.Context, tenant uuid.UUID, path string) (c *okf.Concept, backlinks []string, err error) {
	err = cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		c, backlinks, err = readWithBacklinks(ctx, tx, path)
		return err
	})
	return c, backlinks, err
}

// Detail is ReadWithBacklinks plus the path's newest limit revisions, in one transaction.
func (cs *ConceptStore) Detail(ctx context.Context, tenant uuid.UUID, path string, limit int) (c *okf.Concept, backlinks []string, history []Revision, err error) {
	err = cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		if c, backlinks, err = readWithBacklinks(ctx, tx, path); err != nil || c == nil {
			return err
		}
		history, err = collect[Revision](ctx, tx,
			"SELECT path, version, op, coalesce(updated_by, ''), created_at, tenant_id, "+
				"to_char(created_at, 'YYYY-MM-DD') "+
				"FROM concept_revision WHERE path = $1 "+
				"ORDER BY version DESC LIMIT $2", path, limit)
		return err
	})
	return c, backlinks, history, err
}

// ReadAll returns every concept, body included, path-ordered, from one snapshot.
func (cs *ConceptStore) ReadAll(ctx context.Context, tenant uuid.UUID) ([]okf.Concept, error) {
	var out []okf.Concept
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT "+readCols+" FROM concept ORDER BY path")
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (okf.Concept, error) { return scanConcept(row) })
		return err
	})
	return out, err
}

// Revisions returns the newest limit revisions, oldest first.
func (cs *ConceptStore) Revisions(ctx context.Context, tenant uuid.UUID, limit int) ([]Revision, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []Revision
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = collect[Revision](ctx, tx,
			"SELECT path, version, op, updated_by, created_at, tenant_id,"+
				" to_char(created_at, 'YYYY-MM-DD') FROM ("+
				"  SELECT path, version, op, coalesce(updated_by, '') AS updated_by,"+
				"         created_at, tenant_id"+
				"  FROM concept_revision"+
				"  ORDER BY created_at DESC, path DESC, version DESC LIMIT $1"+
				") recent ORDER BY created_at, path, version",
			limit)
		return err
	})
	return out, err
}

// List returns (path, type) for every concept under prefix. An empty prefix is all.
func (cs *ConceptStore) List(ctx context.Context, tenant uuid.UUID, prefix string) ([]PathType, error) {
	var out []PathType
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = collect[PathType](ctx, tx,
			"SELECT path, type FROM concept WHERE starts_with(path, $1) ORDER BY path", prefix)
		return err
	})
	return out, err
}

// Page returns a path-ordered page under prefix and its total, in one transaction. nil mixes tenants.
func (cs *ConceptStore) Page(ctx context.Context, tenant *uuid.UUID, prefix string, limit, offset int) (page []Summary, total int, err error) {
	err = cs.connect(ctx, tenant, func(tx pgx.Tx) error {
		if limit > 0 {
			// Two tenants can share a path, so tenant_id breaks the tie.
			if page, err = collect[Summary](ctx, tx,
				fmt.Sprintf("SELECT %s FROM concept WHERE starts_with(path, $1) "+
					"ORDER BY path, tenant_id LIMIT $2 OFFSET $3", summaryCols),
				prefix, limit, offset); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, "SELECT count(*) FROM concept WHERE starts_with(path, $1)", prefix).Scan(&total)
	})
	return page, total, err
}

// Totals are corpus-wide counts for the admin console's summary tiles.
func (cs *ConceptStore) Totals(ctx context.Context, tenant *uuid.UUID) (Totals, error) {
	var t Totals
	err := cs.connect(ctx, tenant, func(tx pgx.Tx) error {
		byType, err := collect[struct {
			Type         string
			Count, Links int
		}](ctx, tx, "SELECT type, count(*), coalesce(sum(cardinality(links)), 0) "+
			"FROM concept GROUP BY type")
		if err != nil {
			return err
		}
		t.ByType = map[string]int{}
		for _, row := range byType {
			t.ByType[row.Type] = row.Count
			t.Concepts += row.Count
			t.Links += row.Links
		}

		if err := tx.QueryRow(ctx, "SELECT count(*) FROM concept_revision").Scan(&t.Revisions); err != nil {
			return err
		}

		// Correlated on tenant_id, and unnested so the anti-join hashes on the path.
		return tx.QueryRow(ctx,
			"SELECT count(*) FROM concept c WHERE NOT EXISTS ("+
				"  SELECT 1 FROM concept b, unnest(b.links) AS l(target)"+
				"  WHERE b.tenant_id = c.tenant_id AND l.target = c.path)",
		).Scan(&t.Orphans)
	})
	return t, err
}

// Activity returns the most recent limit revisions, newest first, unlike Revisions.
func (cs *ConceptStore) Activity(ctx context.Context, tenant *uuid.UUID, limit int) ([]Revision, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []Revision
	err := cs.connect(ctx, tenant, func(tx pgx.Tx) (err error) {
		// tenant_id makes the order total: two tenants can share a path, version and timestamp.
		out, err = collect[Revision](ctx, tx,
			"SELECT path, version, op, coalesce(updated_by, ''), created_at, tenant_id, "+
				"to_char(created_at, 'YYYY-MM-DD') "+
				"FROM concept_revision "+
				"ORDER BY created_at DESC, path DESC, version DESC, tenant_id DESC "+
				"LIMIT $1", limit)
		return err
	})
	return out, err
}

// DailyWrites returns revision counts for the last days days, oldest first and zero-filled.
func (cs *ConceptStore) DailyWrites(ctx context.Context, tenant *uuid.UUID, days int) ([]DailyWrite, error) {
	if days <= 0 {
		return nil, nil
	}
	var out []DailyWrite
	err := cs.connect(ctx, tenant, func(tx pgx.Tx) (err error) {
		// The lower bound repeats the series start so the created_at index can serve the join.
		out, err = collect[DailyWrite](ctx, tx,
			"SELECT d::date, count(r.created_at) FROM generate_series("+
				"  (current_date - ($1::int - 1))::timestamp, current_date::timestamp,"+
				"  interval '1 day'"+
				") AS d "+
				"LEFT JOIN concept_revision r ON r.created_at::date = d::date"+
				"  AND r.created_at >= (current_date - ($1::int - 1))::timestamp "+
				"GROUP BY d ORDER BY d", days)
		return err
	})
	return out, err
}

// Tenants lists every tenant with concepts, and how many; there is no other tenant registry.
func (cs *ConceptStore) Tenants(ctx context.Context) ([]TenantCount, error) {
	var out []TenantCount
	err := cs.s.AdminScope(ctx, func(tx pgx.Tx) (err error) {
		out, err = collect[TenantCount](ctx, tx,
			"SELECT tenant_id, count(*) FROM concept GROUP BY tenant_id ORDER BY tenant_id")
		return err
	})
	return out, err
}

// searchSQL is BM25 (k1=0.9, b=0.4) over posting. Document frequency is exact: OR-ed terms
// make every concept holding a term a hit. avgdl is over the hits, which BEIR scores the
// same as over the tenant. $1 is terms, $2 prefix, $3 limit, $4 the tenant, which RLS
// enforces anyway; naming it lets the planner lead with it.
//
// It fetches 2×limit; Search demotes deprecated and stale concepts and trims to limit.
// ponytail: demotion reorders only the top 2×limit; rank over every hit if deep demotions matter.
//
// MATERIALIZED, or the planner inlines docs and avgdl and recounts them once per hit, and
// repeats the card lookup once per ranked path. Cards come by `path = ANY`, a primary-key
// index condition under RLS; a join on path was demoted to a filter over the whole tenant.
const searchSQL = `
WITH terms AS (SELECT DISTINCT unnest(tsvector_to_array(to_tsvector('english', $1))) AS lexeme),
docs AS MATERIALIZED (SELECT count(*)::float8 AS n FROM concept WHERE tenant_id = $4),
hits AS MATERIALIZED (
  SELECT p.path, p.lexeme, p.tf::float8 AS tf, p.dl::float8 AS dl
  FROM posting p JOIN terms t ON p.tenant_id = $4 AND p.lexeme = t.lexeme
),
df AS MATERIALIZED (SELECT lexeme, count(*)::float8 AS n FROM hits GROUP BY lexeme),
avgdl AS MATERIALIZED (SELECT avg(dl) AS a FROM (SELECT DISTINCT path, dl FROM hits) d),
ranked AS MATERIALIZED (
  SELECT h.path, sum(ln(1 + (docs.n - df.n + 0.5) / (df.n + 0.5))
                     * h.tf * 1.9 / (h.tf + 0.9 * (0.6 + 0.4 * h.dl / avgdl.a))) AS score
  FROM hits h JOIN df ON df.lexeme = h.lexeme, docs, avgdl
  WHERE starts_with(h.path, coalesce($2::text, ''))
  GROUP BY h.path
  ORDER BY score DESC, h.path
  LIMIT $3 * 2
),
cards AS MATERIALIZED (
  SELECT c.path, c.type, c.title, c.description,
         coalesce(c.frontmatter->>'status', ''), coalesce(c.frontmatter->>'stale_after', '')
  FROM concept c
  WHERE c.tenant_id = $4 AND c.path = ANY (ARRAY(SELECT path FROM ranked))
)
SELECT c.*, r.score
FROM ranked r JOIN cards c ON c.path = r.path
ORDER BY r.score DESC, c.path`

type searchRow struct {
	Path, Type, Title, Description, Status, StaleAfter string
	Score                                              float64
}

// staleAt reads stale_after, an ISO 8601 instant, or a bare date as other OKF tools write it.
func staleAt(v string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	t, err := time.Parse(time.DateOnly, v)
	return t, err == nil
}

// Search returns ranked cards, never bodies. An empty result beats an invalid query.
// A deprecated concept scores 0.3 times its BM25 and a stale one 0.6.
func (cs *ConceptStore) Search(ctx context.Context, tenant uuid.UUID, query string, limit int, prefix *string) ([]Hit, error) {
	terms := tsquery(query)
	if terms == "" || limit <= 0 {
		return nil, nil
	}
	var rows []searchRow
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) (err error) {
		rows, err = collect[searchRow](ctx, tx, searchSQL, terms, prefix, limit, tenant)
		return err
	})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]Hit, 0, len(rows))
	for _, r := range rows {
		h := Hit{r.Path, r.Type, r.Title, r.Description, r.Score, ""}
		if r.Status == "deprecated" {
			h.Status, h.Score = "deprecated", h.Score*0.3
		} else if t, ok := staleAt(r.StaleAfter); ok && !now.Before(t) {
			h.Status, h.Score = "stale", h.Score*0.6
		}
		out = append(out, h)
	}
	slices.SortStableFunc(out, func(a, b Hit) int { return cmp.Compare(b.Score, a.Score) })
	return out[:min(limit, len(out))], nil
}

// grepSQL is _GREP: the snippet comes from the match position in one haystack the predicate also reads.
const grepSQL = `
SELECT c.path,
       btrim(regexp_replace(
         substr(h.hay, greatest(m.pos - 60, 1), 200),
         '\s+', ' ', 'g'))
FROM concept AS c,
     LATERAL (SELECT concat_ws(chr(10), c.title, c.description, c.body)) AS h(hay),
     LATERAL (SELECT regexp_instr(h.hay, $1, 1, 1, 0, 'i')) AS m(pos)
WHERE m.pos > 0
ORDER BY c.path
LIMIT $2
`

// Grep returns (path, snippet) matches of a POSIX regex, or a *GrepError for a bad or slow one.
func (cs *ConceptStore) Grep(ctx context.Context, tenant uuid.UUID, pattern string, limit int) ([]GrepHit, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []GrepHit
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) (err error) {
		// SET LOCAL takes no parameter; the value is an int literal in this file.
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", grepTimeoutMS)); err != nil {
			return err
		}
		out, err = collect[GrepHit](ctx, tx, grepSQL, pattern, limit)
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "2201B": // invalid_regular_expression
				return nil, &GrepError{Msg: fmt.Sprintf("unusable regular expression: %s", okf.PyReprString(pattern))}
			case "57014": // query_canceled
				return nil, &GrepError{Msg: fmt.Sprintf(
					"the regular expression took longer than %dms: %s", grepTimeoutMS, okf.PyReprString(pattern))}
			}
		}
		return nil, err
	}
	return out, nil
}

// Graph returns (path, type, title, links) for the first limit concepts by path.
func (cs *ConceptStore) Graph(ctx context.Context, tenant uuid.UUID, limit int) ([]GraphRow, error) {
	var out []GraphRow
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = collect[GraphRow](ctx, tx,
			"SELECT path, type, title, links FROM concept ORDER BY path LIMIT $1", limit)
		return err
	})
	return out, err
}
