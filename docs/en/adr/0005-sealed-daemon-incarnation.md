# ADR 0005: Sealed daemon incarnation

Status: accepted for the internal T3.3 candidate; runtime admission pending.

## Context

Mutable settings can give children different identities; a role change can
invalidate SQL evidence. Go goroutines can migrate between Linux threads
before exec, losing the original thread's parent-death signal.

## Decision

The guardian admits private files and one configuration, then gives each child
a sealed snapshot with the guardian PID. The wrapper pins a separate root-owned
ELF by SHA-256 and executes its verified descriptor with sealed GoBGP config.
Set-id and file capabilities are forbidden. The current exec thread is locked,
rearmed with SIGKILL and checked against the expected parent before exec.

The production adapter remains a GoBGP client; the upstream speaker artifact
and advisory qualification are separate. Scanner findings are not suppressed.
Read-only API admission verifies identity and default-reject policies before
opening the route manager. SQL evidence is bound to the selected Patroni role;
the complete cycle must succeed before heartbeat.

## Consequences

There is no shell, default TCP management listener, automatic journal reset or
internal respawn loop. Legacy occupied sockets and flat UUID journals fail
closed. [ADR 0006](0006-inherited-incarnation-leases.md) adds inherited leases
and fresh generation directories for whole-guardian restart. Native config,
process and SQL/RIB tests do not qualify peer forwarding, fencing or three-site
HA. The [daemon contract](../06-daemon-contract.md) defines the boundaries;
the [implementation tracks](../implementation-plan.md) retain later gates.
