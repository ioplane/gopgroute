# GoPGRoute repository contract

Read README.md and docs/en/01-architecture.md before changes.

- Conversation and pull request descriptions: technical Russian.
- Documentation: English under docs/en; Russian under docs/ru.
- Keep infrastructure addresses, inventory, credentials and private evidence
  outside this public repository. Examples use documentation address ranges.
- T1 and T2 implement internal health and journaled route-ownership libraries.
  Do not claim a runnable daemon, operational acceptance or released artifacts.
  T3 must supervise a dedicated speaker independently before runtime admission.
- Read docs/en/03-route-ownership.md and ADR 0002 before ownership changes.
  Never retry an uncertain mutation or delete without a validated owned UUID.
- Go code belongs in cmd and internal; tool dependencies belong in tools.
  Add public APIs only when an external consumer requires them.
- Resolve compiler, module, action and OCI versions from primary sources;
  pin exact versions and digests. Go commands run in pinned Podman images.
- Preserve validation and error handling. Use structured log/slog, bounded
  context lifetimes and errors wrapped with context. Never use unsafe.
- Require Go race tests, vet, format, lint, vulnerability checks and relevant
  integration checks before accepting runtime changes.
- Protect route ownership, fail-closed health, crash withdrawal, startup
  reconciliation and fencing boundaries with executable tests.
- Significant changes use pull requests. Keep default master history and
  source branches; never force-push, reuse tags or silently delete resources.
- Never add automated-author attribution to commits, requests or files.
- Track this project in the existing Beads workflow; do not add TODO ledgers.
