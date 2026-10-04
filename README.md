# homelab

Всё, что работает на домашнем сервере. Каждый сервис — контейнер Docker, обновления приезжают сами после push
в `main`, о каждом обновлении и каждой аварии приходит сообщение служебного бота в Telegram.

## Сервисы

| Сервис | Стек | Что делает | Зависит от | Данные |
|---|---|---|---|---|
| `ops-bot` | platform | Бендер: алерты о важном, недельный отчёт, команды в Telegram, [подробно](docs/monitoring.md) | Docker, датчики хоста | `/srv/ops-bot/data` |
| `llm-gateway` | platform | учёт токенов, лимиты и баланс Gemini, [подробно](docs/llm.md) | Gemini API | `/srv/llm-gateway/data` |
| `dozzle` | platform | логи всех контейнеров в браузере, `http://<LAN_IP>:8889`, только из дома | Docker | нет |
| `panel` | platform | веб-панель «Планета Экспресс»: сервисы, логи, секреты, базы, LLM, доставки, `http://<LAN_IP>:8800`, только из дома, [подробно](docs/panel.md) | Docker, `llm-gateway`, `ops-bot` | `/srv/panel/data` |
| `budget-bot` | apps | учёт общих трат | Gemini через шлюз, SQLite | `/srv/budget-bot/data` |
| `wellbeing-bot` | apps | дневник самочувствия | Gemini через шлюз, SQLite | `/srv/wellbeing-bot/data` |
| `nutrition-assistant` | apps | учёт питания и веса | Gemini через шлюз, Anthropic, OpenAI, SQLite | `/srv/nutrition-assistant/data` |
| `jellyfin`, `sonarr`, `radarr`, `prowlarr`, `qbittorrent`, `bazarr`, `jellyseerr`, `flaresolverr` | media | медиасервер, [подробно](docs/media.md) | NVIDIA GPU, диск `/data` | `/data` |

Версии не пишутся здесь, их показывают `/status` и `status.sh`.

## Каждый день

Почти всё из таблицы есть и в панели: `/panel` в служебном боте присылает ссылку входа.

| Что | Telegram | На сервере |
|---|---|---|
| что запущено, версии, алерты | `/status` | `/opt/homelab/scripts/status.sh` |
| температура, память, диски | `/stats` | |
| логи | `/logs <сервис> [строк]` | `docker logs -f --tail 100 <сервис>` |
| перезапустить | `/restart <сервис>` | `docker restart <сервис>` |
| остановить (обновления его не запустят) | `/stop <сервис>` | `docker stop <сервис>` |
| запустить остановленный | `/start <сервис>` | `docker start <сервис>` |
| обновить всё сейчас | `/update` | `sudo systemctl start homelab-update-now` |
| копия базы сейчас | | `sudo /opt/homelab/scripts/backup.sh <сервис> manual` |
| секреты сервиса | | панель, «Сейф Гермеса» |
| запрос к базе | | панель, «Лаборатория» |
| расход LLM, баланс | `/usage`, `/balance` | |
| записать пополнение Gemini | `/topup 10` | |
| GIF для событий | `/gif` | |

## Как приезжают обновления

- Свои сервисы: push в `main` → GitHub Actions собирает образ → сервер проверяет раз в 5 минут, снимает копию
  базы, ставит новую версию, присылает итог.
- Цены и лимиты LLM: правка `stacks/platform/config/llm-gateway.yaml` и push, шлюз подхватит сам.
- Медиастек: каждую ночь в 4:00 свежие образы `:latest`.
- Откат: тег в `/srv/secrets/homelab.env`, например `BUDGET_BOT_TAG=sha-2740b45`, и копия базы до обновления —
  [update-rollback.md](docs/update-rollback.md).

## Где что лежит

| Путь | Что |
|---|---|
| `/opt/homelab` | этот репозиторий; правки только через git |
| `/srv/secrets/homelab.env` | токен служебного бота, пороги, закреплённые версии |
| `/srv/secrets/<сервис>.env` | секреты сервиса; у `llm-gateway` — настоящий ключ Gemini |
| `/srv/<сервис>/data` | база сервиса |
| `/srv/nutrition-assistant/config.yaml` | настройки `nutrition-assistant` |
| `/srv/secrets/media.env` | пути и PUID медиастека |
| `/srv/panel/data` | база панели: сессии, журнал действий; копии баз из Лаборатории |
| `/var/lib/homelab` | история обновлений и сводка копий для панели |
| `/data` | фильмы, сериалы, конфиги медиастека |
| `/mnt/backup/<сервис>` | копии баз: каждый день в 3:30 и перед каждым обновлением, 30 дней |

## Документация

- [server-setup.md](docs/server-setup.md) — установка на сервер
- [migration.md](docs/migration.md) — переезд со старой установки и уборка
- [monitoring.md](docs/monitoring.md) — служебный бот: команды, алерты, пороги
- [panel.md](docs/panel.md) — веб-панель: вход, экраны, кнопки
- [llm.md](docs/llm.md) — шлюз LLM: ключи, лимиты, баланс
- [new-service.md](docs/new-service.md) — как добавить сервис
- [update-rollback.md](docs/update-rollback.md) — обновление и откат
- [backup-restore.md](docs/backup-restore.md) — копии и восстановление
- [media.md](docs/media.md) — медиастек
