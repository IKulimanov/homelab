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

Порядок: платформа (служебный бот и шлюз) → `budget-bot` → медиастек → `wellbeing-bot` → `nutrition-assistant` →
боты на шлюз → `simply-monitoring` → уборка.

Боты со старой службой systemd (`budget-bot.service` и другие) автоустановка не трогает, пока служба включена или
работает: иначе рядом со старым ботом поднялся бы второй с пустой базой и тем же токеном. Поэтому боты переносятся
руками по шагам ниже; после `systemctl disable` контейнер уже запущен, и дальше его обновляет `update.sh`.

## Шаг 0. Подготовка и опись

Установка homelab: [server-setup.md](server-setup.md). После неё на сервере есть `/opt/homelab`, сеть Docker
`homelab` и таймеры обновления и бэкапа. Старым службам они не мешают: в `stacks/apps` пока нет ни одного
запущенного контейнера, а медиастек без `/srv/secrets/media.env` обновление пропускает.

Опись:

```bash
mkdir -p /mnt/backup/legacy
/opt/homelab/scripts/legacy-audit.sh | tee /mnt/backup/legacy/audit-$(date +%F).txt
```

По выводу нужно сверить:
- раздел «Docker: где лежит compose медиастека» — путь к старому клону `media-server`, дальше он `<MEDIA_DIR>`;
- раздел «неизвестные контейнеры» и «прочие службы» — пусто или только то, что ты ставил сам и знаешь;
- раздел «/mnt/backup» — том смонтирован; если нет, сначала починить монтирование, без него бэкапы не делаются.
  Записать тип тома (`cifs` или `ext4`) и опции: они нужны в шаге 5;
- раздел «logind» — `HandleLidSwitch=ignore` на месте.

## Шаг 1. Платформа: ops-bot и llm-gateway

Образы `ops-bot` и `llm-gateway` собирает Actions в репозитории `homelab` после push в `main`.

1. Настройки. `install.sh` уже создал `/srv/secrets/homelab.env` с `DOCKER_GID` и `LLM_ADMIN_TOKEN`. Вписать туда
   `OPS_BOT_TOKEN`, `OPS_CHAT_ID` (см. server-setup, шаг 4), по желанию `HEALTHCHECK_URL`
   ([monitoring.md](monitoring.md)). В `/srv/secrets/llm-gateway.env` вписать `GEMINI_API_KEY` — настоящий ключ из
   AI Studio. Без него шлюз не запустится.

2. Запуск. Руками не нужен: в течение 5 минут после заполнения ключей `update.sh` сам поднимет `ops-bot`,
   `llm-gateway` и `dozzle` и пришлёт «… установлен». Медиастек и старые боты это не затрагивает. Не ждать:

   ```bash
   sudo systemctl start homelab-update-now
   docker logs ops-bot          # «ops-bot запущен»; в чат бот при запуске не пишет
   docker logs llm-gateway      # «шлюз запущен»
   docker inspect -f '{{.State.Health.Status}}' llm-gateway    # healthy через полминуты
   ```

3. Проверка в Telegram: `/status` показывает контейнеры медиастека и `platform`, `/stats` — температуру и диски.
   Если температура «нет данных», см. [monitoring.md](monitoring.md).

4. Баланс Gemini. Посмотреть остаток в AI Studio → Billing и записать его: `/balance 17.40`.

Шлюзом пока никто не пользуется: боты переключаются на него в шаге 6.

Откат: `docker stop ops-bot llm-gateway`. Старые службы от платформы не зависят.

С этого момента `ops-bot` работает параллельно с `simply-monitoring`. Неделю сверять алерты обоих, затем шаг 7.

## Шаг 2. budget-bot

Простой бота — минута-две, пока переносится база.

```bash
/opt/homelab/scripts/install.sh service budget-bot
```

Перенос настроек: значения `BOT_TOKEN`, `BOT_TIMEZONE`, `BOT_REMINDER_TIME`, `GEMINI_API_KEY`, `GEMINI_MODEL`
из `/etc/budget-bot.env` вписать в `/srv/secrets/budget-bot.env`. `BOT_DB` не переносится, путь к базе задаёт compose.
`GEMINI_BASE_URL` пока пустой.

