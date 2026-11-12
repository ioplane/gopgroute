# ADR 0005: Неизменяемая конфигурация incarnation

Статус: принято для внутреннего кандидата T3.3; runtime admission отсутствует.

## Контекст

Изменяемые настройки способны дать детям разные identity; смена роли
обесценивает SQL-пробу. Goroutine Go может перейти на другой Linux thread
перед exec и потерять parent-death signal первоначального потока.

## Решение

Guardian допускает private files и одну конфигурацию; дети получают sealed
snapshots с PID guardian. Wrapper проверяет SHA-256 отдельного root-owned ELF
и выполняет открытый descriptor с sealed GoBGP config. Set-id и file capabilities
запрещены. Exec thread закрепляется, повторно получает SIGKILL и сверяет parent PID.

Production adapter остаётся клиентом GoBGP; upstream speaker artifact и advisory
qualification принимаются отдельно. Findings сканера не подавляются. Read-only
API admission проверяет identity и default-reject policies до открытия route
manager. SQL-проба относится к выбранной роли Patroni; heartbeat разрешён только
после успешного завершения всего цикла.

## Последствия

Shell, стандартного TCP management listener, автоматических journal reset и
restart нет. Занятый socket и прежний UUID journal приводят к отказу допуска.
Native config/process/SQL/RIB tests не принимают peer forwarding, fencing или
HA трёх ЦОД. Границы задаёт [контракт](../06-daemon-contract.md), последующие
gates — [план](../implementation-plan.md).
