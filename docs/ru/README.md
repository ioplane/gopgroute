# Документация GoPGRoute

GoPGRoute — отдельный адаптер Patroni → GoBGP для маршрутов PostgreSQL.
Клиентский путь: VIP → локальный PgBouncer → PostgreSQL.

**Статус:** реализованы проверки Patroni/SQL, политика допуска, клиент GoBGP
с журналом UUID, supervisor и кандидат Linux-демона. Runtime artifact, отдельный
upstream speaker, peer behavior и restart recovery ещё не приняты.
Проверки не подтверждают HA.

- [Архитектура и критерии приёмки](../en/01-architecture.md)
- [Контракт здоровья](02-health-contract.md)
- [ADR 0001](adr/0001-bounded-health-intent.md)
- [Контракт владения маршрутами](03-route-ownership.md)
- [ADR 0002](adr/0002-durable-route-ownership.md)
- [SQL через локальный пулер](04-sql-pooler-contract.md)
- [Независимый supervisor](05-supervised-progress.md)
- [Контракт демона](06-daemon-contract.md)
- [ADR 0005](adr/0005-sealed-daemon-incarnation.md)
- [Треки реализации](implementation-plan.md)
- [Правила именования](../standards/naming.md)
- [Участие в проекте](../../CONTRIBUTING.md)

Patroni, DCS и watchdog отвечают за promotion и fencing. Адаптер управляет
только собственными маршрутами. При неизвестном или устаревшем состоянии
требуется отзыв маршрута; отказ адаптера при живом GoBGP проверяется отдельно.
