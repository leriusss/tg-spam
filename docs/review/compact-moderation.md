# Компактные карточки модерации

Локальная реализация от master `444aabc2a940f4ae5f38a2591fb0a2290b350c34`.
Production, runtime DB и compose не менялись. Recovery остаётся существующим механизмом.

## Исходное состояние и откат

Канонический checkout: `C:\Users\User\Desktop\Проекты\Github\tg-spam`.
До работы: чистая ветка `codex/moderation-recovery-stage2`,
HEAD `a625e0b10b1cc59d001bb3650447a0e6c1824ded`.
Отдельная ветка: `codex/compact-russian-moderation`, от актуального origin/master.
Исторические каталоги и изменения infrastructure checkout сохранены.

Полная копия tracked-файлов master до правок:
`C:\Users\User\AppData\Local\Temp\tg-spam-444aabc-before-compact.zip`.
SHA256: `62aee6489364dfc05a6e43ae7063816447e5060993c917c9660aaf9419620fdd`.
Копия включает все затрагиваемые исходники; новые файлы отсутствовали в master.
Локальный откат: сохранить feature commit и переключиться на прежнюю ветку либо
создать отдельный checkout master `444aabc2`. Не сбрасывать чужие изменения.
После merge откат UI — revert feature commit, без отката БД, worker или конфигурации.
Никакой production-команды отката или deployment эта работа не выполняет.

## Read-only разбор двух случаев

13 октября 2026, Балтия, 10:29 Europe/Kaliningrad (08:29 UTC), до этапа 2:
сохранённый `baltia-full.log` содержит два incoming update с original message ID
`91700` и `91701`, времена 08:29:19.664 и 08:29:20.364 UTC.
Оба независимо завершились `ban=confirmed delete=confirmed`; обе отправки карточек
связаны с соответствующим оригиналом. Это два оригинала с одинаковым текстом,
не повторная доставка одной карточки. Объединять их по содержимому нельзя.

13 октября 2026, около 21:45 Europe/Kaliningrad (19:45 UTC), после этапа 2,
original `91705`: журнал открыт `sqlite3 -readonly`, без изменения данных.

| UTC | Событие | Результат |
| --- | --- | --- |
| 19:45:40.119 | Incoming original 91705 | принят |
| 19:45:43.469 | notice | confirmed, первая попытка |
| 19:45:44.696 | ban | confirmed, первая попытка, без reconciliation |
| 19:45:45.825 | card | confirmed, первая попытка |
| 19:45:47.106 | original_delete | confirmed, первая попытка |
| 19:46:48.244 | notice обновлён | confirmed |
| 19:46:49.365 | card обновлена | confirmed |

Журнал: card notification_id=913, generation=delivered=3, state=confirmed,
code пустой. Ban/delete attempts=1, state=confirmed, code пустой.
Логи и журнал согласуются с обновлением той же карточки, поскольку сохранённый
notification_id направляет notifyCard в editMessageText. Прямой UI-снимок итоговой
Telegram-карточки не запрашивался. Retry и ошибки уведомлений в этом интервале
не обнаружены; повторное обновление после удаления — штатная новая версия, не retry.

Причина pending-сводки: admission атомарно создаёт/помечает notice; worker выбирает
его раньше ban/card/delete. Старый notify отправлял сводку для любого состава
журнала, включая полностью pending, и показывал внутреннюю операцию card.

## Изменение интерфейса

Recovery-карточка — русский заголовок, название из Telegram chat data, ссылка на
пользователя или публичный sender_chat, сокращённая экранированная цитата и факты.
Без доступного title название не выдумывается. Cleanup показывает успех и остаток;
absent, unknown, reconciliation и истечение retry horizon не превращаются в успех
или доказательство истечения 48-часового окна Telegram.

Ссылки из цитаты разрываются невидимым разделителем только в отображаемой копии;
HTML escaping выполняется после сокращения. LinkPreviewOptions.IsDisabled=true.
В журнале сохраняется полный оригинал; UpdateHam/UpdateSpam используют c.Text.
Техническая информация и детекция находятся в «Подробнее», возврат — «К карточке».
Обе команды проходят существующую проверку привязки карточки. Старые mutation
callback ID, подтверждения и условия кнопок сохранены; MB — новый read-only возврат.

