#!/usr/bin/env bash
# Обновление сервисов: забрать новые образы и конфиги, пересоздать только изменившиеся контейнеры.
#   update.sh auto     — стеки с политикой auto (таймер раз в 5 минут)
#   update.sh nightly  — стеки с политикой nightly (таймер в 4:00)
#   update.sh all      — все стеки (команда /update)
# Остановленный сервис не запускается: если его остановили руками, так и задумано.
# Перед пересозданием сервиса с базой снимается копия: миграции новых версий необратимы.
set -euo pipefail
# shellcheck source=SCRIPTDIR/lib.sh
source "$(dirname "$(readlink -f "$0")")/lib.sh"

POLICY=${1:-auto}
CHECK_DELAY=${CHECK_DELAY:-30}
mkdir -p "$STATE_DIR"

# Два запуска одновременно (таймер и /update) делали бы одно и то же дважды.
exec 9>"$STATE_DIR/update.lock"
flock -n 9 || { log "обновление уже идёт"; exit 0; }

# fail_once KEY TEXT — сообщить о сбое один раз, пока он не пройдёт. Без этого обрыв интернета
# давал бы сообщение каждые 5 минут.
fail_once() {
  local text=$2 flag="$STATE_DIR/failed-$1"
  log "ошибка: $text"
  [[ -e "$flag" ]] && return 0
  touch "$flag"
  notify "$text"
}
clear_fail() {
  local flag="$STATE_DIR/failed-$1"
  if [[ -e "$flag" ]]; then
    rm -f "$flag"
    notify "$2"
  fi
}

# changed_services STACK ENV — сервисы, у которых запущен контейнер и он отличается от нужного:
# другой образ (вышла новая версия, сменился тег) или другая конфигурация (правка compose.yaml).
# Печатает строки «сервис цель», где цель — образ и хэш конфигурации, на которые нужно перейти.
changed_services() {
  local stack=$1 env_file=$2 model svc cid want_image want_id have_id want_hash have_hash
  # Образ берётся из полной модели: config --images с именем сервиса печатает и образы его зависимостей.
  model=$(compose "$stack" "$env_file" config --format json)
  for svc in $(jq -r '.services | keys[]' <<<"$model"); do
    cid=$(compose "$stack" "$env_file" ps -q "$svc")
    [[ -n "$cid" ]] || continue
    want_image=$(jq -r --arg s "$svc" '.services[$s].image' <<<"$model")
    want_id=$(docker image inspect -f '{{.Id}}' "$want_image" 2>/dev/null || true)
    have_id=$(docker inspect -f '{{.Image}}' "$cid")
    want_hash=$(compose "$stack" "$env_file" config --hash "$svc" | awk '{print $2}')
    have_hash=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.config-hash"}}' "$cid")
    if [[ -n "$want_id" && "$want_id" != "$have_id" ]] || [[ "$want_hash" != "$have_hash" ]]; then
      echo "$svc ${want_id#sha256:}-$want_hash"
    fi
  done
}

# has_db SVC — есть ли у сервиса база, которую нужно сохранить до обновления.
has_db() {
  find "$SRV_DIR/$1/data" -maxdepth 1 -name '*.db' 2>/dev/null | grep -q .
}

