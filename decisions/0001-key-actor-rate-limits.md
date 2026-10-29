# 0001. Key rate limits by authenticated actor

**Status:** Accepted
**Date:** 2026-10-29
**Decision-makers:** project user
**Related:** `docs/plans/2026-10-29-actor-rate-limit-design.md`

## Context

One shared bucket lets one caller exhaust other actors' allowances. Removing
the configured 64-actor maximum also makes linear credential scans unsuitable.

## Decision

Use a concurrent actor-ID-to-bucket map with lazy creation and one mutex per
bucket. Authenticate first through a snapshot map keyed by SHA-256 credential
digests. Invalid credentials share one anonymous bucket. Retain startup-only
actor configuration without an application count cap.

Preallocating buckets wastes state for unused actors; eviction can erase live
debt. IP keys merge unrelated actors on this loopback service. Distributed
storage adds infrastructure outside the requested process-local behavior.

## Consequences

Actors have independent admission budgets; credentials for the same actor share
one. State scales with configured credentials and active actors. Map lookup has
no constant-time guarantee. Aggregate throughput increases with actor count;
shared CPU/storage and multiple processes remain outside this isolation.
Reverting requires no data migration.
