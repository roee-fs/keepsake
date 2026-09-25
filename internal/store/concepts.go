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

// updateSQL overwrites a concept and logs it, or returns no row on a stale version or missing path.
var updateSQL = func() string {
	n := len(fields)
	assignments := make([]string, n)
	for i, f := range fields {
		assignments[i] = fmt.Sprintf("%s=$%d", f, i+1)
	}
	return fmt.Sprintf("WITH w AS (UPDATE concept SET %s, updated_by=$%d, version=version+1, updated_at=now() "+
		"WHERE path=$%d AND ($%d::int IS NULL OR version=$%d) RETURNING version) "+revise,
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
func (cs *ConceptStore) Update(ctx context.Context, tenant uuid.UUID, c okf.Concept, actor string, expected *int) (version int, conflict *Conflict, err error) {
	w, err := newWrite(c)
	if err != nil {
		return 0, nil, err
	}
	err = cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		version, conflict, err = overwrite(ctx, tx, tx.QueryRow(ctx, updateSQL, w.updateArgs(tenant, actor, expected)...), c.Path)
		return err
	})
	return version, conflict, err
}

// ImportMany stores a bundle in one transaction, last write winning per path; err is a *NotFoundError if a path vanishes.
func (cs *ConceptStore) ImportMany(ctx context.Context, tenant uuid.UUID, bundle []okf.Concept, actor string) (int, error) {
	writes, err := newWrites(bundle)
	if err != nil {
		return 0, err
	}
	if err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error { return importWrites(ctx, tx, tenant, writes, actor) }); err != nil {
		return 0, err
	}
	return len(bundle), nil
}

// replaceSQL deletes the concepts under $1 that $2 omits, with their history, and counts them.
const replaceSQL = "WITH gone AS (DELETE FROM concept WHERE starts_with(path, $1) AND path <> ALL($2::text[]) RETURNING path), " +
	"history AS (DELETE FROM concept_revision WHERE path IN (SELECT path FROM gone)) " +
	"SELECT count(*) FROM gone"

// ReplacePrefix makes the concepts under prefix+"/" exactly bundle, in one transaction.
func (cs *ConceptStore) ReplacePrefix(ctx context.Context, tenant uuid.UUID, prefix string, bundle []okf.Concept, actor string) (deleted int, err error) {
	under := prefix + "/"
	keep := make([]string, len(bundle))
	for i, c := range bundle {
		if !strings.HasPrefix(c.Path, under) {
			return 0, fmt.Errorf("%s is not under %s", c.Path, under)
		}
		keep[i] = c.Path
	}
	writes, err := newWrites(bundle)
	if err != nil {
		return 0, err
	}
	err = cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		// Two replaces of one prefix would otherwise each keep the path the other inserted.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", tenant.String()+"/"+prefix); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, replaceSQL, under, keep).Scan(&deleted); err != nil {
			return err
		}
		return importWrites(ctx, tx, tenant, writes, actor)
	})
	return deleted, err
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

// importWrites inserts each write, or overwrites its path with no version check.
func importWrites(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, writes []write, actor string) error {
	var inserts, updates pgx.Batch
	for _, w := range writes {
		inserts.Queue(insertSQL, w.insertArgs(tenant, actor)...).QueryRow(func(row pgx.Row) error {
			_, created, err := scanInsert(row)
			if err == nil && !created {
				// The bundle is the operator's authority: there is no version to compare.
				updates.Queue(updateSQL, w.updateArgs(tenant, actor, nil)...).QueryRow(func(row pgx.Row) error {
					// No expected version, so no row means the path is gone; the open batch holds the connection.
					if err := row.Scan(new(int)); !errors.Is(err, pgx.ErrNoRows) {
						return err
					}
					return &NotFoundError{w.c.Path}
				})
			}
			return err
		})
	}
	if err := tx.SendBatch(ctx, &inserts).Close(); err != nil {
		return err
	}
	return tx.SendBatch(ctx, &updates).Close()
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

// overwrite scans updateSQL's row, telling a stale version from a missing path when there is none.
func overwrite(ctx context.Context, tx pgx.Tx, row pgx.Row, path string) (version int, conflict *Conflict, err error) {
	err = row.Scan(&version)
	if !errors.Is(err, pgx.ErrNoRows) {
		return version, nil, err
	}
	var current Conflict
	err = tx.QueryRow(ctx, "SELECT version, body FROM concept WHERE path=$1", path).Scan(&current.CurrentVersion, &current.CurrentBody)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, &NotFoundError{path}
	}
	if err != nil {
		return 0, nil, err
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
