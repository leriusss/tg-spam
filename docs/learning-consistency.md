# Согласованность обучения

## Объём

Исправление подготовлено от `master` `c1e71fa01512326d1f191bfcadfe434d45f31559`.
Исходные материалы: аудит ATAS `docs/audits/spam-learning-20261017/{README,EXAMPLES}.md`.
Production, runtime-обучение, DNS, конфиги и compose не изменялись. Telegram/CAS/OpenAI не вызывались.
Все изменения данных выполнялись на fixtures и локальных копиях; тестовые moderation API используют заглушки.

До правки regression-тесты показали расхождение работающей модели и чистой загрузки после повторного spam,
spam → ham → spam, удаления и параллельных повторов. Save failure уже возвращался как ошибка.

## Механизм

Встроенный `storage.SampleUpdater` реализует дополнительный контракт `tgspam.SampleSnapshot`:
одним SQL SELECT читает обе метки и оба origin того же gid. Detector сериализует сохранение, чтение снимка,
построение classifier и similarity своей write-блокировкой. Проверка сообщения использует read-блокировку.
Кандидат модели публикуется целиком, только после успешного чтения и построения.

Полное перестроение выбрано вместо дополнительного обратного индекса/unlearning: наборы малы, а исключённые слова,
построчное чтение, classifier и similarity уже имеют единый путь загрузки. Алгоритм классификации, stopwords,
пороги, short-message guard, источник authored text, доверие и moderation policies не изменены.
При первоначальной загрузке существующих данных накопленный лишний online-вес отсутствует: модель строится только из строк БД.

DB commit и память **не являются одной транзакцией**. В частности:

1. Ошибка записи возвращается вызывающему коду; модель не изменяется.
2. Если запись завершилась, а snapshot/построение не удалось, возвращается ошибка с указанием сохранения данных
   и недоступности модели. Старые classifier/similarity не используются; Check содержит `sample model unavailable`
   с `Error`. Детерминированные проверки и существующие gates сохраняются.
3. Следующий Check повторяет реконструкцию из БД. Следующая успешная learning/delete операция или reload тоже восстанавливает
   состояние. После остановки процесса новый процесс читает уже сохранённые актуальные строки. Старую БД восстанавливать не нужно.
4. Ошибки чтения/scanner больше не превращаются в успешную загрузку частичного корпуса. Существующий scanner limit около 64 KiB
   на строку сохранён; превышение явно возвращается как ошибка. Для исправления сохранённой проблемной строки можно удалить её
   через соответствующий learning API; этот путь не требует unlearn из неисправной модели.

Нет обещания восстановления в процессе после OOM/panic. Нет синхронизации нескольких независимых процессов,
пишущих в одну и ту же БД: built-in deployment использует один detector на отдельную instance DB.
Прямые внешние SQL/import-изменения требуют штатного reload; они не входят в сериализованный learning API.

## Идентичность и совместимость

- Схема и ключи не меняются: SQLite `(gid,message)`, PostgreSQL существующий `(gid,message_hash)`.
  `SpamFilter` по-прежнему заменяет LF пробелами при UpdateSpam/UpdateHam. Нового trim/case-fold/сходства как identity нет.
- SQLite Add использует UPSERT, сохраняя id/timestamp при той же метке и origin. При смене метки/origin сохраняется
  прежний контракт замены текущего значения; исторические строки не переразмечаются. Bulk import не менялся.
- Timestamp ties для readers/snapshot разрешаются по id. Это фиксирует порядок чтения одного набора, в том числе порядок
  similarity samples, без изменения формулы, порогов или принятия решений по сходству.
- Learning deletion ограничено указанной меткой, gid и точным текстом. Отсутствующий или уже перемеченный в другой класс
  образец — успешный no-op; повтор старого delete не удаляет противоположную метку. Строгий публичный DeleteMessage не менялся.
- Для сторонних library updaters без `SampleSnapshot` сохранён legacy incremental путь. Гарантия persisted-state consistency
  относится к встроенным SQLite/PostgreSQL adapters и сторонним adapters, предоставляющим полный Snapshot.
  Это ограничение не затрагивает существующие application learning paths: оба application updater используют один Samples store.

## Пути вызова

