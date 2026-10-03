#!/usr/bin/env bash
# Установка homelab на сервер. Повторный запуск безопасен: существующие файлы и заданные значения не перезаписываются.
#   install.sh                    — проверки, сеть homelab, юниты и таймеры systemd, /srv/secrets/homelab.env,
#                                   каталоги ops-bot, llm-gateway и panel
#   install.sh service <svc>      — каталоги сервиса: /srv/<svc>/data и /srv/secrets/<svc>.env из образца
#   install.sh llm-key <svc>      — новый ключ шлюза для сервиса: в /srv/secrets/llm-gateway.env и в <svc>.env
#   install.sh enable-nightly     — включить ночное обновление медиастека (только после удаления Watchtower)
# Новые сервисы из compose ставит сам update.sh; service и llm-key — для ручного запуска.
# Docker и его настройки скрипт не трогает: перезапуск dockerd остановил бы медиастек.
set -euo pipefail
# shellcheck source=SCRIPTDIR/lib.sh
source "$(dirname "$(readlink -f "$0")")/lib.sh"

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
  # age нужен только для копии секретов: без него backup.sh пропустит её и напишет в журнал.
  command -v age >/dev/null || log "нет age: копия секретов делаться не будет (apt install age)"
}

install_base() {
  check_tools
  [[ "$(readlink -f "$HOMELAB_DIR")" == "$(readlink -f "$(dirname "$0")/..")" ]] ||
    die "скрипт запущен не из $HOMELAB_DIR: юниты systemd ссылаются на этот путь"

  # Журнал обновлений и сводку копий читает панель: каталог открыт на чтение, секретов в нём нет.
  install -d -m 0755 "$STATE_DIR"
  chmod 0755 "$STATE_DIR"
  install_secrets_dir
  if [[ ! -e "$HOMELAB_ENV" ]]; then
    install -m 0600 -o "$APP_UID" -g "$APP_UID" "$HOMELAB_DIR/env/homelab.env.example" "$HOMELAB_ENV"
    log "создан $HOMELAB_ENV — впиши OPS_BOT_TOKEN и OPS_CHAT_ID"
  fi
  # ops-bot ходит в docker.sock от пользователя nonroot: ему нужна группа docker хоста.
  env_default DOCKER_GID "$(getent group docker | cut -d: -f3)" "$HOMELAB_ENV"
  # Общий секрет ops-bot и llm-gateway. Его никто не вводит руками, поэтому генерируется здесь.
  env_default LLM_ADMIN_TOKEN "$(openssl rand -hex 24)" "$HOMELAB_ENV"
  # Им ops-bot просит у панели ссылку входа. Тоже не для людей.
  env_default PANEL_TOKEN "$(openssl rand -hex 24)" "$HOMELAB_ENV"
  # Адрес, с которого уходит трафик наружу, — это адрес сервера в домашней сети.
  local lan_ip
  lan_ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}')
  if [[ -n "$lan_ip" ]]; then
    env_default LAN_IP "$lan_ip" "$HOMELAB_ENV"
  else
    log "не определил адрес в домашней сети: впиши LAN_IP в $HOMELAB_ENV, иначе Dozzle и панель доступны только с сервера"
  fi

  # Сеть, через которую боты из стека apps ходят в llm-gateway из стека platform.
  # Создание сети не трогает работающие контейнеры.
  if ! docker network inspect homelab >/dev/null 2>&1; then
    docker network create homelab >/dev/null
    log "создана сеть docker homelab"
  fi

  install_service ops-bot
  install_service llm-gateway
  install_service panel

  local unit
  for unit in "$HOMELAB_DIR"/systemd/*; do
    install -m 0644 "$unit" "/etc/systemd/system/$(basename "$unit")"
  done
  systemctl daemon-reload
  systemctl enable --now homelab-update.timer homelab-backup.timer homelab-update.path homelab-apply.path homelab-backup-now.path
  log "таймеры включены: обновление раз в 5 минут, бэкап в 3:30; кнопки панели «Применить» и «Заморозить» работают"
  log "ночное обновление медиастека выключено до удаления Watchtower: install.sh enable-nightly"
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
