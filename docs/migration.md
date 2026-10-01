# Переезд в homelab и уборка старого

Документ одноразовый: по нему старая установка переносится в homelab, а то, что стало не нужно, удаляется.
Шаги идут по порядку. Каждый можно отложить, и сервер в промежутке работает.

## Правила

- Сначала заработало новое, потом выключается старое. Через неделю выключенное удаляется.
  Выключенное (`systemctl disable`) включается обратно одной командой. Удалённое возвращается только из архива.
- Перед каждым удалением снимается архив в `/mnt/backup/legacy/`, и проверяется, что он читается.
- Медиастек не останавливается ни на одном шаге. Поэтому до конца переезда **нельзя**:
  - перезапускать Docker (`systemctl restart docker`) и править `/etc/docker/daemon.json`;
  - запускать для медиастека `docker compose down`, `up --force-recreate`, `--remove-orphans`;
  - запускать `docker system prune` и `docker volume prune`;
  - трогать `/data`, драйвер NVIDIA и `nvidia-container-toolkit`.

Все команды ниже выполняются от root, если не сказано иное: `sudo -i`.

Архив делается одной функцией. Её нужно определить в начале каждой сессии:

```bash
archive() {  # archive <имя> <путь>...
  local out=/mnt/backup/legacy/$1-$(date +%F).tar.gz
  mkdir -p /mnt/backup/legacy && chmod 700 /mnt/backup/legacy
  tar -czf "$out" "${@:2}" && tar -tzf "$out" >/dev/null && echo "архив $out" || echo "АРХИВ НЕ СОЗДАН"
}
```

## Шаг 0. Подготовка и опись

Установка homelab: [server-setup.md](server-setup.md). После неё на сервере есть `/opt/homelab` и работают таймеры
обновления и бэкапа. Старым службам они не мешают: в `stacks/apps` пока нет ни одного запущенного контейнера,
а медиастек без `/srv/media/.env` обновление пропускает.

Опись:

```bash
mkdir -p /mnt/backup/legacy
/opt/homelab/scripts/legacy-audit.sh | tee /mnt/backup/legacy/audit-$(date +%F).txt
```

По выводу нужно сверить:
- раздел «Docker: где лежит compose медиастека» — путь к старому клону `media-server`, дальше он `<MEDIA_DIR>`;
- раздел «неизвестные контейнеры» и «прочие службы» — пусто или только то, что ты ставил сам и знаешь;
- раздел «/mnt/backup» — том смонтирован; если нет, сначала починить монтирование, без него бэкапы не делаются;
- раздел «logind» — `HandleLidSwitch=ignore` на месте.

## Шаг 1. budget-bot

Простой бота — минута-две, пока переносится база.

```bash
/opt/homelab/scripts/install.sh service budget-bot
```

Перенос настроек: значения `BOT_TOKEN`, `BOT_TIMEZONE`, `BOT_REMINDER_TIME`, `GEMINI_API_KEY`, `GEMINI_MODEL`
из `/etc/budget-bot.env` вписать в `/srv/budget-bot/.env`. `BOT_DB` не переносится, путь к базе задаёт compose.

```bash
diff <(grep -v '^#' /etc/budget-bot.env | grep . | sort) <(grep -v '^#' /srv/budget-bot/.env | grep . | sort)
```

В выводе должна остаться только строка `BOT_DB`.

Образ должен быть в ghcr: в репозитории `budget-bot` прошла сборка в Actions после push в `main`.

```bash
docker pull ghcr.io/ikulimanov/budget-bot:main
```

Переключение:

```bash
systemctl stop budget-bot
/opt/budget-bot/backup.sh /opt/budget-bot/budget.db /mnt/backup/legacy/budget-bot 365
install -m 0600 -o 65532 -g 65532 /opt/budget-bot/budget.db /srv/budget-bot/data/budget.db
ls /opt/budget-bot/          # файлов budget.db-wal и budget.db-shm быть не должно: служба остановлена штатно
cd /opt/homelab && docker compose -f stacks/apps/compose.yaml --env-file .env up -d budget-bot
docker logs -f budget-bot    # строка «бот запущен» с version=sha-…
```

