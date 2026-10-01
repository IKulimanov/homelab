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

# notify TEXT — сообщение служебному боту. Без токена только пишет в журнал: обновление не должно падать
# из-за уведомления.
notify() {
  local text=$1 token chat
  token=$(env_get OPS_BOT_TOKEN "$HOMELAB_ENV")
  chat=$(env_get OPS_CHAT_ID "$HOMELAB_ENV")
  log "уведомление: $text"
  if [[ -z "$token" || -z "$chat" ]]; then
    log "OPS_BOT_TOKEN или OPS_CHAT_ID не заданы, сообщение не отправлено"
    return 0
  fi
  curl -fsS --max-time 15 -o /dev/null "https://api.telegram.org/bot${token}/sendMessage" \
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
