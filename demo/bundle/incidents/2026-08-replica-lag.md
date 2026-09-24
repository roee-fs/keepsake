---
type: Incident
title: Replica lag
description: A replica fell twenty minutes behind during a bulk backfill.
tags: [postgres, sev3]
---
A backfill on [Postgres](/services/postgres.md) saturated WAL shipping. Had the
primary failed, the [failover runbook](/runbooks/db-failover.md) would have
promoted a replica twenty minutes stale and broken the [RPO](/glossary/rpo.md).
See [replica lag](/glossary/replica-lag.md).
