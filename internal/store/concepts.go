// Concept writes, ported from 8f2af2e:src/keepsake/store/concepts.py.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/roee-fs/keepsake/okf"
)

// ErrNotFound reports that a path has no concept in the tenant's scope.
var ErrNotFound = errors.New("not found")

// NotFoundError is ErrNotFound for one path.
type NotFoundError struct{ Path string }

func (e *NotFoundError) Error() string        { return ErrNotFound.Error() + ": " + e.Path }
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// fields is every column a write sets, in Concept's order; every write statement derives from it.
var fields = []string{"type", "title", "description", "body", "frontmatter", "links"}

// revise appends the revision of w's row, completing the snapshot with its version.
const revise = "INSERT INTO concept_revision (tenant_id, path, version, op, snapshot, updated_by) " +
	"SELECT $%d, $%d, version, '%s', $%d::jsonb || jsonb_build_object('version', version), $%d FROM w RETURNING version"

// insertSQL creates a concept and its first revision, or returns no row if the path is taken.
var insertSQL = func() string {
	n := len(fields)
	return fmt.Sprintf("WITH w AS (INSERT INTO concept (tenant_id, path, %s, updated_by) VALUES (%s) "+
		"ON CONFLICT (tenant_id, path) DO NOTHING RETURNING version) "+revise,
		strings.Join(fields, ", "), placeholders(n+3), 1, 2, "create", n+4, n+3)
}()

// sameContent is true when the row holds the fields bound from $off+1, ignoring the authorship stamp.
func sameContent(off int) string {
	cols := make([]string, len(fields))
	vals := make([]string, len(fields))
	for i, f := range fields {
		cols[i], vals[i] = f, fmt.Sprintf("$%d", off+i+1)
		if f == "frontmatter" {
			cols[i] += " - 'generated'"
			vals[i] += "::jsonb - 'generated'"
		}
	}
	return fmt.Sprintf("(%s) IS NOT DISTINCT FROM (%s)", strings.Join(cols, ", "), strings.Join(vals, ", "))
}

// updateSQL overwrites a concept and logs it, or returns no row on a stale version, a missing path or no change.
var updateSQL = func() string {
	n := len(fields)
	assignments := make([]string, n)
	for i, f := range fields {
		assignments[i] = fmt.Sprintf("%s=$%d", f, i+1)
	}
	return fmt.Sprintf("WITH w AS (UPDATE concept SET %s, updated_by=$%d, version=nextval('version_seq'), updated_at=now() "+
		"WHERE path=$%d AND ($%d::bigint IS NULL OR version=$%d) AND NOT "+sameContent(0)+" RETURNING version) "+revise,
		strings.Join(assignments, ", "), n+1, n+2, n+3, n+3, n+4, n+2, "update", n+5, n+1)
}()

// Conflict is a stale expected version. CurrentBody is the text to merge against.
type Conflict struct {
	CurrentVersion int
	CurrentBody    string
}

// ConceptStore writes concepts and appends their revision log.
type ConceptStore struct {
	s *Store
}

func NewConceptStore(s *Store) *ConceptStore { return &ConceptStore{s: s} }

// PoolSize is the most connections the store holds at once.
func (cs *ConceptStore) PoolSize() int { return int(cs.s.pool.Config().MaxConns) }

// Create inserts a concept. created is false when the path is already taken.
func (cs *ConceptStore) Create(ctx context.Context, tenant uuid.UUID, c okf.Concept, actor string) (version int, created bool, err error) {
	w, err := newWrite(c)
	if err != nil {
		return 0, false, err
	}
	err = cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		version, created, err = scanInsert(tx.QueryRow(ctx, insertSQL, w.insertArgs(tenant, actor)...))
		return err
	})
	return version, created, err
}

// Update writes a concept, as a compare-and-swap unless expected is nil. err is a *NotFoundError for a missing path.
// A concept that differs only in its authorship stamp writes nothing and returns the current version.
func (cs *ConceptStore) Update(ctx context.Context, tenant uuid.UUID, c okf.Concept, actor string, expected *int) (version int, conflict *Conflict, err error) {
	w, err := newWrite(c)
	if err != nil {
		return 0, nil, err
	}
	err = cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		version, conflict, err = overwrite(ctx, tx, tx.QueryRow(ctx, updateSQL, w.updateArgs(tenant, actor, expected)...), w)
		return err
	})
	return version, conflict, err
}

