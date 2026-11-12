# ADR 0004: Supervised process progress

Status: accepted for the internal T3.2 components; production admission pending.

## Context

The route journal can block on filesystem I/O. An adapter can crash or freeze
while a dedicated GoBGP speaker continues advertising. A normal shutdown hook
and an independent heartbeat timer cannot establish bounded withdrawal.

## Decision

A separate Linux guardian owns the direct speaker and adapter processes.
An inherited private pipe carries progress only after a complete successful
reconciliation. Child exit, progress failure or timeout stops both owned
process handles, with bounded TERM/KILL waits. Parent-death signals bind the
children to the guardian's locked creating thread. Never restart children or
reset the route journal automatically.

## Consequences

The guardian controls one speaker incarnation and must not share it with other
services. Its own scheduling and kernel progress remain assumptions. Daemon
wiring, verified startup, fresh journal recovery, peer GR/client outcomes and
Patroni/DCS/watchdog fencing require separate acceptance. The precise contract
and tests are documented in [supervised progress](../05-supervised-progress.md).
