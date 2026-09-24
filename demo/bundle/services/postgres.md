---
type: Service
title: Postgres
description: The primary relational store, one primary and two streaming replicas.
tags: [datastore, tier-0]
owner: data-platform
---
Owned by [data platform](/teams/data-platform.md).

One primary and two streaming replicas in separate zones. Failover is manual and
follows the [database failover runbook](/runbooks/db-failover.md). Backups are
continuous WAL archiving plus a nightly base backup; see
[restore from backup](/runbooks/restore-from-backup.md).

Watch [replica lag](/glossary/replica-lag.md). The target [RPO](/glossary/rpo.md)
is one minute and the target [RTO](/glossary/rto.md) is fifteen.
