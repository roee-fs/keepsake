---
type: Service
title: Search indexer
description: Streams row changes from Postgres into the search index.
tags: [datastore]
owner: data-platform
---
Owned by [data platform](/teams/data-platform.md). Reads logical replication from
[Postgres](/services/postgres.md). After a failover it must be pointed at the new
primary, which the [failover runbook](/runbooks/db-failover.md) covers.
