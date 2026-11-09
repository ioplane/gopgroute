# Supervised daemon candidate

Status: T3.3 source candidate; no runtime artifact or production admission.

The Linux entry point is `gopgroute -config /absolute/private/config.json`.
Start from the [example](../../configs/gopgroute.example.json), replacing the
zero digest with the approved upstream `gobgpd` artifact's SHA-256. The example
is not a deployment manifest. VIP binding, PgBouncer, Patroni and PostgreSQL
are provisioned separately. There is no privileged port listener, callback API,
automatic restart or journal-reset option in this candidate.

## Startup and trust

The configuration and raw password files must be service-owned, single-link
regular files with mode 0600. Symlinks, empty or oversized reads are rejected.
The password is the exact file contents; do not append a newline unless it is
part of the password. It never enters command arguments or configuration IPC.
Configuration is UTF-8 JSON, at most 64 KiB, with exact lowercase field names,
no unknown/duplicate names, null values, trailing data or replacement characters.
All endpoints, identities and durations are explicit; lag zero is valid but
`max_lag_bytes` must be present. Public examples use documentation addresses.

The pre-existing runtime and journal directories must be distinct, service-owned
mode 0700 directories with trusted ancestors. A nonblocking guardian lock and
an unoccupied `speaker.sock` are required. Existing socket entries are never
removed or adopted. Configuration is serialized into canonical, sealed memfd
snapshots for the children, including the expected guardian PID. Each child has
its own file offset. Internal `_speaker` and `_adapter` roles require these
snapshots; operators start only the guardian command above.

The speaker is a separately supplied root-owned ELF with no group/other write
permission, set-id bits or file capabilities. The guardian checks its digest
before starting children; the wrapper rechecks and executes the verified open
descriptor. Root and the service account remain trusted. SHA-256 pins content;
it does not establish publisher identity, advisory applicability or release
approval. The wrapper passes generated TOML through sealed stdin, an empty
environment, a private Unix API address and disabled profiling/metrics HTTP.
Automatic configuration reload and graceful restart are not enabled.

Linux parent-death signals belong to the creating thread. Before exec the
wrapper locks its current thread, rearms SIGKILL on that thread and checks its
parent against the sealed guardian PID. Set-id/capability-bearing executables
are refused because exec can clear this signal. The guardian's creating
thread remains locked separately. See [Linux PR_SET_PDEATHSIG](https://man7.org/linux/man-pages/man2/PR_SET_PDEATHSIG.2const.html),
[Go Exec](https://github.com/golang/go/blob/go1.27.3/src/syscall/exec_unix.go)
and [sealed memfd](https://man7.org/linux/man-pages/man2/memfd_create.2.html).

## Speaker and cycle admission

The generated GoBGP configuration admits only the two local service /32 prefixes
on import and export, rejecting routes learned from peers. Global import policy
also applies to local AddPath; default reject alone would filter those paths.
It configures one to sixteen distinct
IPv4 external peers and IPv4 unicast only. The adapter admits the private socket
and reads back the ASN/router ID, policy assignments, export statement, prefix
set and peer configuration before creating its route manager. Readiness retries
are read-only and bounded by startup timeout. Other API writers are excluded
for the whole incarnation. API readiness does not attest the peer RIB or FIB.

Each serial cycle probes primary and replica Patroni endpoints, selects the
eligible role and obtains a new SQL observation for that same role. Conflicting
positive roles, absent eligibility, role mismatch, stale or failed SQL cannot
authorize an advertisement. Unknown health is reconciled into withdrawals;
a completed withdrawal cycle may report progress. Probe errors use fixed log
categories. Route/journal errors terminate the adapter and therefore stop the
speaker. Progress is emitted only after the complete cycle succeeds.

For positive durations, startup + cycle and interval + cycle must each be less
than heartbeat timeout. Probe timeout is at most cycle/3; RPC timeout is at most
cycle; max age is at least cycle. Subtractions enforce these constraints without
duration-addition overflow. The supervisor can stop a callback that ignores its
context. Kernel stalls, a stalled guardian, network detection and peer stale
retention remain outside a userspace deadline guarantee. Budget PgBouncer
connections for the SQL probe's bounded background closes separately.

## Model, checks and remaining gates

The ownership graph is a guardian with two direct children; neither child may
daemonize. Each completed cycle precedes its progress write, which precedes
lease renewal. The [Go memory model](https://go.dev/ref/mem) and
[channel specification](https://go.dev/ref/spec#Send_statements) define the
in-process ordering. Native subprocess and pidfd tests check the OS boundaries.
The pure role-binding matrix covers `3^3 * 3 * 3 = 243` combinations of health,
requested SQL role and SQL age; this is an exhaustive finite policy check,
not a database HA proof. The two /32 singleton sets are disjoint; no prefix
trie or general graph library is required for this fixed topology.

Binary hashing uses `crypto/sha256`. The pinned Go implementation supplies
AVX2/SHA-NI dispatch on supported amd64 CPUs
([source](https://github.com/golang/go/blob/go1.27.3/src/crypto/internal/fips140/sha256/sha256block_amd64.go));
no custom SIMD or cryptographic primitive is introduced. CPU-path selection and
throughput are not measured. Existing SCRAM verification remains in pgx;
[RFC 7677](https://www.rfc-editor.org/rfc/rfc7677.html) is its mechanism reference.

Required checks include race/shuffle, configuration fuzzing, native GoBGP
configuration, guardian death across exec, and the isolated PostgreSQL/PgBouncer
RIB lifecycle under `make sql-native`. The latter proves local announcement,
role-mismatch withdrawal, recovery and SQL-loss withdrawal, not BGP convergence.
The test exec target uses pinned upstream native configuration/server code;
the exact delivered `gobgpd` artifact still needs separate qualification.

No BGP peer or client transaction is qualified by these RIB tests. Apply
[RFC 8212](https://www.rfc-editor.org/rfc/rfc8212.html) policy requirements and
measure [RFC 4724](https://www.rfc-editor.org/rfc/rfc4724.html#section-4.2)
stale retention on actual peers. Clean restart/recovery of occupied sockets or
old UUID journals requires separate evidence and an explicit operator procedure.
The candidate never resets them automatically. Patroni/DCS/watchdog fencing,
backup/restore, three-site quorum and client outcomes remain T3/T4 gates.
