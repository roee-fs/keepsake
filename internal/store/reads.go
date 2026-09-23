// Concept reads. Ported from 8f2af2e:src/keepsake/store/concepts.py (the read half; writes
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

// backlinksSQL is _BACKLINKS: sequential, not the GIN index, because arraycontains
// is not leakproof under FORCE ROW LEVEL SECURITY.
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

// readWithBacklinks reads the concept and the paths linking to it in one statement,
// so the backlinks cannot come from a later snapshot than the concept.
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

// Detail is ReadWithBacklinks plus the path's most recent limit revisions, newest
// first, in one transaction. The cap is per path, so other concepts cannot crowd it out.
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

// Revisions returns the most recent limit revisions, oldest first: the newest
// window, not the oldest, bounded at the wrong end otherwise shows only what a
// bundle carried first and nothing since.
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

// Page returns a path-ordered page of concepts under prefix and how many there are
// in all, in one transaction. tenant=nil mixes every tenant.
func (cs *ConceptStore) Page(ctx context.Context, tenant *uuid.UUID, prefix string, limit, offset int) (page []Summary, total int, err error) {
	err = cs.connect(ctx, tenant, func(tx pgx.Tx) error {
		if limit > 0 {
			// path alone is not a total order under AdminScope: two tenants can share a
			// path, so tenant_id breaks the tie the same way both ways.
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

		// Correlated on tenant_id: paths repeat across tenants under AdminScope.
		// Unnested so the anti-join hashes on the path; = ANY(links) is quadratic.
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
		// Under AdminScope two tenants can hold the same path at the same version
		// and timestamp, so tenant_id is what makes the order total.
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

// DailyWrites returns concept-revision counts for the last days days, oldest
// first and zero-filled so a quiet day does not vanish from the chart.
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

// Tenants lists every tenant holding at least one concept, and its count.
// Admin-only: there is no tenant registry besides this table.
func (cs *ConceptStore) Tenants(ctx context.Context) ([]TenantCount, error) {
	var out []TenantCount
	err := cs.s.AdminScope(ctx, func(tx pgx.Tx) (err error) {
		out, err = collect[TenantCount](ctx, tx,
			"SELECT tenant_id, count(*) FROM concept GROUP BY tenant_id ORDER BY tenant_id")
		return err
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
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = collect[Hit](ctx, tx,
			"SELECT path, type, title, description, "+
				"       ts_rank_cd(search, to_tsquery('english', $1)) AS score "+
				"FROM concept "+
				"WHERE search @@ to_tsquery('english', $2) "+
				"  AND starts_with(path, coalesce($3::text, '')) "+
				"ORDER BY score DESC, path LIMIT $4",
			terms, terms, prefix, limit)
		return err
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
