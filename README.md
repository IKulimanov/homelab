# homelab

Всё, что работает на домашнем сервере. Каждый сервис — контейнер Docker, обновления приезжают сами после push
в `main`, о каждом обновлении приходит сообщение в Telegram.

## Сервисы

| Сервис | Стек | Что делает | Зависит от | Данные |
|---|---|---|---|---|
| `budget-bot` | apps | учёт общих трат | Gemini (отчёт за месяц), SQLite | `/srv/budget-bot/data` |
| `jellyfin`, `sonarr`, `radarr`, `prowlarr`, `qbittorrent`, `bazarr`, `jellyseerr`, `flaresolverr`, `notifiarr` | media | медиасервер, [подробно](docs/media.md) | NVIDIA GPU, диск `/data` | `/data` |

Версии не пишутся здесь, их показывает `status.sh`.

## Каждый день

Все команды выполняются на сервере.

| Что | Команда |
|---|---|
| что запущено, какие версии | `/opt/homelab/scripts/status.sh` |
| логи | `docker logs -f --tail 100 <сервис>` |
| перезапустить | `docker restart <сервис>` |
| остановить | `docker stop <сервис>` — обновления его не запустят |
| запустить остановленный | `docker start <сервис>` |
| обновить всё сейчас | `sudo systemctl start homelab-update-now` |
| копия баз сейчас | `sudo /opt/homelab/scripts/backup.sh <сервис> manual` |

## Как приезжают обновления

- Свои сервисы: push в `main` → GitHub Actions собирает образ → сервер проверяет раз в 5 минут, снимает копию
  базы, ставит новую версию, присылает итог.
- Медиастек: каждую ночь в 4:00 свежие образы `:latest`.
- Откат: тег в `/opt/homelab/.env`, например `BUDGET_BOT_TAG=sha-2740b45`, и копия базы до обновления —
  [update-rollback.md](docs/update-rollback.md).

## Где что лежит

| Путь | Что |
|---|---|
| `/opt/homelab` | этот репозиторий; правки только через git |
| `/opt/homelab/.env` | токен служебного бота, закреплённые версии |
| `/srv/<сервис>/.env` | секреты сервиса |
| `/srv/<сервис>/data` | база сервиса |
| `/srv/media/.env` | пути и PUID медиастека |
| `/data` | фильмы, сериалы, конфиги медиастека |
| `/mnt/backup/<сервис>` | копии баз: каждый день в 3:30 и перед каждым обновлением, 30 дней |

## Документация

- [server-setup.md](docs/server-setup.md) — установка на сервер
- [migration.md](docs/migration.md) — переезд со старой установки и уборка
- [new-service.md](docs/new-service.md) — как добавить сервис
- [update-rollback.md](docs/update-rollback.md) — обновление и откат
- [backup-restore.md](docs/backup-restore.md) — копии и восстановление
- [media.md](docs/media.md) — медиастек
