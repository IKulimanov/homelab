#!/usr/bin/env bash
# Установка homelab на сервер. Повторный запуск безопасен: существующие файлы и заданные значения не перезаписываются.
#   install.sh                    — проверки, сеть homelab, юниты и таймеры systemd, /opt/homelab/.env,
#                                   каталоги ops-bot и llm-gateway
#   install.sh service <svc>      — каталоги сервиса: /srv/<svc>/data и /srv/<svc>/.env из образца
#   install.sh llm-key <svc>      — новый ключ шлюза для сервиса: в /srv/llm-gateway/.env и в /srv/<svc>/.env
#   install.sh enable-nightly     — включить ночное обновление медиастека (только после удаления Watchtower)
# Docker и его настройки скрипт не трогает: перезапуск dockerd остановил бы медиастек.
set -euo pipefail
# shellcheck source=SCRIPTDIR/lib.sh
source "$(dirname "$(readlink -f "$0")")/lib.sh"

# Пользователь nonroot в образах distroless: от него работают свои сервисы.
APP_UID=65532
GATEWAY_URL=http://llm-gateway:8080

check_tools() {
  local missing=() tool
  for tool in docker git curl jq flock setpriv sqlite3 gzip mountpoint openssl; do
    command -v "$tool" >/dev/null || missing+=("$tool")
  done
  [[ ${#missing[@]} -eq 0 ]] || die "не хватает программ: ${missing[*]} (apt install sqlite3 jq)"

  local version
  version=$(docker compose version --short 2>/dev/null) || die "нет плагина docker compose"
  # config --hash и --images нужны update.sh.
  if [[ "$(printf '%s\n' 2.24.0 "${version#v}" | sort -V | head -n 1)" != "2.24.0" ]]; then
    die "docker compose $version, нужна 2.24 или новее"
  fi
  log "программы на месте, docker compose $version"
}

# env_set KEY VALUE FILE — записать значение ключа, заменив прежнее.
env_set() {
  local key=$1 value=$2 file=$3
  if grep -q "^${key}=" "$file"; then
    sed -i "s|^${key}=.*|${key}=${value}|" "$file"
  else
    printf '%s=%s\n' "$key" "$value" >>"$file"
  fi
}

# env_default KEY VALUE FILE — записать значение, только если ключ пустой. Заданное руками не трогается.
env_default() {
  [[ -n "$(env_get "$1" "$3")" ]] || env_set "$@"
}

install_base() {
  check_tools
  [[ "$(readlink -f "$HOMELAB_DIR")" == "$(readlink -f "$(dirname "$0")/..")" ]] ||
    die "скрипт запущен не из $HOMELAB_DIR: юниты systemd ссылаются на этот путь"

  install -d -m 0700 "$STATE_DIR"
  if [[ ! -e "$HOMELAB_ENV" ]]; then
    install -m 0600 "$HOMELAB_DIR/env/homelab.env.example" "$HOMELAB_ENV"
    log "создан $HOMELAB_ENV — впиши OPS_BOT_TOKEN и OPS_CHAT_ID"
  fi
  # ops-bot ходит в docker.sock от пользователя nonroot: ему нужна группа docker хоста.
  env_default DOCKER_GID "$(getent group docker | cut -d: -f3)" "$HOMELAB_ENV"
  # Общий секрет ops-bot и llm-gateway. Его никто не вводит руками, поэтому генерируется здесь.
  env_default LLM_ADMIN_TOKEN "$(openssl rand -hex 24)" "$HOMELAB_ENV"

  # Сеть, через которую боты из стека apps ходят в llm-gateway из стека platform.
  # Создание сети не трогает работающие контейнеры.
  if ! docker network inspect homelab >/dev/null 2>&1; then
    docker network create homelab >/dev/null
    log "создана сеть docker homelab"
  fi

  install_service ops-bot
  install_service llm-gateway

  local unit
  for unit in "$HOMELAB_DIR"/systemd/*; do
    install -m 0644 "$unit" "/etc/systemd/system/$(basename "$unit")"
  done
  systemctl daemon-reload
  systemctl enable --now homelab-update.timer homelab-backup.timer homelab-update.path
  log "таймеры включены: обновление раз в 5 минут, бэкап в 3:30"
  log "ночное обновление медиастека выключено до удаления Watchtower: install.sh enable-nightly"
}

install_service() {
  local svc=$1 example="$HOMELAB_DIR/env/$1.env.example"
  [[ "$svc" =~ ^[a-z0-9-]+$ ]] || die "имя сервиса: строчные буквы, цифры, дефис"
  install -d -m 0755 "$SRV_DIR/$svc"
  install -d -m 0700 -o "$APP_UID" -g "$APP_UID" "$SRV_DIR/$svc/data"
  if [[ ! -e "$SRV_DIR/$svc/.env" ]]; then
    if [[ -r "$example" ]]; then
      install -m 0600 "$example" "$SRV_DIR/$svc/.env"
    else
      install -m 0600 /dev/null "$SRV_DIR/$svc/.env"
    fi
    log "создан $SRV_DIR/$svc/.env — заполни его"
  fi

  case "$svc" in
    ops-bot)
      # Сюда ops-bot кладёт файл-триггер команды /update, его ждёт homelab-update.path.
      install -d -m 0700 -o "$APP_UID" -g "$APP_UID" "$SRV_DIR/ops-bot/trigger"
      ;;
    nutrition-assistant) install_nutrition ;;
  esac
  log "$svc: каталоги готовы"
}

install_nutrition() {
  local cfg="$SRV_DIR/nutrition-assistant/config.yaml" old=/etc/nutrition-assistant/config.yaml root dir require
  if [[ ! -e "$cfg" ]]; then
    [[ -r "$old" ]] || die "нет $old: положи в $cfg копию config.example.yaml из репозитория nutrition-assistant"
    # Читает контейнер (группа 65532), пишет только root.
    install -m 0640 -o root -g "$APP_UID" "$old" "$cfg"
    # Пути внутри контейнера: база в /data, копии в /backup (это /mnt/backup хоста).
    sed -i -e 's|^database_path:.*|database_path: /data/nutrition.db|' \
      -e 's|^backup_dir:.*|backup_dir: /backup/nutrition-assistant|' "$cfg"
    log "создан $cfg из $old, пути к базе и копиям заменены на пути в контейнере"
  fi

  # Бот сам делает копии в backup_dir и каталог не создаёт.
  root=$(env_get BACKUP_ROOT "$HOMELAB_ENV")
  root=${root:-/mnt/backup}
  dir="$root/nutrition-assistant"
  require=$(env_get BACKUP_REQUIRE_MOUNT "$HOMELAB_ENV")
  if [[ "${require:-yes}" == yes ]] && ! mountpoint -q "$root"; then
    log "$root не смонтирован: создай $dir с владельцем $APP_UID, когда том будет на месте"
    return 0
  fi
  mkdir -p "$dir"
  # Контейнеру нужно пройти через $root к своему каталогу. Только проход, без чтения списка:
  # каталоги копий других сервисов закрыты (0700, root).
  chmod o+x "$root" 2>/dev/null || true
  # На CIFS владельца задают опции монтирования uid= и gid=, chown там не работает.
  chown "$APP_UID:$APP_UID" "$dir" 2>/dev/null || true
  if ! setpriv --reuid="$APP_UID" --regid="$APP_UID" --clear-groups test -w "$dir"; then
    log "внимание: пользователь $APP_UID не может писать в $dir. Для CIFS нужны опции uid=$APP_UID,gid=$APP_UID"
  fi
}

# llm_key SVC — выдать сервису ключ шлюза. Настоящий ключ Gemini остаётся только у шлюза.
llm_key() {
  local svc=$1 var key gw_env="$SRV_DIR/llm-gateway/.env" svc_env="$SRV_DIR/$1/.env"
  [[ -w "$gw_env" ]] || die "нет $gw_env: сначала install.sh"
  [[ -w "$svc_env" ]] || die "нет $svc_env: сначала install.sh service $svc"
  grep -q "^  $svc:" "$HOMELAB_DIR/stacks/platform/config/llm-gateway.yaml" ||
    die "$svc нет в clients в stacks/platform/config/llm-gateway.yaml: шлюз ответит 403"

  var="LLM_KEY_$(tr 'a-z-' 'A-Z_' <<<"$svc")"
  key=$(openssl rand -hex 24)
  env_set "$var" "$key" "$gw_env"
  env_set GEMINI_API_KEY "$key" "$svc_env"
  env_set GEMINI_BASE_URL "$GATEWAY_URL" "$svc_env"
  log "$svc: ключ записан в $var и в GEMINI_API_KEY сервиса, GEMINI_BASE_URL=$GATEWAY_URL"
  # nutrition-assistant берёт адрес Gemini не из env, а из config.yaml.
  if [[ "$svc" == nutrition-assistant ]]; then
    local cfg="$SRV_DIR/nutrition-assistant/config.yaml"
    if grep -q '^gemini:' "$cfg"; then
      sed -i "/^gemini:/,/^[^ #]/ s|^  base_url:.*|  base_url: $GATEWAY_URL|" "$cfg"
    else
      printf '\ngemini:\n  base_url: %s\n' "$GATEWAY_URL" >>"$cfg"
    fi
    grep -q "^  base_url: $GATEWAY_URL" "$cfg" || die "не удалось вписать gemini.base_url в $cfg, впиши руками"
    log "$cfg: gemini.base_url=$GATEWAY_URL"
  fi
  # Изменённый env_file меняет хэш конфигурации: update.sh сам пересоздаст оба контейнера, шлюз первым.
  log "контейнеры llm-gateway и $svc пересоздаст update.sh в течение 5 минут; сразу — scripts/update.sh auto"
}

main() {
  [[ $EUID -eq 0 ]] || die "нужен root"
  case "${1:-}" in
    "") install_base ;;
    service) [[ -n "${2:-}" ]] || die "использование: install.sh service <svc>"; install_service "$2" ;;
    llm-key) [[ -n "${2:-}" ]] || die "использование: install.sh llm-key <svc>"; llm_key "$2" ;;
    enable-nightly)
      if docker ps -a --format '{{.Names}}' | grep -qx watchtower; then
        die "контейнер watchtower ещё есть: два обновлятеля будут мешать друг другу"
      fi
      systemctl enable --now homelab-update-nightly.timer
      log "ночное обновление включено"
      ;;
    *) die "неизвестная команда: $1" ;;
  esac
}

main "$@"
