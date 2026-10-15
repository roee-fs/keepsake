package repositories

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// pruneBatch bounds each delete's transaction, so a first prune of a large table never holds locks for long.
var pruneBatch = 10000

// LogTenants lists every tenant with revisions or reads. Both outlive a tenant's concepts.
func (cs *ConceptRepository) LogTenants(ctx context.Context) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := cs.s.AdminScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			"SELECT tenant_id FROM concept_revision UNION SELECT tenant_id FROM concept_read ORDER BY 1")
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return out, err
}

// PruneReads deletes tenant's reads older than before and returns how many.
func (cs *ConceptRepository) PruneReads(ctx context.Context, tenant uuid.UUID, before time.Time) (int, error) {
	return cs.prune(ctx, tenant,
		"DELETE FROM concept_read WHERE ctid = ANY (ARRAY("+
			"SELECT ctid FROM concept_read WHERE created_at < $1 LIMIT $2))", before)
}

// PruneRevisions deletes tenant's revisions older than before, except each path's newest keep, and returns how many.
// A path with keep or fewer revisions has no version at that offset, so the comparison is NULL and keeps them all.
func (cs *ConceptRepository) PruneRevisions(ctx context.Context, tenant uuid.UUID, before time.Time, keep int) (int, error) {
	return cs.prune(ctx, tenant,
		"DELETE FROM concept_revision WHERE id = ANY (ARRAY("+
			"SELECT r.id FROM concept_revision r WHERE r.created_at < $1 AND r.version < ("+
			"  SELECT n.version FROM concept_revision n WHERE n.path = r.path"+
			"  ORDER BY n.version DESC OFFSET $3 - 1 LIMIT 1)"+
			" LIMIT $2))", before, keep)
}

// prune runs sql, a batched DELETE taking before, the batch size and args, until a batch comes back short.
func (cs *ConceptRepository) prune(ctx context.Context, tenant uuid.UUID, sql string, before time.Time, args ...any) (int, error) {
	total := 0
	for {
		var n int64
		err := cs.s.Scope(ctx, tenant, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, sql, append([]any{before, pruneBatch}, args...)...)
			n = tag.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += int(n)
		if n < int64(pruneBatch) {
			return total, nil
		}
	}
}
