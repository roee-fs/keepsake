---
type: Term
title: Replica lag
description: How far a streaming replica trails the primary.
tags: [glossary, postgres]
---
Measured in seconds of WAL not yet replayed. Promoting a lagging replica loses
those seconds, so it bounds the [RPO](/glossary/rpo.md). See
[the August lag](/incidents/2026-08-replica-lag.md).
