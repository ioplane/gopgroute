# Кандидат демона с независимым supervisor

Статус: исходный кандидат T3.3; runtime-артефакт и production admission отсутствуют.

Linux entry point: `gopgroute -config /absolute/private/config.json`.
В [примере](../../configs/gopgroute.example.json) нулевой digest заменяется
SHA-256 одобренного upstream `gobgpd`. Пример не является deployment manifest.
VIP binding, PgBouncer, Patroni и PostgreSQL предоставляются отдельно.
Привилегированного слушателя, callback API, автоматического restart и reset
журнала в кандидате нет.

## Допуск и доверие

Config и файл пароля — service-owned regular files, mode 0600, один hard link.
Symlink, пустой файл и превышение размера отвергаются. Пароль — точные байты
файла; перевод строки не добавляется, если он не входит в пароль. Пароль не
попадает в argv или IPC настроек. Конфигурация — UTF-8 JSON до 64 KiB:
точные lowercase имена, без unknown/duplicate keys, null, trailing data и
символов замены. Endpoints, identity и durations задаются явно; lag=0 допустим,
но поле `max_lag_bytes` обязательно. В примерах используются документационные IP.

Runtime и journal directories существуют заранее, различаются, принадлежат
сервису и имеют mode 0700 и доверенных предков. Требуются nonblocking lock
guardian и свободный `speaker.sock`; существующий socket не удаляется и не
принимается под управление. Дети получают отдельные canonical sealed memfd
snapshots с PID guardian и независимыми offsets. `_speaker` и `_adapter` —
внутренние роли; оператор запускает только guardian командой выше.

Speaker предоставляется отдельным root-owned ELF: без group/other write,
set-id и file capabilities. Guardian проверяет digest до запуска детей;
wrapper повторяет проверку и выполняет открытый проверенный descriptor.
Root и сервисная учётная запись доверены. SHA-256 закрепляет содержимое,
но не подтверждает издателя, устранение advisory или допуск релиза.
Сгенерированный TOML передаётся через sealed stdin; environment пустой,
API — private Unix socket, HTTP profiling/metrics выключены. Auto reload
и graceful restart не включаются.

Linux PDEATHSIG связан с потоком. Перед exec wrapper закрепляет текущий
поток, повторно устанавливает SIGKILL и сверяет parent PID с sealed snapshot.
Set-id и file capabilities запрещены: exec способен очистить сигнал.
Создающий поток guardian закреплён отдельно. Источники:
[PR_SET_PDEATHSIG](https://man7.org/linux/man-pages/man2/PR_SET_PDEATHSIG.2const.html),
[Go Exec](https://github.com/golang/go/blob/go1.27.3/src/syscall/exec_unix.go),
[memfd](https://man7.org/linux/man-pages/man2/memfd_create.2.html).

## Speaker и цикл сверки

GoBGP допускает только два локальных /32 на import и export, отвергая маршруты
от peers. Global import policy применяется и к локальному AddPath; один
default reject отфильтровал бы эти маршруты.
Задаются 1–16 отдельных IPv4 external peers, только IPv4 unicast. До открытия
route manager адаптер проверяет socket, ASN/router ID, policy assignments,
export statement, prefix set и peers через read-only RPC с startup deadline.
Другие API writers исключаются на весь срок incarnation. API readiness не
подтверждает RIB/FIB соседей.

Цикл последовательно получает primary/replica Patroni checks и новое SQL
наблюдение именно для выбранной роли. Противоречивые положительные роли,
отсутствующий допуск, SQL role mismatch, stale или failed SQL не разрешают
анонс. Unknown health приводит к сверке отзыва; завершённый отзыв разрешает
progress. Ошибки probes имеют фиксированные log categories. Ошибка маршрута
или журнала завершает адаптер и останавливает speaker. Heartbeat отправляется
только после завершения всего цикла.

Все durations положительны. `startup + cycle < heartbeat` и
`interval + cycle < heartbeat`; `probe <= cycle/3`, `RPC <= cycle`,
`max_age >= cycle`. Проверки через вычитание исключают переполнение суммы.
Supervisor ограничивает зависший callback. Kernel stall, зависший guardian,
сетевое обнаружение и удержание stale routes не входят в гарантию userspace
deadline. Бюджет PgBouncer учитывает ограниченные фоновые закрытия SQL-пробы.

## Модель и оставшаяся приёмка

Граф владения — guardian и два прямых ребёнка без демонизации. Завершение цикла
предшествует записи progress, запись — продлению lease. Основание:
[Go memory model](https://go.dev/ref/mem) и
[Go spec](https://go.dev/ref/spec#Send_statements). OS-границы проверяются
реальными subprocess/pidfd tests. Матрица `3^3 * 3 * 3 = 243` исчерпывает
сочетания health, SQL role и SQL age; это конечная проверка политики,
а не доказательство HA. Два /32 — непересекающиеся singleton sets;
trie и универсальная graph library для этой топологии не нужны.

SHA-256 использует стандартную библиотеку Go с AVX2/SHA-NI dispatch на
поддерживаемом amd64 ([source](https://github.com/golang/go/blob/go1.27.3/src/crypto/internal/fips140/sha256/sha256block_amd64.go)).
Собственных SIMD/crypto primitives нет; выбранный CPU path и throughput не
измерены. SCRAM остаётся в pgx; механизм задан
[RFC 7677](https://www.rfc-editor.org/rfc/rfc7677.html).

Проверки включают race/shuffle, config fuzzing, native GoBGP configuration,
смерть guardian после exec и изолированный PostgreSQL/PgBouncer RIB lifecycle
в `make sql-native`: анонс, отзыв при role mismatch, возврат и отзыв при потере
SQL. Test exec target использует закреплённые upstream config/server;
доставляемый `gobgpd` требует отдельной квалификации.

RIB tests не принимают BGP peers или клиентские транзакции. Для реальных
peers требуются policy по [RFC 8212](https://www.rfc-editor.org/rfc/rfc8212.html)
и измерение stale retention по [RFC 4724](https://www.rfc-editor.org/rfc/rfc4724.html#section-4.2).
Занятый socket и прежний UUID journal требуют отдельной доказанной процедуры
restart/recovery; автоматического сброса нет. Fencing Patroni/DCS/watchdog,
backup/restore, quorum трёх площадок и client outcomes остаются gates T3/T4.
