---
type: Service
title: Auth service
description: Issues and validates sessions.
tags: [edge]
owner: edge
---
Owned by [edge](/teams/edge.md). Sessions live in [Valkey](/services/valkey.md)
and users in [Postgres](/services/postgres.md). A Valkey flush logs everyone out.
