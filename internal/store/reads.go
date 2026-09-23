// Concept reads. Ported from src/keepsake/store/concepts.py (the read half; writes
// are concepts.go). Every SQL statement is copied verbatim, %s changed to $n.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

// Revision is one entry in the revision log. It carries no snapshot: that holds
// the body.
type Revision struct {
	Path      string
	Version   int
	Op        string
	UpdatedBy string
	CreatedAt time.Time
	TenantID  uuid.UUID
}

// Hit is one search result. It carries no body: the agent searches, chooses, then reads.
type Hit struct {
	Path        string
	Type        string
	Title       string
	Description string
	Score       float64
}

// Summary is one row in the admin console's concept table: Hit without the score,
// plus TenantID, since a tenant-less page mixes tenants.
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

// GrepHit is one (path, snippet) match. The snippet is a card, not a body: enough
// context to judge relevance, no more.
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

// GrepError reports a pattern Postgres could not compile, or one whose match
// exceeded the statement timeout.
type GrepError struct{ Msg string }

func (e *GrepError) Error() string { return e.Msg }

func tsquery(query string) string {
	return strings.Join(word.FindAllString(query, -1), " | ")
}

// connect dispatches tenant=nil to AdminScope, any other value to Scope. Centralised
// so a per-method copy cannot get the two backwards, mirroring Python's _connect.
func (cs *ConceptStore) connect(ctx context.Context, tenant *uuid.UUID, fn func(pgx.Tx) error) error {
	if tenant == nil {
		return cs.s.AdminScope(ctx, fn)
	}
	return cs.s.Scope(ctx, *tenant, fn)
}

func scanConcept(row pgx.Row) (okf.Concept, error) {
	var c okf.Concept
	var fm []byte
	if err := row.Scan(&c.Path, &c.Type, &c.Title, &c.Description, &c.Body, &fm, &c.Links, &c.Version); err != nil {
		return okf.Concept{}, err
	}
	return finishConcept(c, fm)
}

func finishConcept(c okf.Concept, fm []byte) (okf.Concept, error) {
	m := okf.NewMap()
	if err := json.Unmarshal(fm, m); err != nil {
		return okf.Concept{}, err
	}
	c.Frontmatter = m
	if c.Links == nil {
		c.Links = []string{}
	}
	return c, nil
}

// backlinksSQL is _BACKLINKS with its one placeholder at position n: sequential,
// not the GIN index (`links @> ARRAY[path]` is the only form it serves, and
// arraycontains is not leakproof under FORCE ROW LEVEL SECURITY).
func backlinksSQL(n int) string {
	return fmt.Sprintf("ARRAY(SELECT b.path FROM concept b WHERE $%d = ANY(b.links) ORDER BY b.path)", n)
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

// ReadWithBacklinks returns the concept and the paths linking to it. One statement,
// so the backlinks cannot be read from a later snapshot than the concept.
func (cs *ConceptStore) ReadWithBacklinks(ctx context.Context, tenant uuid.UUID, path string) (*okf.Concept, []string, error) {
	var concept *okf.Concept
	var backlinks []string
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		var c okf.Concept
		var fm []byte
		var bl []string
		err := tx.QueryRow(ctx,
			fmt.Sprintf("SELECT %s, %s FROM concept WHERE path = $1", readCols, backlinksSQL(2)),
			path, path,
		).Scan(&c.Path, &c.Type, &c.Title, &c.Description, &c.Body, &fm, &c.Links, &c.Version, &bl)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		c, err = finishConcept(c, fm)
		if err != nil {
			return err
		}
		concept = &c
		if bl == nil {
			bl = []string{}
		}
		backlinks = bl
		return nil
	})
	return concept, backlinks, err
}