// ImportMany stores a bundle in one transaction, last write winning per path. written counts the concepts that changed.
func (cs *ConceptStore) ImportMany(ctx context.Context, tenant uuid.UUID, bundle []okf.Concept, actor string) (written int, err error) {
	writes, err := newWrites(bundle)
	if err != nil {
		return 0, err
	}
	err = cs.s.Scope(ctx, tenant, func(tx pgx.Tx) (err error) {
		written, err = importWrites(ctx, tx, tenant, writes, actor)
		return err
	})
	return written, err
}

// replaceSQL deletes the concepts under $1 that $2 omits and logs each as a delete revision with a new version.
// NOT EXISTS keeps a generic plan from scanning $2 per row. nextval sits in a select list, so it runs
// once per deleted row; an uncorrelated LATERAL may run once for the whole statement.
const replaceSQL = `
WITH d AS (
  DELETE FROM concept WHERE starts_with(path, $1) AND NOT EXISTS (SELECT 1 FROM unnest($2::text[]) k WHERE k = path)
  RETURNING tenant_id, path, type, title, description, body, frontmatter, links
)
INSERT INTO concept_revision (tenant_id, path, version, op, snapshot, updated_by)
SELECT tenant_id, path, v, 'delete', jsonb_build_object('path', path, 'type', type, 'title', title,
  'description', description, 'body', body, 'frontmatter', frontmatter, 'links', to_jsonb(links), 'version', v), $3
FROM (SELECT *, nextval('version_seq') AS v FROM d) d`

// storedComputationsSQL reads what a replace could change of an Attested Computation:
// those under $1, and the concepts at the paths in $3, where the bundle holds one.
// FOR UPDATE holds off a tool write between the guard's read and the replace.
var storedComputationsSQL = "SELECT " + readCols + " FROM concept WHERE starts_with(path, $1) AND (type = $2 OR path = ANY($3::text[])) FOR UPDATE"

// ReplacePrefix makes the concepts under prefix+"/" exactly bundle, in one transaction.
// A non-nil guard sees the stored Attested Computations the replace could change, by path, and may refuse it.
func (cs *ConceptStore) ReplacePrefix(ctx context.Context, tenant uuid.UUID, prefix string, bundle []okf.Concept, actor string,
	guard func(stored map[string]okf.Concept) error) (written, deleted int, err error) {
	under := prefix + "/"
	keep := make([]string, len(bundle))
	for i, c := range bundle {
		if !strings.HasPrefix(c.Path, under) {
			return 0, 0, fmt.Errorf("%s is not under %s", c.Path, under)
		}
		keep[i] = c.Path
	}
	writes, err := newWrites(bundle)
	if err != nil {
		return 0, 0, err
	}
	err = cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		// Every replace of a tenant serializes, so replaces of nested prefixes cannot interleave either.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", tenant.String()); err != nil {
			return err
		}
		if guard != nil {
			if err := guardComputations(ctx, tx, under, bundle, guard); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, replaceSQL, under, keep, actor)
		if err != nil {
			return err
		}
		deleted = int(tag.RowsAffected())
		written, err = importWrites(ctx, tx, tenant, writes, actor)
		return err
	})
	return written, deleted, err
}

