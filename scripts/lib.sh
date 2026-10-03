# Общие настройки и функции скриптов homelab. Подключается через source, сам не запускается.
# shellcheck shell=bash

HOMELAB_DIR=${HOMELAB_DIR:-/opt/homelab}
HOMELAB_ENV=${HOMELAB_ENV:-$HOMELAB_DIR/.env}
SRV_DIR=${SRV_DIR:-/srv}
STATE_DIR=${STATE_DIR:-/var/lib/homelab}

# Стеки: каталог в stacks/, политика обновления, файл переменных для подстановки в compose.
# auto — проверка раз в 5 минут, nightly — в 4:00. Политика задаётся здесь, а не меткой в compose:
# новая метка у сервиса медиастека пересоздала бы его контейнер.
# shellcheck disable=SC2034 # используется в скриптах, которые подключают lib.sh
STACKS=(
  "platform auto $HOMELAB_ENV"
  "apps auto $HOMELAB_ENV"
  "media nightly $SRV_DIR/media/.env"
)

log() { printf '%s %s\n' "$(date '+%F %T')" "$*"; }
die() { log "ошибка: $*" >&2; exit 1; }

# env_get KEY FILE — значение ключа из env-файла. Файл не исполняется через source: в нём секреты и чужой синтаксис.
env_get() {
  local key=$1 file=$2
  [[ -r "$file" ]] || return 0
  sed -n "s/^${key}=//p" "$file" | tail -n 1 | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'$/\1/"
}

# compose STACK ENV_FILE args... — docker compose для стека с его файлом и переменными.
compose() {
  local stack=$1 env_file=$2
  shift 2
  local args=(-f "$HOMELAB_DIR/stacks/$stack/compose.yaml")
  [[ -r "$env_file" ]] && args+=(--env-file "$env_file")
  docker compose "${args[@]}" "$@"
}

# Фразы Бендера и GIF событий. GIF хранит ops-bot в своей базе, сюда — только file_id Telegram.
VOICE_DIR=${VOICE_DIR:-$HOMELAB_DIR/stacks/platform/config/voice}
OPS_DB=${OPS_DB:-$SRV_DIR/ops-bot/data/ops.db}

# quip EVENT [NAME] — случайная фраза события, {name} заменяется на NAME. Нет фраз — пусто.
quip() {
  local file="$VOICE_DIR/$1.txt"
  [[ -r "$file" ]] || return 0
  grep -v '^#' "$file" | grep . | shuf -n 1 | sed "s|{name}|${2:-}|g"
}

# gif_for EVENT — file_id случайной GIF события из базы ops-bot. Нет базы или GIF — пусто.
gif_for() {
  [[ -r "$OPS_DB" ]] && command -v sqlite3 >/dev/null || return 0
  [[ "$1" =~ ^[a-z-]+$ ]] || return 0
  sqlite3 -readonly "$OPS_DB" "SELECT file_id FROM gifs WHERE event = '$1' ORDER BY random() LIMIT 1" 2>/dev/null || true
}

# notify TEXT [EVENT] — сообщение служебному боту, с фразой и GIF события, если они есть. Без токена только
# пишет в журнал: обновление не должно падать из-за уведомления.
notify() {
  local text=$1 event=${2:-} token chat line gif api
  token=$(env_get OPS_BOT_TOKEN "$HOMELAB_ENV")
  chat=$(env_get OPS_CHAT_ID "$HOMELAB_ENV")
  log "уведомление: $text"
  if [[ -z "$token" || -z "$chat" ]]; then
    log "OPS_BOT_TOKEN или OPS_CHAT_ID не заданы, сообщение не отправлено"
    return 0
  fi
  if [[ -n "$event" ]]; then
    line=$(quip "$event")
    [[ -n "$line" ]] && text="$text

$line"
    gif=$(gif_for "$event")
  fi
  api="https://api.telegram.org/bot${token}"
  # Подпись к GIF — не длиннее 1024 символов; длинный текст (например, с логом) идёт обычным сообщением.
  if [[ -n "$gif" && ${#text} -le 1000 ]] &&
    curl -fsS --max-time 15 -o /dev/null "$api/sendAnimation" \
      --data-urlencode "chat_id=${chat}" --data-urlencode "animation=${gif}" --data-urlencode "caption=${text}"; then
    return 0
  fi
  curl -fsS --max-time 15 -o /dev/null "$api/sendMessage" \
    --data-urlencode "chat_id=${chat}" --data-urlencode "text=${text}" \
    -d disable_web_page_preview=true || log "не удалось отправить уведомление"
}

# short_version IMAGE_ID — версия образа для людей: sha из метки сборки, иначе версия из метки, иначе начало id.
short_version() {
  local id=$1 rev ver
  rev=$(docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$id" 2>/dev/null)
  if [[ -n "$rev" && "$rev" != "<no value>" ]]; then
    echo "sha-${rev:0:7}"
    return
  fi
  ver=$(docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.version"}}' "$id" 2>/dev/null)
  if [[ -n "$ver" && "$ver" != "<no value>" ]]; then
    echo "$ver"
    return
  fi
  id=${id#sha256:}
  echo "${id:0:12}"
}