// ReadAll returns every concept, body included, path-ordered, from one snapshot.
func (cs *ConceptStore) ReadAll(ctx context.Context, tenant uuid.UUID) ([]okf.Concept, error) {
	var out []okf.Concept
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT "+readCols+" FROM concept ORDER BY path")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanConcept(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

func scanRevisions(rows pgx.Rows) ([]Revision, error) {
	var out []Revision
	for rows.Next() {
		var r Revision
		if err := rows.Scan(&r.Path, &r.Version, &r.Op, &r.UpdatedBy, &r.CreatedAt, &r.TenantID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Revisions returns the most recent limit revisions, oldest first: the newest
// window, not the oldest, bounded at the wrong end otherwise shows only what a
// bundle carried first and nothing since.
func (cs *ConceptStore) Revisions(ctx context.Context, tenant uuid.UUID, limit int) ([]Revision, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []Revision
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			"SELECT path, version, op, updated_by, created_at, tenant_id FROM ("+
				"  SELECT path, version, op, coalesce(updated_by, '') AS updated_by,"+
				"         created_at, tenant_id"+
				"  FROM concept_revision"+
				"  ORDER BY created_at DESC, path DESC, version DESC LIMIT $1"+
				") recent ORDER BY created_at, path, version",
			limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanRevisions(rows)
		return err
	})
	return out, err
}

// Backlinks returns the paths whose outbound links name path.
func (cs *ConceptStore) Backlinks(ctx context.Context, tenant uuid.UUID, path string) ([]string, error) {
	var out []string
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		var bl []string
		if err := tx.QueryRow(ctx, "SELECT "+backlinksSQL(1), path).Scan(&bl); err != nil {
			return err
		}
		out = bl
		return nil
	})
	if out == nil && err == nil {
		out = []string{}
	}
	return out, err
}

// List returns (path, type) for every concept under prefix. An empty prefix is all.
func (cs *ConceptStore) List(ctx context.Context, tenant uuid.UUID, prefix string) ([]PathType, error) {
	var out []PathType
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			"SELECT path, type FROM concept WHERE starts_with(path, $1) ORDER BY path", prefix)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pt PathType
			if err := rows.Scan(&pt.Path, &pt.Type); err != nil {
				return err
			}
			out = append(out, pt)
		}
		return rows.Err()
	})
	return out, err
}

// Page returns a path-ordered page of concepts under prefix. tenant=nil mixes
// every tenant.
func (cs *ConceptStore) Page(ctx context.Context, tenant *uuid.UUID, prefix string, limit, offset int) ([]Summary, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []Summary
	err := cs.connect(ctx, tenant, func(tx pgx.Tx) error {
		// path alone is not a total order under AdminScope: two tenants can share a
		// path, so tenant_id breaks the tie the same way both ways.
		rows, err := tx.Query(ctx,
			fmt.Sprintf("SELECT %s FROM concept WHERE starts_with(path, $1) "+
				"ORDER BY path, tenant_id LIMIT $2 OFFSET $3", summaryCols),
			prefix, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s Summary
			if err := rows.Scan(&s.Path, &s.Type, &s.Title, &s.Description, &s.Version, &s.UpdatedAt, &s.TenantID); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// Count reports how many concepts Page would cover for the same tenant/prefix.
func (cs *ConceptStore) Count(ctx context.Context, tenant *uuid.UUID, prefix string) (int, error) {
	var n int
	err := cs.connect(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM concept WHERE starts_with(path, $1)", prefix).Scan(&n)
	})
	return n, err
}

// Totals are corpus-wide counts for the admin console's summary tiles.
func (cs *ConceptStore) Totals(ctx context.Context, tenant *uuid.UUID) (Totals, error) {
	var t Totals
	err := cs.connect(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			"SELECT count(*), coalesce(sum(cardinality(links)), 0) FROM concept",
		).Scan(&t.Concepts, &t.Links); err != nil {
			return err
		}

		rows, err := tx.Query(ctx, "SELECT type, count(*) FROM concept GROUP BY type")
		if err != nil {
			return err
		}
		t.ByType = map[string]int{}
		for rows.Next() {
			var typ string
			var n int
			if err := rows.Scan(&typ, &n); err != nil {
				rows.Close()
				return err
			}
			t.ByType[typ] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, "SELECT count(*) FROM concept_revision").Scan(&t.Revisions); err != nil {
			return err
		}

		// An orphan is a concept no concept of its own tenant links to. The
		// anti-join correlates on tenant_id as well as path: under AdminScope one
		// tenant's link would otherwise hide another tenant's orphan, since a path
		// is unique only within a tenant. Sequential like backlinksSQL, and for the
		// same reason: the GIN index only serves @>, and arraycontains is not
		// leakproof under FORCE ROW LEVEL SECURITY.
		return tx.QueryRow(ctx,
			"SELECT count(*) FROM concept c WHERE NOT EXISTS ("+
				"  SELECT 1 FROM concept b"+
				"  WHERE b.tenant_id = c.tenant_id AND c.path = ANY(b.links))",
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
	err := cs.connect(ctx, tenant, func(tx pgx.Tx) error {
		// Under AdminScope two tenants can hold the same path at the same version
		// and timestamp, so tenant_id is what makes the order total.
		rows, err := tx.Query(ctx,
			"SELECT path, version, op, coalesce(updated_by, ''), created_at, tenant_id "+
				"FROM concept_revision "+
				"ORDER BY created_at DESC, path DESC, version DESC, tenant_id DESC "+
				"LIMIT $1", limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanRevisions(rows)
		return err
	})
	return out, err
}

// RevisionsFor returns the most recent limit revisions of one path, newest first.
// Unlike Activity, the cap is per path, so a concept's history cannot be crowded
// out by other concepts' unrelated revisions.
func (cs *ConceptStore) RevisionsFor(ctx context.Context, tenant *uuid.UUID, path string, limit int) ([]Revision, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []Revision
	err := cs.connect(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			"SELECT path, version, op, coalesce(updated_by, ''), created_at, tenant_id "+
				"FROM concept_revision WHERE path = $1 "+
				"ORDER BY version DESC LIMIT $2", path, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanRevisions(rows)
		return err
	})
	return out, err
}

// DailyWrites returns concept-revision counts for the last days days, oldest
// first and zero-filled so a quiet day does not vanish from the chart.
func (cs *ConceptStore) DailyWrites(ctx context.Context, tenant *uuid.UUID, days int) ([]DailyWrite, error) {
	if days <= 0 {
		return nil, nil
	}
	var out []DailyWrite
	err := cs.connect(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			"SELECT d::date, count(r.created_at) FROM generate_series("+
				"  (current_date - ($1::int - 1))::timestamp, current_date::timestamp,"+
				"  interval '1 day'"+
				") AS d "+
				"LEFT JOIN concept_revision r ON r.created_at::date = d::date "+
				"GROUP BY d ORDER BY d", days)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var dw DailyWrite
			if err := rows.Scan(&dw.Date, &dw.Count); err != nil {
				return err
			}
			out = append(out, dw)
		}
		return rows.Err()
	})
	return out, err
}

// Tenants lists every tenant holding at least one concept, and its count.
// Admin-only: there is no tenant registry besides this table.
func (cs *ConceptStore) Tenants(ctx context.Context) ([]TenantCount, error) {
	var out []TenantCount
	err := cs.s.AdminScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			"SELECT tenant_id, count(*) FROM concept GROUP BY tenant_id ORDER BY tenant_id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tc TenantCount
			if err := rows.Scan(&tc.TenantID, &tc.Count); err != nil {
				return err
			}
			out = append(out, tc)
		}
		return rows.Err()
	})
	return out, err
}