```bash
diff <(grep -v '^#' /etc/budget-bot.env | grep . | sort) <(grep -v '^#' /srv/secrets/budget-bot.env | grep . | sort)
```

В выводе должна остаться только строка `BOT_DB` и пустая `GEMINI_BASE_URL=`.

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
cd /opt/homelab && docker compose -f stacks/apps/compose.yaml --env-file /srv/secrets/homelab.env up -d budget-bot
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

## Шаг 3. Медиастек — без остановки

Цель: тот же compose-проект `media-stack` начинает управляться из `/opt/homelab/stacks/media`. Контейнеры не
пересоздаются и не перезапускаются.

Пока шаг не закончен, не нажимать `/update` и не запускать `homelab-update-now`: с появлением `/srv/secrets/media.env` они
обновляют и медиастек, и при несовпадении конфигурации пересоздали бы его контейнеры. Таймер раз в 5 минут
медиастек не трогает.

1. Переменные медиастека — как есть, без правок:

   ```bash
   install -m 0600 -o 65532 -g 65532 <MEDIA_DIR>/.env /srv/secrets/media.env
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
   docker compose -f /opt/homelab/stacks/media/compose.yaml --env-file /srv/secrets/media.env config jellyfin
   ```

   Если стоит `image`, значит, кто-то уже скачал новый образ, а контейнер ещё старый. Ничего страшного: пункт 4 его
   не тронет из-за `--no-recreate`, а ночное обновление потом пересоздаст.

