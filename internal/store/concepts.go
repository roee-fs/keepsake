// Concept writes. Two statements, never one upsert: an upsert guarded by a version
// predicate would silently create a row the caller believed it was updating.
//
// Tables are named unqualified: Store.Scope sets search_path to the validated
// schema plus pg_catalog and nothing else, so a literal prefix would only break a
// non-default schema.
//
// Ported from 8f2af2e:src/keepsake/store/concepts.py (the write half; reads are Task 9).
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

// fields is every column a write sets, in Concept's own field order. The INSERT
// column list, its placeholder run and the UPDATE SET clause are all derived from
// this, so a seventh field cannot reach one statement and not another.
var fields = []string{"type", "title", "description", "body", "frontmatter", "links"}

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
	err = cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		version, created, err = insert(ctx, tx, tenant, c, actor)
		return err
	})
	return version, created, err
}

// Update writes a concept. expected makes it a compare-and-swap; nil is
// last-write-wins. err wraps ErrNotFound if the path does not exist.
func (cs *ConceptStore) Update(ctx context.Context, tenant uuid.UUID, c okf.Concept, actor string, expected *int) (version int, conflict *Conflict, err error) {
	err = cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		version, conflict, err = overwrite(ctx, tx, tenant, c, actor, expected)
		return err
	})
	return version, conflict, err
}

// ImportMany stores a whole bundle in one transaction, last write winning per
// path. err wraps ErrNotFound if a path is deleted underneath the import.
func (cs *ConceptStore) ImportMany(ctx context.Context, tenant uuid.UUID, bundle []okf.Concept, actor string) (int, error) {
	err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		for _, c := range bundle {
			_, created, err := insert(ctx, tx, tenant, c, actor)
			if err != nil {
				return err
			}
			if !created {
				// The bundle is the authority the operator is replaying, so
				// there is no version to compare and no conflict to resolve.
				if _, _, err := overwrite(ctx, tx, tenant, c, actor, nil); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(bundle), nil
}

func insert(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, c okf.Concept, actor string) (version int, created bool, err error) {
	fm, err := frontmatterJSON(c.Frontmatter)
	if err != nil {
		return 0, false, err
	}
	columns := append(append([]string{"tenant_id", "path"}, fields...), "updated_by")
	query := fmt.Sprintf(
		"INSERT INTO concept (%s) VALUES (%s) ON CONFLICT (tenant_id, path) DO NOTHING RETURNING version",
		strings.Join(columns, ", "), placeholders(len(columns)))
	err = tx.QueryRow(ctx, query,
		tenant, c.Path, c.Type, c.Title, c.Description, c.Body, fm, links(c.Links), actor,
	).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if err := revise(ctx, tx, tenant, c, version, "create", actor); err != nil {
		return 0, false, err
	}
	return version, true, nil
}

func overwrite(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, c okf.Concept, actor string, expected *int) (version int, conflict *Conflict, err error) {
	fm, err := frontmatterJSON(c.Frontmatter)
	if err != nil {
		return 0, nil, err
	}
	assignments := make([]string, len(fields))
	for i, f := range fields {
		assignments[i] = fmt.Sprintf("%s=$%d", f, i+1)
	}
	n := len(fields)
	query := fmt.Sprintf(
		"UPDATE concept SET %s, updated_by=$%d, version=version+1, updated_at=now() "+
			"WHERE path=$%d AND ($%d::int IS NULL OR version=$%d) RETURNING version",
		strings.Join(assignments, ", "), n+1, n+2, n+3, n+3)
	err = tx.QueryRow(ctx, query,
		c.Type, c.Title, c.Description, c.Body, fm, links(c.Links), actor, c.Path, expected,
	).Scan(&version)
	if err == nil {
		if err := revise(ctx, tx, tenant, c, version, "update", actor); err != nil {
			return 0, nil, err
		}
		return version, nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, err
	}

	var currentVersion int
	var currentBody string
	err = tx.QueryRow(ctx, "SELECT version, body FROM concept WHERE path=$1", c.Path).Scan(&currentVersion, &currentBody)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, fmt.Errorf("%w: %s", ErrNotFound, c.Path)
	}
	if err != nil {
		return 0, nil, err
	}
	return 0, &Conflict{CurrentVersion: currentVersion, CurrentBody: currentBody}, nil
}

func revise(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, c okf.Concept, version int, op, actor string) error {
	snap, err := snapshotJSON(c, version)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		"INSERT INTO concept_revision (tenant_id, path, version, op, snapshot, updated_by) VALUES ($1,$2,$3,$4,$5,$6)",
		tenant, c.Path, version, op, snap, actor)
	return err
}

// snapshotJSON is asdict(c) | {"version": v}: path, type, title, description,
// body, frontmatter, links, version, Concept's own field order.
func snapshotJSON(c okf.Concept, version int) ([]byte, error) {
	snap := okf.NewMap()
	snap.Set("path", c.Path)
	snap.Set("type", c.Type)
	snap.Set("title", c.Title)
	snap.Set("description", c.Description)
	snap.Set("body", c.Body)
	fm := c.Frontmatter
	if fm == nil {
		fm = okf.NewMap()
	}
	snap.Set("frontmatter", fm)
	snap.Set("links", links(c.Links))
	snap.Set("version", version)
	return json.Marshal(snap)
}

// frontmatterJSON binds Frontmatter into a jsonb parameter. A nil Frontmatter is
// an unset field, not an absent one, so it is "{}" like Python's dataclass default.
func frontmatterJSON(fm *okf.Map) ([]byte, error) {
	if fm == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(fm)
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