func guardComputations(ctx context.Context, tx pgx.Tx, under string, bundle []okf.Concept, guard func(map[string]okf.Concept) error) error {
	var paths []string
	for _, c := range bundle {
		if c.Type == okf.AttestedComputation {
			paths = append(paths, c.Path)
		}
	}
	rows, err := tx.Query(ctx, storedComputationsSQL, under, okf.AttestedComputation, paths)
	if err != nil {
		return err
	}
	defer rows.Close()
	stored := map[string]okf.Concept{}
	for rows.Next() {
		c, err := scanConcept(rows)
		if err != nil {
			return err
		}
		stored[c.Path] = c
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return guard(stored)
}

func newWrites(bundle []okf.Concept) ([]write, error) {
	writes := make([]write, len(bundle))
	for i, c := range bundle {
		var err error
		if writes[i], err = newWrite(c); err != nil {
			return nil, err
		}
	}
	return writes, nil
}

// upsertSQL creates or overwrites a concept with no version check, since a bundle is the operator's
// authority. It writes nothing when the concept is unchanged, so a re-import adds no revisions.
// xmax is zero only on a row this statement inserted.
var upsertSQL = func() string {
	n := len(fields)
	set := make([]string, n)
	cols := make([]string, n)
	for i, f := range fields {
		set[i] = f + "=EXCLUDED." + f
		cols[i] = "concept." + f
	}
	return fmt.Sprintf("WITH w AS (INSERT INTO concept (tenant_id, path, %s, updated_by) VALUES (%s) "+
		"ON CONFLICT (tenant_id, path) DO UPDATE SET %s, updated_by=EXCLUDED.updated_by, version=EXCLUDED.version, updated_at=now() "+
		"WHERE (%s) IS DISTINCT FROM (%s) RETURNING version, xmax = 0 AS created) "+
		"INSERT INTO concept_revision (tenant_id, path, version, op, snapshot, updated_by) "+
		"SELECT $1, $2, version, CASE WHEN created THEN 'create' ELSE 'update' END, "+
		"$%d::jsonb || jsonb_build_object('version', version), $%d FROM w RETURNING version",
		strings.Join(fields, ", "), placeholders(n+3), strings.Join(set, ", "),
		strings.Join(cols, ", "), "EXCLUDED."+strings.Join(fields, ", EXCLUDED."), n+4, n+3)
}()

// importWrites creates or overwrites each write, and returns how many changed.
func importWrites(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, writes []write, actor string) (written int, err error) {
	var batch pgx.Batch
	for _, w := range writes {
		batch.Queue(upsertSQL, w.insertArgs(tenant, actor)...).QueryRow(func(row pgx.Row) error {
			err := row.Scan(new(int))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err == nil {
				written++
			}
			return err
		})
	}
	err = tx.SendBatch(ctx, &batch).Close()
	return written, err
}

// write is a concept with its jsonb parameters marshalled once.
type write struct {
	c        okf.Concept
	fm, snap []byte
}

func newWrite(c okf.Concept) (write, error) {
	// A nil Frontmatter is an unset field, so it is "{}" like Python's dataclass default.
	fm := []byte("{}")
	if c.Frontmatter != nil {
		var err error
		if fm, err = json.Marshal(c.Frontmatter); err != nil {
			return write{}, err
		}
	}
	// asdict(c) without its version, which the statement adds.
	snap := okf.NewMap()
	snap.Set("path", c.Path)
	snap.Set("type", c.Type)
	snap.Set("title", c.Title)
	snap.Set("description", c.Description)
	snap.Set("body", c.Body)
	snap.Set("frontmatter", json.RawMessage(fm))
	snap.Set("links", links(c.Links))
	b, err := json.Marshal(snap)
	return write{c, fm, b}, err
}

func (w write) insertArgs(tenant uuid.UUID, actor string) []any {
	c := w.c
	return []any{tenant, c.Path, c.Type, c.Title, c.Description, c.Body, w.fm, links(c.Links), actor, w.snap}
}

func (w write) updateArgs(tenant uuid.UUID, actor string, expected *int) []any {
	c := w.c
	return []any{c.Type, c.Title, c.Description, c.Body, w.fm, links(c.Links), actor, c.Path, expected, tenant, w.snap}
}

func scanInsert(row pgx.Row) (version int, created bool, err error) {
	err = row.Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return version, err == nil, err
}

// overwrite scans updateSQL's row. When there is none it tells a stale version from a missing path or no change.
func overwrite(ctx context.Context, tx pgx.Tx, row pgx.Row, w write) (version int, conflict *Conflict, err error) {
	err = row.Scan(&version)
	if !errors.Is(err, pgx.ErrNoRows) {
		return version, nil, err
	}
	c := w.c
	var current Conflict
	var same bool
	err = tx.QueryRow(ctx, "SELECT version, body, "+sameContent(1)+" FROM concept WHERE path=$1",
		c.Path, c.Type, c.Title, c.Description, c.Body, w.fm, links(c.Links)).Scan(&current.CurrentVersion, &current.CurrentBody, &same)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, &NotFoundError{c.Path}
	}
	if err != nil {
		return 0, nil, err
	}
	// Nothing to merge, so a stale expected version is no conflict either.
	if same {
		return current.CurrentVersion, nil, nil
	}
	return 0, &current, nil
}

// links normalizes a nil slice to empty so it binds as "{}", not NULL.
func links(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func placeholders(n int) string {
	ph := make([]string, n)
	for i := range ph {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	return strings.Join(ph, ",")
}