Если `budget.db-wal` после остановки остался, его тоже нужно скопировать рядом с базой с тем же владельцем.

Проверка: в чате бота пройти обычную запись траты и `/report`, если он есть. Затем:

```bash
systemctl disable budget-bot
```

Если что-то не так, откат такой:

```bash
docker stop budget-bot && systemctl enable --now budget-bot
```

Старая база не менялась, поэтому всё, что записали через новую версию, при откате потеряется.

Дальше бот обновляется сам: push в `main` → сборка → через 5–10 минут сообщение служебного бота.

## Шаг 2. Медиастек — без остановки

Цель: тот же compose-проект `media-stack` начинает управляться из `/opt/homelab/stacks/media`. Контейнеры не
пересоздаются и не перезапускаются.

1. Переменные медиастека — как есть, без правок:

   ```bash
   install -d -m 0755 /srv/media
   install -m 0600 <MEDIA_DIR>/.env /srv/media/.env
   ```

2. Файл compose в homelab сверен с тем, что запущено. Если клон на сервере правили руками, его файл может отличаться
   от GitHub:

   ```bash
   diff <MEDIA_DIR>/docker-compose.yml /opt/homelab/stacks/media/compose.yaml
   ```

   Ожидается пусто. Если есть разница, сначала перенести её в `stacks/media/compose.yaml` через git, потом дальше.

3. Главная проверка: compose ничего не будет пересоздавать.

   ```bash
   /opt/homelab/scripts/diff.sh media
   ```

   У всех сервисов, включая `watchtower`, должно быть `ok`. Если у какого-то `config`, **дальше не идти**. Причина
   почти всегда в переменной: в старом каталоге compose брал её из окружения оболочки, а не из `.env`. Разница видна так:

   ```bash
   docker inspect jellyfin --format '{{json .Config.Env}}' | tr ',' '\n'
   docker compose -f /opt/homelab/stacks/media/compose.yaml --env-file /srv/media/.env config jellyfin
   ```

   Если стоит `image`, значит, кто-то уже скачал новый образ, а контейнер ещё старый. Ничего страшного: пункт 4 его
   не тронет из-за `--no-recreate`, а ночное обновление потом пересоздаст.

4. Взять под управление. `--no-recreate` запрещает пересоздавать существующие контейнеры, `--pull never` — скачивать
   образы:

   ```bash
   docker ps --filter label=com.docker.compose.project=media-stack --format '{{.Names}} {{.CreatedAt}}' > /root/media-before.txt
   cd /opt/homelab && docker compose -f stacks/media/compose.yaml --env-file /srv/media/.env up -d --no-recreate --pull never
   docker ps --filter label=com.docker.compose.project=media-stack --format '{{.Names}} {{.CreatedAt}}' | diff /root/media-before.txt -
   ```

   Последний `diff` должен быть пустым: те же контейнеры, то же время создания.

5. Убрать Watchtower. Остальные контейнеры это не затрагивает:

   ```bash
   docker stop watchtower && docker rm watchtower
   ```

   Затем в репозитории homelab удалить блок `watchtower` из `stacks/media/compose.yaml`, сделать push, на сервере
   `git -C /opt/homelab pull`. Удаление блока меняет только список сервисов, хэши остальных не меняются:

   ```bash
   /opt/homelab/scripts/diff.sh media     # все ok, watchtower в списке больше нет
   /opt/homelab/scripts/install.sh enable-nightly
   ```

   С этого момента медиастек обновляется в 4:00 через `update.sh nightly`, о результате приходит сообщение.

6. Старый клон через неделю:

   ```bash
   archive media-server <MEDIA_DIR>
   rm -rf <MEDIA_DIR>
   ```

   Контейнеры файлы клона не используют: все тома указывают в `/data`. Это проверено в пункте 2: compose-файлы
   совпадают.

Если хэши в пункте 3 так и не совпали, медиастек остаётся в старом каталоге, homelab его не трогает: без
`/srv/media/.env` стек пропускается. Пересоздание контейнеров тогда делается в удобное время, по твоему решению.

## Шаг 3. wellbeing-bot и nutrition-assistant

