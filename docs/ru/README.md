# Документация GoPGRoute

GoPGRoute — отдельный адаптер Patroni → GoBGP для маршрутов PostgreSQL.
Клиентский путь: VIP → локальный PgBouncer → PostgreSQL.

**Статус:** реализованы внутренние проверки Patroni и политика допуска.
Демон, клиент GoBGP и SQL-проба пулера ещё не предоставляются; проверки
репозитория не подтверждают HA.

- [Архитектура и критерии приёмки](../en/01-architecture.md)
- [Контракт здоровья](02-health-contract.md)
- [ADR 0001](adr/0001-bounded-health-intent.md)
- [Треки реализации](implementation-plan.md)
- [Правила именования](../standards/naming.md)
- [Участие в проекте](../../CONTRIBUTING.md)

Patroni, DCS и watchdog отвечают за promotion и fencing. Адаптер управляет
только собственными маршрутами. При неизвестном или устаревшем состоянии
требуется отзыв маршрута; отказ адаптера при живом GoBGP проверяется отдельно.