Команды `/spam`, надёжные admin-forward, legacy callbacks, recovery approve/restore, community report/approve
и HTTP/UI update сходятся в `SpamFilter.UpdateSpam/UpdateHam` → `Detector` → built-in updater.
HTTP/UI delete использует `RemoveDynamicSpamSample/RemoveDynamicHamSample` → Detector remove.
`/ban`, ручной Telegram ban/delete и обычный automatic verdict не превращены в обучение.
Callback IDs, journal decisions/idempotency и порядок moderation actions не менялись.

Дополнительно исправлен связанный reload: `SpamFilter.ReloadSamples` полностью читает и закрывает каждый SQL reader
до открытия следующего. Recovery использует pool из одной connection; раньше несколько незавершённых readers
делали reload недоступным. Regression-тест до этой правки завершался по timeout; после правки single-connection reload проходит.
Параметры pool и recovery journal не изменены. Ошибки чтения/закрытия reader возвращаются вызывающему коду.

## Проверки

Новые проверки:

- `TestLearningMatchesPersistedSnapshot`: add, repeat, spam → ham → spam, повтор ham, delete, delete absent;
  сравнение внутренних classifier/similarity и всех checks с чистой загрузкой после каждого шага.
- `TestLearningSQLiteReopen`: те же операции и ham delete с закрытием/повторным открытием настоящей SQLite;
  проверяется нормализация LF через настоящий SpamFilter и сохранность оставшегося корпуса.
- Concurrent replay на fake snapshot и SQLite; одновременный Check во время переключения ham/spam допускает только целые модели.
- Save failure, snapshot failure после сохранения/удаления, ошибка построения (длинная строка), автоматическое восстановление.
- Реальные direct `/spam` и recovery callbacks поверх настоящих detector/SQLite; Telegram API заменён mock.
  Повтор доставки callback и повтор команды не добавляют веса, unban/restore обучает ham без остаточного spam.
- Single-connection reload, ошибки чтения и закрытия SQL reader.

Выполнены `go test -race ./...` и профильные проверки. Полный Windows run имеет прежние ограничения:
`TestBackupDB` использует запрещённые двоеточия в filename; `Test_checkVolumeMount` требует Unix mount;
`Test_activateServerOnly` не всегда освобождает SQLite handle до TempDir cleanup. Эти три падения воспроизведены на отдельном
неизменённом checkout исходного master. PostgreSQL integration tests требуют Docker, которого локально нет;
baseline `TestSqliteToPostgresIntegration` воспроизводит тот же panic о rootless Docker на Windows.

Проверка всех остальных пакетов: `go test -race -short -skip 'Test_activateServerOnly|Test_checkVolumeMount|TestBackupDB' ./...`.
Профильные пакеты: `lib/tgspam`, `app/bot`, `app/storage`, `app/events`, `app/webapi` с `-race -short`.
Выполнены Windows `go build -race ./...`, сборка Linux executable с CGO=0, проверки и race build `_examples/simplechat`,
`golangci-lint run --new-from-rev=origin/master`, gofmt и нормализация комментариев изменённых Go-файлов.
Lint изменений: 0 issues. Полный `golangci-lint run` на Windows сообщает 108 issues против 145 на исходном master;
сравнение `(файл, текст предупреждения)` не нашло новых issues, разница — 37 gofmt-предупреждений checkout/форматирования.
Остальной lint baseline не исправлялся этой задачей.
Lint `_examples/simplechat` сообщает 3 gofmt-предупреждения в неизменённых файлах примера; его race tests/build проходят.
Полный Linux/Docker matrix оставлен GitHub CI; его результаты следует смотреть в проверках PR.

## Примеры аудита

Для существующего release и локально собранного кандидата использован тот же диагностический набор из 24 примеров
на каждом из трёх согласованных корпусных снимков. Изолированный Linux network namespace имел только loopback;
CAS и Telegram не были доступны. В контроле обычного classifier exact guard исключён через `authored_text=""`;
production exact rule не изменялся. Групповая часть normal pipeline проверена отдельно тем же harness.

**0 различий** до обучения, **0 различий** после первого обучения, **0 различий** после reopen против результатов первого
обучения для каждого из трёх корпусов (сравнивались полные checks/details, а не только итог spam).
Повторное обучение и исправление метки дали следующие результаты на Neoport:

