# Документация GoPGRoute

GoPGRoute — отдельный адаптер Patroni → GoBGP для маршрутов PostgreSQL.
Клиентский путь: VIP → локальный PgBouncer → PostgreSQL.

**Статус:** реализованы внутренние проверки Patroni, политика допуска и
клиент GoBGP с журналом UUID. Демон и SQL-проба пулера ещё не предоставляются;
запуск требует независимого супервизора T3. Проверки не подтверждают HA.

- [Архитектура и критерии приёмки](../en/01-architecture.md)
- [Контракт здоровья](02-health-contract.md)
- [ADR 0001](adr/0001-bounded-health-intent.md)
- [Контракт владения маршрутами](03-route-ownership.md)
- [ADR 0002](adr/0002-durable-route-ownership.md)
- [Треки реализации](implementation-plan.md)
- [Правила именования](../standards/naming.md)
- [Участие в проекте](../../CONTRIBUTING.md)

Patroni, DCS и watchdog отвечают за promotion и fencing. Адаптер управляет
только собственными маршрутами. При неизвестном или устаревшем состоянии
требуется отзыв маршрута; отказ адаптера при живом GoBGP проверяется отдельно.