Обычный pending/успешный notice подтверждается внутри очереди без Telegram-отправки.
Ошибки, ручная работа, отказ по ёмкости и наблюдаемые повторы оставляют короткое
предупреждение. Если предупреждение уже отправлено, его обновление сообщает об
устранении проблем. Журнал, дедупликация, retry schedule и moderation policy не меняются.
Количество retry-проблем основано на существующей ограниченной выборке Digest;
полный список по-прежнему доступен оператору в журнале.

Legacy/training/dry путь сохраняет исходный контракт восстановления текста из
сообщения; переводятся кнопки. Сокращение и разрыв ссылок применяются к recovery
карточкам, где полный оригинал хранится независимо от отображения. Старые карточки
до этапа 2 не переписываются и не объединяются.

## Визуальное ревью и проверки

`compact-moderation.html` и PNG созданы из реального Go renderer на синтетических
данных через TestCompactPreview. Это макет, не live Telegram screenshot.
Повторить: задать COMPACT_PREVIEW_PATH и выполнить
`go test ./app/events -run '^TestCompactPreview$' -count=1`.

Локально: `go test -short ./app/events ./app/moderation` и `go build ./...` — успешно;
`golangci-lint run --new-from-rev=444aabc2 --timeout=5m` — 0 issues.
Нормализация комментариев выполнена, несвязанные правки исключены.
Tests покрывают фактические состояния, partial cleanup, escaping/лимит отображения,
sender_chat, запрет обычной pending-сводки, Подробнее/возврат/чужую привязку.
Существующие lifecycle tests проверяют learning original, кнопки, safety gates,
training/dry, отмену, reconciliation и worker.
Полный race/build/lint прогон — Linux GitHub CI draft PR (локальный Windows Go без
C compiler, локальный Docker daemon недоступен). Более широкий Windows-прогон
выявляет ошибки Unix mount/имён backup с двоеточиями/открытых DB файлов и storage timeout;
они не являются основанием ослаблять проверки.

## State-based UI revision in PR #3

Before this revision: clean feature HEAD b885999c998ef84a38afa6d6bc22c95cd9f708bb.
Tracked source backup: Documents/Codex/backups/compact-antispam-20261013/pr3-b885999c-before-state-ui.zip.
Rollback preserves feature commits and returns to that revision; no DB/config rollback.

Buttons reflect core work, ignoring internal notification actions. A clean success has only
Изменить бан and Подробнее. Pending/running core work exposes cancellation; unclosed manual
records expose manual closure only when core work has settled, preserving the existing worker
refusal to close live actions. Подтвердить модерацию requires a current actionable decision,
no live work, no incomplete local decision and no unfinished undo. An expired known deletion
window does not offer a pointless new approval. Existing authority checks and mutation callback
IDs are retained. Historically failed records remain available for explicit manual closure even
when a later approval succeeded, since success does not close the journal's manual records.

The compact projection groups deletion outcomes by exact chat/message pair and membership
operations by target. Recorded next_at ordering identifies successive worker outcomes; canceled
future retries do not override later independent work. A later successful approval replaces the
old failure in the compact text, and confirmed unban replaces the old ban. A pending/failed undo
keeps the last confirmed membership fact visible. Confirmed deletion remains a fact after absent
replay. All original rows remain unchanged; Подробнее pages through the complete action history
(two records per page, new read-only MP callbacks bound to the same card).

Detail timestamps use Europe/Kaliningrad with bundled IANA timezone data. Empty reasons and
irrelevant zero IDs/dates are omitted; attempt count remains a relevant field even before the
first attempt. Mutation policy, journal, retry scheduling, training and original text are unchanged.

Additional tests cover exact keyboard composition and callback refs, real-journal refusal →
Approve success → unban, preserved history and bound pagination, canceled future retries,
manual closure, pending/undo/safety states and readable timezone-specific dates. Preview HTML/PNG
now include successful re-approval, unban and a report requiring a new decision.