4. Взять под управление. `--no-recreate` запрещает пересоздавать существующие контейнеры, `--pull never` — скачивать
   образы:

   ```bash
   docker ps --filter label=com.docker.compose.project=media-stack --format '{{.Names}} {{.CreatedAt}}' > /root/media-before.txt
   cd /opt/homelab && docker compose -f stacks/media/compose.yaml --env-file /srv/secrets/media.env up -d --no-recreate --pull never
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
`/srv/secrets/media.env` стек пропускается. Пересоздание контейнеров тогда делается в удобное время, по твоему решению.

## Шаг 4. wellbeing-bot

Сейчас: служба `wellbeing-bot.service` от пользователя `wellbeingbot`, программа и база в `/opt/wellbeing-bot`,
настройки в `/etc/wellbeing-bot.env`, копии делает `/usr/local/bin/wellbeing-backup.sh` из cron root в 4:00.

```bash
/opt/homelab/scripts/install.sh service wellbeing-bot
```

Из `/etc/wellbeing-bot.env` перенести в `/srv/secrets/wellbeing-bot.env` значения `BOT_TOKEN`, `ADMIN_ID`,
`GEMINI_API_KEY`, `GEMINI_MODEL`, `BOT_TIMEZONE`, `MAX_USERS`. `BOT_DB` не переносится.

```bash
diff <(grep -v '^#' /etc/wellbeing-bot.env | grep . | sort) <(grep -v '^#' /srv/secrets/wellbeing-bot.env | grep . | sort)
docker pull ghcr.io/ikulimanov/wellbeing-bot:main
```

Переключение:

```bash
systemctl stop wellbeing-bot
/usr/local/bin/wellbeing-backup.sh /opt/wellbeing-bot/wellbeing.db /mnt/backup/legacy/wellbeing-bot 365
ls /opt/wellbeing-bot/       # wellbeing.db-wal и -shm быть не должно
install -m 0600 -o 65532 -g 65532 /opt/wellbeing-bot/wellbeing.db /srv/wellbeing-bot/data/wellbeing.db
cd /opt/homelab && docker compose -f stacks/apps/compose.yaml --env-file /srv/secrets/homelab.env up -d wellbeing-bot
docker logs -f wellbeing-bot
```

Проверка в боте, затем `systemctl disable wellbeing-bot`. Откат — как у `budget-bot`:
`docker stop wellbeing-bot && systemctl enable --now wellbeing-bot`.

## Шаг 5. nutrition-assistant

Сейчас: программа `/usr/local/bin/nutrition-assistant`, служба от пользователя `nutrition-assistant`, секреты
в `/etc/nutrition-assistant/env`, настройки в `/etc/nutrition-assistant/config.yaml`, база в
`/var/lib/nutrition-assistant/nutrition.db`. Бот сам делает копии в `/mnt/backup/nutrition-assistant` в 03:30
и сам удаляет старые. В контейнере это продолжается: `/mnt/backup` смонтирован в `/backup`.

1. Доступ контейнера к `/mnt/backup`. Контейнер пишет от uid 65532.

   Если том `ext4` (или другой локальный), хватит `install.sh` из пункта 2: он сделает владельцем каталога
   `/mnt/backup/nutrition-assistant` uid 65532.

   Если том `cifs` с опциями `uid=nutrition-assistant,gid=nutrition-assistant,dir_mode=0700,file_mode=0600`,
   владельца задают опции монтирования, и их нужно поменять на `uid=65532,gid=65532`. Root пишет на такой том и
   так. Делать не в 3:30–4:00, когда идут копии; медиастек `/mnt/backup` не использует:

   ```bash
   cp /etc/fstab /etc/fstab.bak-$(date +%F)
   nano /etc/fstab           # uid=nutrition-assistant,gid=nutrition-assistant → uid=65532,gid=65532
   findmnt --verify
   systemctl daemon-reload
   umount /mnt/backup && mount /mnt/backup
   findmnt /mnt/backup
   ```

   Старая служба до своего отключения в пункте 4 копий больше не сделает: том теперь не её. Это не страшно, копия
   будет снята в пункте 3.

2. Каталоги и настройки:

   ```bash
   /opt/homelab/scripts/install.sh service nutrition-assistant
   ```

   Он создаёт `/srv/nutrition-assistant/config.yaml` из `/etc/nutrition-assistant/config.yaml` и меняет в нём
   два пути на пути внутри контейнера: `database_path: /data/nutrition.db`, `backup_dir: /backup/nutrition-assistant`.
   В конце он пишет, может ли uid 65532 писать в каталог копий. Если не может — вернуться к пункту 1.

   Секреты: из `/etc/nutrition-assistant/env` перенести `TELEGRAM_BOT_TOKEN`, `GEMINI_API_KEY`, а также
   `ANTHROPIC_API_KEY` и `OPENAI_API_KEY`, если они там есть, в `/srv/secrets/nutrition-assistant.env`.

   ```bash
   diff /etc/nutrition-assistant/config.yaml /srv/nutrition-assistant/config.yaml   # только два пути
   docker pull ghcr.io/ikulimanov/nutrition-assistant:main
   ```

3. Переключение. Остановка службы занимает до 40 секунд:

   ```bash
   systemctl stop nutrition-assistant
   sqlite3 /var/lib/nutrition-assistant/nutrition.db ".backup '/mnt/backup/legacy/nutrition-$(date +%F).db'"
   ls /var/lib/nutrition-assistant/     # nutrition.db-wal и -shm быть не должно
   install -m 0600 -o 65532 -g 65532 /var/lib/nutrition-assistant/nutrition.db /srv/nutrition-assistant/data/nutrition.db
   cd /opt/homelab && docker compose -f stacks/apps/compose.yaml --env-file /srv/secrets/homelab.env up -d nutrition-assistant
   docker logs -f nutrition-assistant
   ```

4. Проверка: записать еду в боте; в боте `/admin backup` — в `/mnt/backup/nutrition-assistant` появилась новая
   копия. Затем `systemctl disable nutrition-assistant`.

   Откат: `docker stop nutrition-assistant && systemctl enable --now nutrition-assistant`. Если в пункте 1 менялись
   опции `cifs`, для отката их нужно вернуть и перемонтировать том.

## Шаг 6. Боты на шлюз

Порядок для каждого бота. Шлюз выдаёт боту свой ключ, а настоящий ключ Gemini остаётся только у шлюза:

```bash
/opt/homelab/scripts/install.sh llm-key budget-bot
/opt/homelab/scripts/update.sh auto      # пересоздаёт llm-gateway и бота с новыми ключами
```

Скрипт записывает `LLM_KEY_BUDGET_BOT` в `/srv/secrets/llm-gateway.env`, а в `/srv/secrets/budget-bot.env` —
`GEMINI_API_KEY` с тем же значением и `GEMINI_BASE_URL=http://llm-gateway:8080`. У `nutrition-assistant` адрес
записывается в `config.yaml`, `gemini.base_url`.

