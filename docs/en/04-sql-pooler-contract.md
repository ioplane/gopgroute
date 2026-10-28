# SQL pooler observation contract

Status: T3 SQL subtrack. This internal probe neither starts a daemon nor
completes T3 runtime acceptance. Supervision, peer graceful restart and
client-visible failure tests remain required by the implementation plan.

## Connection boundary

The probe targets an explicit literal-loopback TCP address and port of the
local PgBouncer. Its configured expected PostgreSQL backend must also be an
exact loopback address and port, different from the frontend endpoint. DNS, remote targets, Unix-backend NULL
identities, multi-host fallbacks and arbitrary DSNs are outside this contract.
Use the application database alias through the pooler, not its admin console.

Require a nonempty explicit database, user and password, positive timeout,
UTF-8 without NUL, and bounded credentials. Reject nonempty `PG*` environment
variables before parsing a fixed driver configuration. Do not inherit service,
passfile, options, proxy, host or TLS defaults. Connection secrets never enter
a DSN, error, log or command argument. Runtime credential-file admission
belongs to T3 supervision and deployment.

Use pinned pgx v5.11.0 with SCRAM-SHA-256 only. Plaintext/MD5/trust server
authentication requests cannot authorize a check. The first backend message
must be an `AuthenticationSASL` request; any other first message, including
`ReadyForQuery`, `ParameterStatus`, `BackendKeyData` or `ErrorResponse`, ends
the attempt before the driver handles it. A pre-authentication server error,
such as a missing database or exhausted connections, is therefore reported only
as the fixed connect category. The guard relies on pinned pgx verifying the
SCRAM server signature; re-qualify it on every driver upgrade. Transport is
local TCP without TLS; the local host and privileged processes are
trusted. Remote SQL or TLS support requires a separate trust contract, not an
insecure switch.
Use unnamed extended execution compatible with PgBouncer transaction pooling;
disable statement/description caches and cap incoming protocol message bodies
at 64 KiB. Open a new connection for each observation, without automatic retry.

## Observation

A single fixed SELECT reads `current_database`, `current_user`,
`inet_server_addr`, `inet_server_port`, `pg_is_in_recovery` and
`transaction_read_only`. Exactly one complete row must decode successfully.
Require exact database, user, backend address and port. Primary checks require
recovery=false and read-only=false; replica checks require both true. SQL
mismatch is unhealthy; transport, authentication, protocol, query, decoding or
close errors are unknown. No successful TCP handshake alone authorizes health.

One context deadline bounds connect, query, row consumption and close. Check
cancellation after close even when the driver returns nil. Retain
the request-start monotonic timestamp and the requested role in the result.
The later runtime must combine it only with Patroni observations for the same
role and re-evaluate freshness at T2's final actuation boundary. It must not
reuse a primary SQL result for a replica intent after a role change.

Returned errors contain only a fixed operation category and, where relevant,
the caller's context error. Do not wrap raw driver/server errors: those can
contain credentials, SQL text or server-controlled strings.

After a timeout or cancellation, pgx v5.11.0 closes the connection in the
background: it sends a cancel request and drains the socket for up to 15
seconds. The check returns at its deadline, but the runtime probe interval must
account for these bounded background closes.

## Limits and verification

This checks a read-only statement and session eligibility, not write I/O,
storage durability, all application-user pools, fencing or HA. The health role
needs CONNECT and the selected built-in reads; it needs no application DML or
superuser rights. A deployment must ensure that its pooler database alias
targets local PostgreSQL and qualify actual application transactions separately.

Require real PostgreSQL 18.6 and PgBouncer 1.26.1 SQL tests, exact identity and
read-only mismatches, server loss/timeouts, auth downgrade, malformed/oversized
protocol, environment refusal and error non-disclosure. Apply the existing
container quality gates and independent plan/ADR/spec review. The whole T3
remains incomplete until independently supervised crash/stall and peer/client
outcomes pass; a free-running watchdog goroutine cannot prove loop progress.

## Sources

- [PostgreSQL 18 session identity functions](https://www.postgresql.org/docs/18/functions-info.html)
- [PostgreSQL 18 recovery functions](https://www.postgresql.org/docs/18/functions-admin.html)
- [PostgreSQL host(inet) without netmask](https://www.postgresql.org/docs/18/functions-net.html)
- [PostgreSQL transaction read-only setting](https://www.postgresql.org/docs/18/runtime-config-client.html#GUC-TRANSACTION-READ-ONLY)
- [Pinned pgx PgBouncer contract](https://github.com/jackc/pgx/blob/v5.11.0/doc.go)
- [Pinned pgx connection configuration](https://github.com/jackc/pgx/blob/v5.11.0/pgconn/config.go)
- [PgBouncer 1.26.1 security fixes](https://github.com/pgbouncer/pgbouncer/releases/tag/pgbouncer_1_26_1)
