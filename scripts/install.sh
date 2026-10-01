#!/usr/bin/env bash
# Установка homelab на сервер. Повторный запуск безопасен: существующие файлы не перезаписываются.
#   install.sh                    — проверки, юниты и таймеры systemd, /opt/homelab/.env
#   install.sh service <svc>      — каталоги нового сервиса: /srv/<svc>/data и /srv/<svc>/.env из образца
#   install.sh enable-nightly     — включить ночное обновление медиастека (только после удаления Watchtower)
# Docker и его настройки скрипт не трогает: перезапуск dockerd остановил бы медиастек.
set -euo pipefail
# shellcheck source=SCRIPTDIR/lib.sh
source "$(dirname "$(readlink -f "$0")")/lib.sh"

# Пользователь nonroot в образах distroless: от него работают свои сервисы.
APP_UID=65532

check_tools() {
  local missing=() tool
  for tool in docker git curl jq flock setpriv sqlite3 gzip mountpoint; do
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

install_base() {
  check_tools
  [[ "$(readlink -f "$HOMELAB_DIR")" == "$(readlink -f "$(dirname "$0")/..")" ]] ||
    die "скрипт запущен не из $HOMELAB_DIR: юниты systemd ссылаются на этот путь"

  install -d -m 0700 "$STATE_DIR"
  if [[ ! -e "$HOMELAB_ENV" ]]; then
    install -m 0600 "$HOMELAB_DIR/env/homelab.env.example" "$HOMELAB_ENV"
    log "создан $HOMELAB_ENV — впиши OPS_BOT_TOKEN и OPS_CHAT_ID"
  fi

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
  log "$svc: каталоги готовы"
}

main() {
  [[ $EUID -eq 0 ]] || die "нужен root"
  case "${1:-}" in
    "") install_base ;;
    service) [[ -n "${2:-}" ]] || die "использование: install.sh service <svc>"; install_service "$2" ;;
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