| Шаг, исходный текст «Есть проба 0.5г» без exact guard | До исправления | После исправления |
|---|---|---|
| До обучения | ham 75.42%, similarity 0.00 | то же |
| Spam один раз | spam 56.70%, similarity 1.00 | то же |
| Spam второй раз | spam 74.74%, similarity 1.00 | spam 56.70%, similarity 1.00 |
| Затем Ham | итог spam; classifier ham 57.52%, similarity 1.00 | итог ham; classifier ham 92.48%, similarity 0.00 |
| Restart после Ham | ham 92.48%, similarity 0.00 | то же, без изменения online-результата |
| Отдельно: Spam → delete | итог spam из-за similarity 1.00 | итог ham; модель равна чистой загрузке |

Ограничения антиспама не маскируются: после одного spam-образца вариант `0.7г` всё ещё ham, а легитимная проба золота
и жалобы могут быть false positive. Эта задача исправляет consistency, не обобщение/accuracy.

## Замеры на копиях

Локальный Windows amd64, Go 1.26.1, Ryzen 5 4600H; данные не передавались внешним сервисам.
Benchmark включает загрузку модели из подготовленных строк, без SQL/network; B/op — суммарные аллокации, не peak RSS.
`model-live-B` — разница live Go heap после GC при удержании модели. Это не гарантия production latency.

| Корпус | Строк | Baseline rebuild | Candidate rebuild | Аллокации candidate | Live heap модели |
|---|---:|---:|---:|---:|---:|
| Neoport | 647 | 35.41 ms | 37.52 ms | 4,301,078 B/op | 2,155,408 B |
| Балтия | 665 | 36.26 ms | 46.81 ms | 4,336,992 B/op | 2,175,280 B |
| Европа | 661 | 40.15 ms | 43.66 ms | 4,322,895 B/op | 2,168,424 B |

При 300 checks и 10 параллельных последовательных rebuilds (после добавления одной строки):

| Корпус | Check baseline p95 / max | Check при обновлении p95 / max |
|---|---|---|
| Neoport | 1.000 / 1.530 ms | 1.019 / 43.963 ms |
| Балтия | 1.001 / 1.516 ms | 1.008 / 69.379 ms |
| Европа | 1.000 / 1.757 ms | 1.005 / 42.722 ms |

Проверка может ждать полного rebuild; это осознанная цена простой синхронной реализации. SQL I/O, production CPU,
tail latency под реальным потоком и process peak RSS здесь не измерены. Увеличение корпуса требует повторных замеров;
две модели временно сосуществуют в памяти. Нет отдельной очереди, фонового worker или внешнего сервиса.

Воспроизведение: подготовить приватные `{neoport,baltia,europa}.json` с массивами `Spam`, `Ham`, `Excluded`,
задать `TGSPAM_AUDIT_CORPUS` на их каталог и запустить
`go test ./lib/tgspam -run TestLearningCorpusContention -v -bench BenchmarkLearningCorpus -benchmem -benchtime=1s`.
Без env тест замеров и benchmark пропускаются; fixture/regression-тесты автономны.

## Сохранённое исходное состояние и будущий откат

Engine перед работой был чистым. Проверенный Git bundle исходного commit и копии затронутых исходников сохранены локально:
`C:/Users/User/Documents/Codex/backups/tg-spam-learning-20261017`; там же полные журналы проверок и приватные replay copies.
Незакоммиченные материалы аудита в соседнем ATAS оставлены без изменений.

Схема не меняется. Никакого внедрения этим PR не выполняется. Перед отдельным разрешённым внедрением сохранить текущий
image и его digest, конфиги/compose diff, логи и согласованные runtime SQLite backups; проверить integrity/checksum и новый
image на копиях, включая загрузку существующих данных. После переключения контролировать ошибки learning/rebuild и задержки checks.

Откат будущего релиза: вернуть сохранённый image, сохранив **актуальные** runtime-БД и незавершённые moderation actions.
Старые SQLite backups не разворачивать, историю не переигрывать. Старый binary читает тот же schema/corpus; он также
возвращает прежние lifecycle-дефекты для последующего online-обучения. Не запускать bulk reclassification/cleanup.

## Отдельная следующая задача: source=auto

Не исправлена намеренно. В одном процессе используется счётчик первых ham, production threshold=3. Approved storage
не сохраняет count; после загрузки любой строки, включая auto, detector ставит count=threshold+1. Поэтому один предыдущий
ham после рестарта может включить pre-approved bypass обычных проверок. Exact authored guard остаётся перед auto bypass,
manual trust и superuser gates сохранены. Пересмотр автоматического доверия требует отдельной задачи и собственного review.
