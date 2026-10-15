package services

import (
	"context"
	"log/slog"
	"time"

	"github.com/roee-fs/keepsake/internal/repositories"
)

// Retention is how long prune keeps reads and revisions. RevisionDays 0 keeps every revision.
type Retention struct {
	ReadDays     int
	RevisionDays int
	// RevisionKeep is how many of each path's newest revisions survive any age.
	RevisionKeep int
}

// Prune applies r to every tenant, one tenant at a time, and returns how many reads and revisions it deleted.
func Prune(ctx context.Context, cs *repositories.ConceptRepository, r Retention, now time.Time) (reads, revisions int, err error) {
	tenants, err := cs.LogTenants(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, t := range tenants {
		nr, err := cs.PruneReads(ctx, t, now.AddDate(0, 0, -r.ReadDays))
		reads += nr
		if err != nil {
			return reads, revisions, err
		}
		nv := 0
		if r.RevisionDays > 0 {
			nv, err = cs.PruneRevisions(ctx, t, now.AddDate(0, 0, -r.RevisionDays), r.RevisionKeep)
			revisions += nv
			if err != nil {
				return reads, revisions, err
			}
		}
		if nr+nv > 0 {
			slog.Info("pruned", "tenant", t, "reads", nr, "revisions", nv)
		}
	}
	return reads, revisions, nil
}