# verify STACK ENV SVC — работает ли сервис после пересоздания. Печатает итог одной строкой.
verify() {
  local stack=$1 env_file=$2 svc=$3 cid state
  cid=$(compose "$stack" "$env_file" ps -q "$svc")
  if [[ -z "$cid" ]]; then
    echo "не запущен"
    return 1
  fi
  state=$(docker inspect -f '{{.State.Running}} {{.RestartCount}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$cid")
  read -r running restarts health <<<"$state"
  if [[ "$running" != "true" || "$restarts" != "0" || "$health" == "unhealthy" ]]; then
    echo "не поднялся (running=$running, перезапусков $restarts${health:+, health $health})"
    return 1
  fi
  echo "работает${health:+, health $health}"
}

update_stack() {
  local stack=$1 env_file=$2 svc old_id new_id result report=() failed=0 existing=()
  # Скачиваются образы только тех сервисов, у которых уже есть контейнер. Сервис, описанный в compose,
  # но ещё не перенесённый на сервер, может не иметь образа в ghcr, и pull всего стека падал бы.
  mapfile -t existing < <(compose "$stack" "$env_file" ps -a --services)
  [[ ${#existing[@]} -gt 0 ]] || return 0
  if ! compose "$stack" "$env_file" pull --quiet "${existing[@]}" 2>"$STATE_DIR/pull-$stack.err"; then
    fail_once "pull-$stack" "Стек $stack: не удалось скачать образы. $(tail -n 3 "$STATE_DIR/pull-$stack.err")"
    return 0
  fi
  clear_fail "pull-$stack" "Стек $stack: образы снова скачиваются."

  local changes target flag
  changes=$(changed_services "$stack" "$env_file")
  [[ -n "$changes" ]] || return 0

  # Список читается из fd 3: docker и sqlite3 внутри цикла не должны съесть его со stdin.
  while read -r svc target <&3; do
    # Цель, на которой сервис уже не поднялся, повторно не ставится: иначе каждые 5 минут были бы
    # новая копия базы, пересоздание и сообщение. Следующая попытка — с новой сборкой или правкой compose.
    flag="$STATE_DIR/failed-target-$svc"
    if [[ "$(cat "$flag" 2>/dev/null)" == "$target" ]]; then
      log "$svc: эта версия уже не поднялась, пропускаю"
      continue
    fi
    old_id=$(docker inspect -f '{{.Image}}' "$(compose "$stack" "$env_file" ps -q "$svc")")
    if has_db "$svc" && ! "$HOMELAB_DIR/scripts/backup.sh" "$svc" pre-update; then
      report+=("$svc: не обновлён — не удалось сделать копию базы")
      echo "$target" >"$flag"
      failed=1
      continue
    fi
    # --no-deps: пересоздаётся только этот сервис, его зависимости не трогаются.
    if ! compose "$stack" "$env_file" up -d --no-deps "$svc"; then
      report+=("$svc: ошибка docker compose up, см. journalctl -u homelab-update")
      echo "$target" >"$flag"
      failed=1
      continue
    fi
    sleep "$CHECK_DELAY"
    new_id=$(docker inspect -f '{{.Image}}' "$(compose "$stack" "$env_file" ps -q "$svc")" 2>/dev/null || echo "")
    if result=$(verify "$stack" "$env_file" "$svc"); then
      report+=("$svc: $(short_version "$old_id") → $(short_version "${new_id:-$old_id}"), $result")
      rm -f "$flag"
    else
      report+=("$svc: $result
$(docker logs --tail 20 "$svc" 2>&1)")
      echo "$target" >"$flag"
      failed=1
    fi
  done 3<<<"$changes"

  [[ ${#report[@]} -gt 0 ]] || return 0

  local title="Обновление $stack" event=update-ok
  [[ $failed -eq 0 ]] || title="Обновление $stack с ошибками" event=update-fail
  notify "$title
$(printf '%s\n' "${report[@]}")" "$event"
}

main() {
  [[ $EUID -eq 0 ]] || die "нужен root"
  case "$POLICY" in auto|nightly|all) ;; *) die "политика: auto, nightly или all" ;; esac

  if git -C "$HOMELAB_DIR" pull --ff-only --quiet 2>"$STATE_DIR/git.err"; then
    clear_fail git "homelab: git pull снова работает."
  else
    fail_once git "homelab: git pull не прошёл, работаю со старыми compose-файлами. $(tail -n 2 "$STATE_DIR/git.err")"
  fi

  local entry stack policy env_file
  for entry in "${STACKS[@]}"; do
    read -r stack policy env_file <<<"$entry"
    [[ "$POLICY" == "all" || "$POLICY" == "$policy" ]] || continue
    # Без файла переменных compose подставит пустые пути и пересоздаст контейнеры неправильно.
    # Так стек ещё не перенесён в homelab (медиастек до шага 1 переезда) — пропускаем.
    if [[ ! -r "$env_file" ]]; then
      log "стек $stack пропущен: нет $env_file"
      continue
    fi
    update_stack "$stack" "$env_file"
  done

  # Только образы, которые не использует ни один контейнер. Старые версии своих сервисов
  # держим две недели: по ним можно откатиться без сборки.
  docker image prune -af --filter "until=336h" >/dev/null || true
}

main
