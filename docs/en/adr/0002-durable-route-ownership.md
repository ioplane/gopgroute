# ADR 0002: durable UUID authority and explicit uncertainty

Date: 2026-10-28. Status: accepted for T2.

## Context

The native GoBGP 4.10.0 API omits UUIDs from ListPath and allows family-wide
deletion with an empty UUID. An RPC deadline does not roll back a mutation.
Prefix coincidence therefore cannot reconcile a lost AddPath response safely.

## Decision

Implement [the ownership contract](../03-route-ownership.md): two reserved
IPv4 /32 prefixes, private Unix RPC, serialized durable pending/owned states,
validated UUID-only deletion, and refusal of ambiguous automatic recovery.
Evaluate T1 observations at the final actuation boundary. Retain a single
manager lock and withdraw known owned UUIDs at startup before accepting intent.
Bind each manager lifetime to a single supervised speaker incarnation. T3 must
terminate/recreate the manager on speaker restart; a nil reconciliation result
does not attest the RIB or recover a changed speaker.

## Consequences

An unknown UUID, pending journal, failed persistence or ambiguous write can
block availability until a separately verified speaker reset. This deliberate
false negative prevents replacement/deletion of a foreign path. Exclusive API
ownership of target prefixes is a deployment requirement, not an API guarantee.
IPv6 and shared-speaker recovery are outside this contract. T2 cannot bound
kernel/filesystem stalls or adapter crashes: a runnable daemon remains blocked
on the independently supervised failure mechanism in T3. No fencing guarantee
or client failover bound follows from successful RPC/unit tests.

## Verification

Use native GoBGP over Unix RPC, durable restart tests and post-mutation lost
response injection. Compare code and evidence independently with the plan,
this ADR and the ownership specification before accepting the track.