// Search returns ranked cards, never bodies. An empty result beats an invalid tsquery.
func (cs *ConceptStore) Search(ctx context.Context, tenant uuid.UUID, query string, limit int, prefix *string) ([]Hit, error) {
	terms := tsquery(query)
	if terms == "" || limit <= 0 {
		return nil, nil
	}
	var out []Hit
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			"SELECT path, type, title, description, "+
				"       ts_rank_cd(search, to_tsquery('english', $1)) AS score "+
				"FROM concept "+
				"WHERE search @@ to_tsquery('english', $2) "+
				"  AND starts_with(path, coalesce($3::text, '')) "+
				"ORDER BY score DESC, path LIMIT $4",
			terms, terms, prefix, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h Hit
			if err := rows.Scan(&h.Path, &h.Type, &h.Title, &h.Description, &h.Score); err != nil {
				return err
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// grep is the query the Python _GREP constant holds: the match position, not the
// matched text, since substring(body from pattern) would tell an agent nothing
// about relevance. One haystack rather than three columns keeps the snippet and
// the predicate from ever disagreeing.
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

// Grep returns (path, snippet) for every concept matching the POSIX regex pattern.
// It returns a *GrepError if Postgres cannot compile pattern, or if matching it
// exceeds the statement timeout.
func (cs *ConceptStore) Grep(ctx context.Context, tenant uuid.UUID, pattern string, limit int) ([]GrepHit, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []GrepHit
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		// SET LOCAL takes no parameter; the value is an int literal in this file.
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", grepTimeoutMS)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, grepSQL, pattern, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h GrepHit
			if err := rows.Scan(&h.Path, &h.Snippet); err != nil {
				return err
			}
			out = append(out, h)
		}
		return rows.Err()
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
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			"SELECT path, type, title, links FROM concept ORDER BY path LIMIT $1", limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g GraphRow
			if err := rows.Scan(&g.Path, &g.Type, &g.Title, &g.Links); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, err
}
