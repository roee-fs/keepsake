---
type: Runbook
title: Database failover
description: Promote a Postgres replica when the primary is lost.
tags: [postgres, sev1]
owner: data-platform
---
Use when the [Postgres](/services/postgres.md) primary is unreachable for more
than two minutes. This is a [SEV1](/glossary/sev1.md).

1. Page the DBA on-call in `#dba-oncall` before touching anything.
2. Pick the replica with the least [replica lag](/glossary/replica-lag.md).
3. Promote it with `pg_ctl promote`.
4. Repoint the connection pooler at the new primary.
5. Restart the [search indexer](/services/search-indexer.md) against the new primary.
6. Confirm the [billing worker](/services/billing-worker.md) has resumed.

If no replica is healthy, follow [restore from backup](/runbooks/restore-from-backup.md).