Проверка: вызвать в боте то, что ходит в Gemini (у `budget-bot` — отчёт за месяц), затем `/usage` в служебном
боте показывает вызов и стоимость. Подробно — [llm.md](llm.md).

Откат одного бота: в `/srv/secrets/<svc>.env` вернуть настоящий ключ, `GEMINI_BASE_URL` очистить, затем
`scripts/update.sh auto`.

Ключ для самого `ops-bot` — так же, `install.sh llm-key ops-bot`. С ним Бендер комментирует недельный отчёт через
Gemini, без него ставит готовую фразу ([monitoring.md](monitoring.md)).

## Шаг 7. simply-monitoring

После того как `ops-bot` неделю шлёт алерты параллельно со старым мониторингом:

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
добавил `sensors-detect`: без них `ops-bot` не видит температуру. Пакеты `lm-sensors`, `smartmontools`, `acpi`, `bc`,
`nvme-cli` можно оставить: они ничего не запускают. `sqlite3` нужен `backup.sh`.

## Шаг 8. Уборка ботов — через неделю после переключения каждого

`budget-bot`:

```bash
systemctl is-enabled budget-bot      # disabled — иначе не начинать
docker inspect -f '{{.State.Running}}' budget-bot   # true

archive budget-bot /opt/budget-bot /etc/budget-bot.env /etc/systemd/system/budget-bot.service
rm /etc/systemd/system/budget-bot.service && systemctl daemon-reload && systemctl reset-failed
rm -rf /opt/budget-bot /etc/budget-bot.env
userdel budgetbot
```

`wellbeing-bot`:

```bash
systemctl is-enabled wellbeing-bot   # disabled
docker inspect -f '{{.State.Running}}' wellbeing-bot

archive wellbeing-bot /opt/wellbeing-bot /etc/wellbeing-bot.env /etc/systemd/system/wellbeing-bot.service \
  /usr/local/bin/wellbeing-backup.sh
rm /etc/systemd/system/wellbeing-bot.service && systemctl daemon-reload && systemctl reset-failed
rm -rf /opt/wellbeing-bot /etc/wellbeing-bot.env /usr/local/bin/wellbeing-backup.sh
userdel wellbeingbot
```

Строки старых копий в cron root. Удалять вместе со скриптом `wellbeing-backup.sh`, иначе cron будет падать каждую ночь:

```bash
crontab -l > /mnt/backup/legacy/crontab-root-$(date +%F).txt
crontab -e        # удалить строки с /opt/budget-bot/backup.sh и /usr/local/bin/wellbeing-backup.sh
```

Копии теперь делает `homelab-backup.timer` в 3:30. Старые копии в `/mnt/backup/budget-bot` и
`/mnt/backup/wellbeing-bot` остаются, новые (`<база>-ГГГГ-ММ-ДД.db.gz`) лежат рядом.

`nutrition-assistant`. Сначала проверить `grep credentials /etc/fstab`: если файл учётных данных `cifs` лежит
в `/etc/nutrition-assistant/`, перенести его, например в `/etc/cifs-backup` с правами 0600, поправить путь в
`credentials=` и проверить `findmnt --verify`. Иначе после удаления каталога том не смонтируется при перезагрузке.

```bash
systemctl is-enabled nutrition-assistant   # disabled
docker inspect -f '{{.State.Running}}' nutrition-assistant

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

## Шаг 9. Итог

```bash
/opt/homelab/scripts/legacy-audit.sh
```

Во всех разделах про старое должно быть «нет». Ещё нужно проверить:
- `docker ps` — у контейнеров медиастека время создания с начала переезда или с ночного обновления;
- `ls /mnt/backup/*/` — свежие копии баз за сегодня;
- в `logind` по-прежнему `HandleLidSwitch=ignore`.

На GitHub архивировать `media-server` и `simply-monitoring`: Settings → Archive this repository.
