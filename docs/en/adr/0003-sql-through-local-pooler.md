# ADR 0003: observe SQL through the local pooler

Date: 2026-10-28. Status: accepted for the SQL subtrack of T3.

## Context

An open PgBouncer port can hide a broken database pool, wrong backend, failed
authentication or read-only primary. The T1 pooler input needs an actual SQL
observation. Driver defaults and server-controlled errors introduce additional
destination and credential-disclosure risks.

## Decision

Implement the [SQL observation contract](../04-sql-pooler-contract.md) using
pgx v5.11.0, explicit loopback addresses, SCRAM-only authentication, one fixed
bounded SELECT, exact backend/session identity and explicit role/read-only
requirements. Reject inherited PG configuration and hide raw driver errors.
Require the first backend message to be an `AuthenticationSASL` request so no
session can become ready without the driver's complete SCRAM exchange.
Retain request-start time and the requested role. Keep the library internal.

## Consequences

Fresh connections consume authentication work; measure the eventual probe
interval rather than assuming zero overhead. A monitor role configured
read-only on a primary cannot authorize the write service. Remote or Unix
backends cannot satisfy the exact loopback TCP identity contract. The query
does not write data or prove application write durability. This subtrack does
not provide a runnable daemon: T3 still requires an independent progress
watchdog, speaker-incarnation binding and measured peer/client failure tests.

## Verification

Use real PostgreSQL/PgBouncer for positive SQL behavior and role/identity
mismatches; use protocol fault fixtures for timeouts, downgrade and hostile
errors. Require container quality gates and independent plan/ADR/spec review.
