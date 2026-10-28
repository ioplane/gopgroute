# Route ownership contract

T2 is an internal Linux library, not a deployable daemon. T3 must supervise a
dedicated speaker independently of the adapter before runtime admission.

## Authority

Configure two distinct, canonical IPv4 /32 service prefixes (write and read),
one unicast IPv4 next hop, a positive RPC timeout and health freshness bound.
IPv6, prefix aggregation and multiple next hops require a separate contract.
The operator exclusively reserves these prefixes for this adapter. Other API
writers must be excluded; GoBGP provides no compare-and-swap for AddPath.
Reject any occupied target prefix before adding, including remote paths. Paths
outside the two targets are never adopted, replaced or deleted.

Only a nonzero 16-byte UUID returned by a successful AddPath response becomes
deletion authority. Never infer ownership from a matching prefix, attributes,
or ListPath. GoBGP 4.10.0 does not populate UUID in ListPath responses, while a
DeletePath request without UUID or path can delete an entire address family.
Production deletion must always contain a validated UUID, and no path/family
fallback. [Pinned server source](https://github.com/osrg/gobgp/blob/da824d99912e02124b53741245ee4186c381adcc/pkg/server/grpc_server.go)
defines this boundary; native tests execute these behaviors.

## Journal and transitions

Use a pre-existing service-owned mode-0700 directory on a local filesystem
with working file/directory fsync and advisory locks. The directory and socket
parents must have trusted ancestors; the service account and root are trusted.
Files are single-link regular service-owned mode-0600 files. Reject symlinks,
foreign ownership, concurrent managers, truncated/noncanonical JSON, unknown
schema/configuration and invalid UUID/state combinations. Bound journal reads.
Hold an exclusive nonblocking flock for the entire manager lifetime.

For each prefix the durable states are empty, pending-add, owned(UUID), and
pending-delete(UUID). Write a create-only staging file, fsync it, rename over
the journal, then fsync the directory. Leftover staging files block startup.

1. Persist pending-add before the AddPath RPC. Re-evaluate T1 freshness after
   preflight and persistence, immediately before the RPC. Cap its deadline by
   both the RPC timeout and the remaining health lifetime.
2. Persist owned only after a successful response containing a valid UUID.
3. Persist pending-delete before deleting that exact UUID. Persist empty only
   after successful deletion. Withdrawals precede new announcements.
4. Any ambiguous mutation response or persistence failure permanently blocks
   further mutations by this manager and returns recovery/uncertain errors.
   Never retry AddPath or fall back to prefix/family deletion.
5. Startup rejects pending state. For owned state it attempts exact-UUID
   withdrawal before accepting new health. An unknown UUID after a speaker
   restart blocks recovery; it never authorizes adoption of another path.

A failed read-only preflight does not prove that owned paths disappeared.
The caller must keep reconciling or invoke the independent supervisor. A
successful AddPath whose health has expired is immediately withdrawn by UUID;
an ambiguous result remains unsafe until speaker shutdown is proven.

The invariant is conditional: for completed exact operations, route changes
are confined to issued UUIDs and unoccupied allowlisted prefixes, under the
single-writer reservation. A pending journal or RPC timeout is evidence of
uncertainty, not evidence of withdrawal. T3 must stop the dedicated speaker on
manager crash, stall or uncertain state and account for peer graceful restart.
Resetting a journal requires a separate proven-clean speaker incarnation;
there is deliberately no automatic reset or unsafe recovery switch in T2.
An active manager is valid only for one speaker incarnation. T3 must terminate
it when the supervised speaker exits, restarts or is replaced, and perform
startup reconciliation with a new manager. `Reconcile` returning nil means
that the requested library operations completed; it is not RIB attestation.
An unchanged owned intent does not issue another RPC. ListPath cannot prove
continued UUID identity even if the same prefix remains visible.

## Transport and verification

Use only a service-owned private Unix socket in a mode-0700 directory, with
mode 0600 on the socket. Disable gRPC application retries. Every RPC has a
deadline. Filesystem operations can stall beyond that deadline; only an
independent T3 supervisor can bound the complete failure response.

Test real GoBGP add/withdraw, foreign-prefix preservation, occupied-prefix
rejection, clean adapter restart, speaker restart with stale UUID, pending
startup refusal, lost AddPath replies, invalid UUIDs, stale health and journal
validation/locking. Fault injection models responses lost *after* native
mutation, not a fake RIB. Each completed track receives an independent review
against the implementation plan, ADR and this specification.

## Dependency advisory qualification

On 2026-10-28, the Go vulnerability database reports
[GO-2026-4736](https://pkg.go.dev/vuln/GO-2026-4736) against every GoBGP v4
version without recording a fixed version. The pinned v4.10.0 commit contains
the advisory's [fix 583080a7258e22cc884162e15b078771aa2c2c80](https://github.com/osrg/gobgp/commit/583080a7258e22cc884162e15b078771aa2c2c80).
Its unchanged upstream `TestRecvMessageWithError_MalformedNextHop` passes with
race detection for both treat-as-withdraw and session-reset cases. This is
source and regression evidence for this finding, not a general security or
deployed-speaker qualification. Keep the scanner enabled without suppression;
refresh the database and exact-source evidence when changing the dependency.
The adapter does not import the GoBGP server; native tests do. No BGP peer is
configured in those tests. The x/net HTML advisories are resolved by pinning
v0.62.0, rather than by relying on their currently unreachable code paths.
