# Обновление и откат

## Как приезжает новая версия

1. Push в `main` репозитория сервиса. GitHub Actions прогоняет `go vet` и `go test`, собирает образ и публикует
   два тега: `ghcr.io/ikulimanov/<svc>:main` и `:sha-<7 символов коммита>`.
2. `homelab-update.timer` раз в 5 минут запускает `scripts/update.sh auto`:
   - `git pull` в `/opt/homelab`, чтобы правки compose-файлов тоже доехали;
   - `docker compose pull` для стеков с политикой `auto`;
   - для каждого запущенного сервиса сравнивает образ и хэш конфигурации с тем, что в compose-файле;
   - изменившийся сервис с базой сначала копируется (`backup.sh <svc> pre-update`), потом пересоздаётся
     `docker compose up -d --no-deps <svc>`;
   - через 30 секунд проверка: контейнер работает, не перезапускался, health не `unhealthy`.
3. В Telegram приходит итог: `budget-bot: sha-2740b45 → sha-9c1d0e2, работает` или «не поднялся» с хвостом лога.

Медиастек обновляется так же, но ночью в 4:00 (`update.sh nightly`). Обновить всё сразу:

```bash
sudo systemctl start homelab-update-now
```

или `/update` в служебном боте.

Остановленный руками сервис обновление не запускает.

## Если сборка не приехала

```bash
systemctl status homelab-update.timer
journalctl -u homelab-update -n 50
```

Частые причины: сборка в Actions упала (тесты), истёк токен `docker login ghcr.io`, `git pull` упёрся в локальные
правки в `/opt/homelab`. Серверная копия правится только через git, руками в ней ничего не меняется.

## Откат

Миграции баз необратимы: новая версия могла изменить схему, старая на ней не заработает. Поэтому откат состоит
из двух частей: старый образ и копия базы, снятая перед обновлением.

1. Найти прошлую версию: в сообщении об обновлении (`sha-…` слева от стрелки) или
   `docker image ls ghcr.io/ikulimanov/<svc>`.
2. Закрепить её в `/opt/homelab/.env`: `BUDGET_BOT_TAG=sha-2740b45`. Закреплённую версию `update.sh` не трогает,
   и новые сборки из `main` не приедут.
3. Если новая версия меняла схему базы, восстановить копию `pre-update` ([backup-restore.md](backup-restore.md)).
4. Применить:

   ```bash
   cd /opt/homelab && sudo docker compose -f stacks/apps/compose.yaml --env-file .env up -d --no-deps budget-bot
   ```

Когда исправление в `main` готово, строку `BUDGET_BOT_TAG=` нужно очистить. Следующий запуск таймера поставит
свежую сборку.
