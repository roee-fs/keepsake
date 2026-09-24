---
type: Service
title: Billing worker
description: Runs invoicing and payment retries off the job queue.
tags: [payments]
owner: payments
---
Owned by [payments](/teams/payments.md). Pulls jobs from
[Valkey](/services/valkey.md) and writes to [Postgres](/services/postgres.md).

Scale it with [scale workers](/runbooks/scale-workers.md). It pauses itself
during a [database failover](/runbooks/db-failover.md).
