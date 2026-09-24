---
type: Runbook
title: Restore from backup
description: Rebuild Postgres from the base backup and WAL archive.
tags: [postgres, sev1]
owner: data-platform
---
The last resort after a failed [failover](/runbooks/db-failover.md). Restore the
latest base backup, replay WAL to the target time, then verify row counts on the
billing tables before reopening traffic. Expect to miss the
[RTO](/glossary/rto.md); the [RPO](/glossary/rpo.md) still holds.
