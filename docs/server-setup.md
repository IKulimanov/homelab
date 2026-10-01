# Установка homelab на сервер

Сервер: Ubuntu, Docker Engine с плагином compose 2.24 или новее. Docker на текущем сервере уже стоит для
медиастека. Его настройки и `nvidia-container-toolkit` эта установка не трогает.

## 1. Пакеты

```bash
sudo apt install -y git sqlite3 jq curl
docker compose version
```

`sqlite3` нужен для копий баз, `jq` — для `update.sh` и `diff.sh`.

## 2. Доступ к GitHub

Сервер забирает с GitHub две вещи: репозиторий `homelab` (`git pull` раз в 5 минут) и образы сервисов из ghcr.io.

Репозиторий `homelab`. Секретов в нём нет, поэтому проще всего сделать его публичным, тогда `git clone` по https
работает без ключей. Если он приватный, нужен deploy key только на чтение:

```bash
sudo ssh-keygen -t ed25519 -f /root/.ssh/homelab_deploy -N ''
sudo cat /root/.ssh/homelab_deploy.pub    # GitHub → homelab → Settings → Deploy keys → Add, без write access
```

и в `/root/.ssh/config`:

```
Host github.com
  IdentityFile /root/.ssh/homelab_deploy
```

Образы ботов приватные, как и их репозитории; образы `ops-bot` и `llm-gateway` собираются в `homelab`.
Для скачивания нужен classic token GitHub только с правом
`read:packages`: Settings → Developer settings → Personal access tokens → Tokens (classic). Вход делается один раз,
от root, потому что `update.sh` работает от root:

```bash
sudo docker login ghcr.io -u IKulimanov
```

Токен вводится в ответ на запрос пароля. Docker хранит его в `/root/.docker/config.json`.

Общий workflow сборки лежит в `homelab`, а вызывают его репозитории ботов. Если `homelab` приватный: GitHub →
homelab → Settings → Actions → General → Access → «Accessible from repositories owned by the user».

## 3. Клон и установка

```bash
sudo git clone https://github.com/IKulimanov/homelab.git /opt/homelab
sudo /opt/homelab/scripts/install.sh
```

`install.sh` проверяет программы и делает следующее:
- создаёт `/opt/homelab/.env` из образца и вписывает в него `DOCKER_GID` (группа docker, через неё `ops-bot`
  читает `docker.sock`) и `LLM_ADMIN_TOKEN` (общий секрет `ops-bot` и `llm-gateway`, вводить его не нужно);
- создаёт сеть Docker `homelab`: через неё боты ходят в шлюз. Работающие контейнеры это не трогает;
- создаёт `/srv/ops-bot` и `/srv/llm-gateway`;
- ставит юниты systemd и включает:
  - `homelab-update.timer` — проверка новых версий раз в 5 минут;
  - `homelab-backup.timer` — копии баз в 3:30;
  - `homelab-update.path` — обновление по команде `/update` из служебного бота.

Ночное обновление медиастека остаётся выключенным, пока жив Watchtower. Его включает шаг 3 в
[migration.md](migration.md).

## 4. Служебный бот

В `/opt/homelab/.env` вписать `OPS_BOT_TOKEN` и `OPS_CHAT_ID`. Бота лучше создать нового в @BotFather: неделю
`ops-bot` работает рядом с `simply-monitoring`, а два процесса с одним токеном мешают друг другу принимать команды.
Chat id — твой Telegram ID, его показывает, например, @userinfobot. Новому боту сначала написать `/start`:
без этого Telegram не даст ему писать первым. Проверка:

```bash
sudo bash -c 'source /opt/homelab/scripts/lib.sh; notify "homelab установлен"'
```

## 5. Проверка

```bash
systemctl list-timers 'homelab-*'
sudo /opt/homelab/scripts/legacy-audit.sh | less
```

Дальше — перенос сервисов по [migration.md](migration.md).

## Крышка ноутбука

Сервер — ноутбук. Чтобы он не засыпал при закрытой крышке, в `/etc/systemd/logind.conf` должно быть:

```
HandleLidSwitch=ignore
HandleLidSwitchExternalPower=ignore
HandleLidSwitchDocked=ignore
```

На текущем сервере это уже настроил `simply-monitoring`. На новом сервере эти строки нужно вписать и выполнить
`sudo systemctl restart systemd-logind`.
