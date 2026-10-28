# Health and intent contract

Status: accepted for track T1, 2026-10-28. This contract implements the health
boundary in [the architecture](01-architecture.md); it does not authorize route
writes or qualify a daemon.

## Observations

Each observation contains a three-valued state (unknown, unhealthy, healthy)
and the request-start timestamp. Unknown is the zero value. Production probes
use Go monotonic timestamps; observations are not persisted or restored.
A healthy observation is fresh only when its timestamp is nonzero, not in the
future, and its age is strictly less than a positive maximum age. Expiry is
inclusive: at the maximum age the observation no longer authorizes anything.

Let P, R and B mean fresh healthy primary, replica and pooler observations.
The intent is W = P AND B AND NOT R; Q = R AND B AND NOT P. Thus contradictory
positive role observations withdraw both intents. Unknown, unhealthy or stale
pooler health withdraws both. A fresh positive role may authorize its own
intent while the other role is unknown. This is a bounded observation policy,
not a proof of exclusive database write authority.

## Patroni probe

The supported REST contract is Patroni 4.x; the target version is 4.1.5.
Legacy `role=master` responses from Patroni 3.x are unhealthy. This probe
validates response semantics, not installed-version attestation; deployment
must admit the pinned Patroni version separately.

- Only `/primary` and `/replica?lag=<unsigned-byte-count>` are selectable.
  `/leader` is never substituted for primary health.
- HTTP 200 is necessary. The body must be a UTF-8 JSON object, state `running`
  and the matching role. Replica `tags.noloadbalance=true` denies eligibility.
- The entire request, including body read, has a positive finite timeout.
  Response headers and body are independently bounded to 64 KiB.
- Reject malformed JSON, trailing JSON, duplicate names in the root or tags
  objects, wrong types for consumed fields, and null or non-object tags.
  Unknown fields remain compatible with newer Patroni responses.
- Redirects are not followed. Userinfo, query, fragment, base path and opaque
  URLs are rejected. HTTP requires a literal loopback address; other endpoints
  require HTTPS with CA and hostname verification and TLS 1.3 or newer.
- Environment proxies are not used. Errors never include response bodies.
  HTTP failures produce an unhealthy observation; transport or parsing failures
  produce unknown state with an error. Neither can authorize an intent.

The lag decision is delegated to Patroni's endpoint, which uses the DCS leader
WAL position; it is not an independently measured replay-lag guarantee.
The pooler input is reserved for a separately tested SQL probe through the
local PgBouncer. An open TCP port does not establish healthy pooler service.

## Track acceptance

T1 requires real HTTP/TLS-server tests, cancellation and bounded-body tests,
malformed-response cases, and an exhaustive three-state intent matrix plus
freshness boundaries. Race tests, vet, format, lint, vulnerability checks and
independent plan/ADR/spec review are required. There is no GoBGP client,
route mutation, production workload or infrastructure change in T1.

## Sources

- [Patroni REST API](https://patroni.readthedocs.io/en/latest/rest_api.html)
- [Go HTTP client](https://pkg.go.dev/net/http#Client)
- [Go monotonic clocks](https://pkg.go.dev/time#hdr-Monotonic_Clocks)
- [RFC 8259, objects](https://www.rfc-editor.org/rfc/rfc8259.html#section-4)
- [ADR 0001](adr/0001-bounded-health-intent.md)

- [Patroni 4.0 role rename](https://patroni.readthedocs.io/en/latest/releases.html)
