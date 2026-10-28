# GoPGRoute documentation

Status: internal health and route-ownership libraries; no runtime release exists.

| Document | Purpose |
|---|---|
| [01 Architecture](01-architecture.md) | Health, ownership and failover boundaries |
| [02 Health contract](02-health-contract.md) | Bounded probes and fail-closed intent |
| [ADR 0001](adr/0001-bounded-health-intent.md) | Health before actuation |
| [03 Route ownership](03-route-ownership.md) | UUID authority and durable uncertainty |
| [ADR 0002](adr/0002-durable-route-ownership.md) | Recovery before supervised runtime |
| [Implementation tracks](implementation-plan.md) | Dependencies and exit evidence |
| [Naming](../standards/naming.md) | Stable project identifiers |

Architecture, configuration, deployment, monitoring and interoperability
references use the same numbered-document approach as GoBFD as their
implementations become available.
