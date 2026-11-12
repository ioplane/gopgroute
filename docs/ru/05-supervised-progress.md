# Контроль выполнения процессов

Статус: внутренние Linux-компоненты; демон и runtime-релиз отсутствуют.

`supervisor.Run` владеет двумя отдельными незапущенными командами прямых
дочерних процессов: выделенным GoBGP speaker и адаптером. Другие нагрузки и
писатели API этого speaker исключаются оператором. Дочерние процессы не
демонизируются и не отделяют потомков: supervisor управляет прямыми process
handles, а не группой процессов или произвольными PID. Стандартные потоки —
nil либо существующие `*os.File`. ExtraFiles, Cancel и SysProcAttr не задаются.

Supervisor создаёт закрытый pipe, унаследованный только адаптером как fd 3.
Адаптер пишет один `+` после завершённого цикла сверки. Отдельный heartbeat
timer или независимая goroutine записи запрещены: они способны сохранять
анонс при зависшей сверке или journal fsync. `reconcile.Run` выполняет циклы
последовательно, возвращает ошибки и истёкшие deadline, отправляет progress
только после успеха. Интервал начинается после завершения; пропущенные
интервалы не создают очередь догоняющих циклов. Цикл включает полную проверку
здоровья и необходимые операции маршрутов, в том числе отзыв при unknown.
Callback может игнорировать context и зависнуть; независимую границу отказа
обеспечивает процесс supervisor.

Выход любого ребёнка, EOF, неверный heartbeat, отсутствие progress и отмена
останавливают оба процесса. Shutdown отправляет SIGTERM обоим принадлежащим
handles, ждёт StopTimeout, отправляет SIGKILL оставшимся и ждёт ещё не более
StopTimeout. Ошибки Wait и нереапируемая kernel task возвращаются вызывающему
коду. Uninterruptible kernel I/O может задержать фактическое завершение;
новую incarnation нельзя запускать до доказанной смерти прежних детей.
Автоматических restart и journal reset нет. Guardian должен планироваться
независимо от детей; зависший guardian не обеспечивает userspace deadline.
Progress, прочитанный после предыдущего heartbeat deadline, не продлевает lease.

Linux Pdeathsig завершает каждого ребёнка при смерти создавшего его guardian
thread. Run удерживает этот OS thread закреплённым на время работы и остановки
детей. Полномочие сигнализации — Go process handle, не сохранённый номер PID.
Семантику задают закреплённые исходники Go:
[`SysProcAttr`](https://github.com/golang/go/blob/go1.27.3/src/syscall/exec_linux.go)
и [process handles](https://github.com/golang/go/blob/go1.27.3/src/os/pidfd_linux.go).

Реальные subprocess tests проверяют SIGKILL, SIGSTOP, игнорирование SIGTERM,
ошибку запуска, потерю progress и смерть guardian. Native GoBGP tests проверяют
анонс перед stall/uncertainty адаптера и завершение выделенного speaker после
отказа. BGP peers не настроены: peer graceful restart, удержание RIB/FIB,
клиентские результаты, конфигурация демона и Patroni/DCS/watchdog fencing
остаются отдельными gates. Остановка speaker не доказывает отзыв у peer или
fencing БД.