То же, что шаг 1, после того как боты получат Dockerfile и блоки в `stacks/apps/compose.yaml` (веха 3 плана).
Отличия `nutrition-assistant`:
- настройки в `/etc/nutrition-assistant/env` и `/etc/nutrition-assistant/config.yaml`;
  `config.yaml` переносится в `/srv/nutrition-assistant/config.yaml`, в нём `database_path: /data/nutrition.db`,
  `backup_dir: /backup`;
- база в `/var/lib/nutrition-assistant/`, перед переносом — `/admin backup` в боте.

## Шаг 4. simply-monitoring

После того как `ops-bot` неделю шлёт алерты параллельно со старым мониторингом (веха 4 плана):

```bash
systemctl disable --now monitor-check.timer monitor-report.timer
```

Через неделю:

```bash
archive monitor /opt/monitor /etc/systemd/system/monitor-*.service /etc/systemd/system/monitor-*.timer
rm -rf /opt/monitor /etc/systemd/system/monitor-check.{service,timer} /etc/systemd/system/monitor-report.{service,timer}
systemctl daemon-reload && systemctl reset-failed
```

**Не трогать:** `/etc/systemd/logind.conf` (настройки крышки, без них ноутбук уснёт) и модули датчиков, которые
добавил `sensors-detect`. Пакеты `lm-sensors`, `smartmontools`, `acpi`, `bc`, `nvme-cli` можно оставить: они ничего
не запускают. `sqlite3` нужен `backup.sh`.

## Шаг 5. Уборка ботов — через неделю после переключения каждого

Пример для `budget-bot`. Для `wellbeing-bot` пути те же с другим именем.

```bash
systemctl is-enabled budget-bot      # disabled — иначе не начинать
docker inspect -f '{{.State.Running}}' budget-bot   # true

archive budget-bot /opt/budget-bot /etc/budget-bot.env /etc/systemd/system/budget-bot.service
rm /etc/systemd/system/budget-bot.service && systemctl daemon-reload && systemctl reset-failed
rm -rf /opt/budget-bot /etc/budget-bot.env
userdel budgetbot
```

Строки старого `backup.sh` в cron root:

```bash
crontab -l > /mnt/backup/legacy/crontab-root-$(date +%F).txt
crontab -e        # удалить строки с /opt/budget-bot/backup.sh и /opt/wellbeing-bot/backup.sh
```

Копии теперь делает `homelab-backup.timer` в 3:30. Старые копии в `/mnt/backup/budget-bot` (имена
`budget-ГГГГ-ММ-ДД.db.gz`) остаются, новые лежат рядом.

`nutrition-assistant`:

```bash
archive nutrition-assistant /usr/local/bin/nutrition-assistant /etc/nutrition-assistant \
  /var/lib/nutrition-assistant /etc/systemd/system/nutrition-assistant.service
rm /etc/systemd/system/nutrition-assistant.service && systemctl daemon-reload && systemctl reset-failed
rm -rf /usr/local/bin/nutrition-assistant /etc/nutrition-assistant /var/lib/nutrition-assistant
userdel nutrition-assistant
```

В `/etc/fstab` у строки `/mnt/backup` убрать только опцию `x-systemd.before=nutrition-assistant.service`,
сама строка остаётся. Перемонтировать том не нужно:

```bash
cp /etc/fstab /etc/fstab.bak-$(date +%F)
nano /etc/fstab
findmnt --verify          # без ошибок
systemctl daemon-reload
findmnt /mnt/backup       # том по-прежнему смонтирован
```

Если на CIFS-томе были опции `uid=nutrition-assistant,gid=nutrition-assistant`, после `userdel` их нужно заменить
на `uid=0,gid=0`: бэкапы homelab пишет root. Изменение применится при следующем монтировании, сейчас том работает
как раньше.

## Шаг 6. Итог

```bash
/opt/homelab/scripts/legacy-audit.sh
```

Во всех разделах про старое должно быть «нет». Ещё нужно проверить:
- `docker ps` — у контейнеров медиастека время создания с начала переезда или с ночного обновления;
- `ls /mnt/backup/*/` — свежие копии баз за сегодня;
- в `logind` по-прежнему `HandleLidSwitch=ignore`.

На GitHub архивировать `media-server` и `simply-monitoring`: Settings → Archive this repository.
