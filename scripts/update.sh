#!/usr/bin/env bash
# Обновление сервисов: забрать новые образы и конфиги, пересоздать только изменившиеся контейнеры.
#   update.sh auto     — стеки с политикой auto (таймер раз в 5 минут)
#   update.sh nightly  — стеки с политикой nightly (таймер в 4:00)
#   update.sh all      — все стеки (команда /update)
# Остановленный сервис не запускается: если его остановили руками, так и задумано.
# Перед пересозданием сервиса с базой снимается копия: миграции новых версий необратимы.
# В стеках с политикой auto новый сервис из compose ставится сам: каталоги, секреты из образца, ключ шлюза;
# запускается, когда заполнены обязательные ключи.
set -euo pipefail
# shellcheck source=SCRIPTDIR/lib.sh
source "$(dirname "$(readlink -f "$0")")/lib.sh"

POLICY=${1:-auto}
CHECK_DELAY=${CHECK_DELAY:-30}
# Запуск по кнопке (панель, /update) ждёт плановый прогон, а не выходит: иначе нажатие потерялось бы.
LOCK_WAIT=${UPDATE_LOCK_WAIT:-0}
HISTORY="$STATE_DIR/history.jsonl"
HISTORY_DAYS=90
mkdir -p "$STATE_DIR"

# Два запуска одновременно (таймер и /update) делали бы одно и то же дважды.
exec 9>"$STATE_DIR/update.lock"
if [[ "$LOCK_WAIT" -gt 0 ]]; then
  flock -w "$LOCK_WAIT" 9 || { log "обновление идёт дольше $LOCK_WAIT с, выхожу"; exit 1; }
else
  flock -n 9 || { log "обновление уже идёт"; exit 0; }
fi

# last-run.json — начало и конец прогона. По нему панель понимает, что «Применить» отработало,
# даже если ничего не изменилось.
RUN_STARTED=$(date +%s)
last_run() {
  jq -cn --argjson started "$RUN_STARTED" --argjson finished "$1" --argjson code "$2" --arg policy "$POLICY" \
    '{started: $started, finished: $finished, code: $code, policy: $policy}' >"$STATE_DIR/last-run.json.tmp" &&
    mv "$STATE_DIR/last-run.json.tmp" "$STATE_DIR/last-run.json"
}
last_run null null
trap 'last_run "$(date +%s)" "$?"' EXIT

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

# history STACK SVC FROM TO RESULT REASON TEXT — строка в журнал доставок для панели.
history() {
  jq -cn --argjson ts "$(date +%s)" --arg stack "$1" --arg svc "$2" --arg from "$3" --arg to "$4" \
    --arg result "$5" --arg reason "$6" --arg text "$7" \
    '{ts: $ts, stack: $stack, svc: $svc, from: $from, to: $to, result: $result, reason: $reason, text: $text}' \
    >>"$HISTORY" || log "журнал доставок не записан"
}

prune_history() {
  [[ -s "$HISTORY" ]] || return 0
  local cut tmp
  cut=$(( $(date +%s) - HISTORY_DAYS * 86400 ))
  tmp=$(mktemp "$HISTORY.XXXXXX")
  if jq -c --argjson cut "$cut" 'select(.ts >= $cut)' "$HISTORY" >"$tmp"; then
    chmod 0644 "$tmp"
    mv "$tmp" "$HISTORY"
  else
    rm -f "$tmp"
  fi
}

# want_hash STACK ENV SVC — хэш конфигурации, который должен быть у контейнера по compose-файлу.
want_hash() {
  compose "$1" "$2" config --hash "$3" | awk '{print $2}'
}

# Цель сервиса — «id образа-хэш конфигурации», которую update.sh поставил или застал последней. Сравнение идёт
# с ней, а не с меткой контейнера: метка com.docker.compose.config-hash и .Image контейнера не всегда совпадают
# с тем, что печатают config --hash и image inspect (зависит от версии compose и хранилища образов). Тогда
# сервис считался бы изменённым на каждом прогоне: копия базы и сообщение «обновлено» каждые 5 минут.
APPLIED_DIR="$STATE_DIR/applied"
applied_get() { cat "$APPLIED_DIR/$1-$2" 2>/dev/null || true; }
applied_set() { mkdir -p "$APPLIED_DIR" && echo "$3" >"$APPLIED_DIR/$1-$2"; }

# changed_services STACK ENV — сервисы, у которых запущен контейнер и он отличается от нужного:
# другой образ (вышла новая версия, сменился тег) или другая конфигурация (правка compose.yaml, секретов).
# Печатает строки «сервис цель причина», где цель — образ и хэш конфигурации, на которые нужно перейти.
changed_services() {
  local stack=$1 env_file=$2 model svc cid want_image want_id have_id want have target applied reason
  # Образ берётся из полной модели: config --images с именем сервиса печатает и образы его зависимостей.
  model=$(compose "$stack" "$env_file" config --format json)
  for svc in $(jq -r '.services | keys[]' <<<"$model"); do
    cid=$(compose "$stack" "$env_file" ps -q "$svc")
    [[ -n "$cid" ]] || continue
    want_image=$(jq -r --arg s "$svc" '.services[$s].image' <<<"$model")
    want_id=$(docker image inspect -f '{{.Id}}' "$want_image" 2>/dev/null || true)
    want_id=${want_id#sha256:}
    want=$(want_hash "$stack" "$env_file" "$svc")
    target="$want_id-$want"
    applied=$(applied_get "$stack" "$svc")
    reason=""
    if [[ -n "$applied" ]]; then
      [[ "$applied" == "$target" ]] && continue
      [[ -n "$want_id" && "${applied%-*}" != "$want_id" ]] && reason=image
      [[ "${applied##*-}" != "$want" ]] && reason=${reason:+$reason+}config
    else
      # Первый прогон после установки: записанной цели ещё нет, сравнение с самим контейнером.
      have_id=$(docker inspect -f '{{.Image}}' "$cid")
      have=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.config-hash"}}' "$cid")
      [[ -n "$want_id" && "$want_id" != "${have_id#sha256:}" ]] && reason=image
      [[ "$want" != "$have" ]] && reason=${reason:+$reason+}config
      [[ -n "$reason" ]] || applied_set "$stack" "$svc" "$target"
    fi
    [[ -z "$reason" ]] || echo "$svc $target $reason"
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

# image_of STACK ENV SVC — id образа запущенного контейнера сервиса, пусто, если контейнера нет.
image_of() {
  local cid
  cid=$(compose "$1" "$2" ps -q "$3")
  [[ -n "$cid" ]] && docker inspect -f '{{.Image}}' "$cid" 2>/dev/null || true
}

update_stack() {
  local stack=$1 env_file=$2 svc old_cid old_id new_id result report=() failed=0 existing=()
  # Скачиваются образы только тех сервисов, у которых уже есть контейнер. Новый сервис может ещё
  # не иметь образа в ghcr, и pull всего стека падал бы; новые скачивает launch_new по одному.
  # Без контейнеров compose печатает пустую строку: без фильтра pull получил бы сервис с пустым именем.
  mapfile -t existing < <(compose "$stack" "$env_file" ps -a --services | grep -v '^$')
  [[ ${#existing[@]} -gt 0 ]] || return 0
  if ! compose "$stack" "$env_file" pull --quiet "${existing[@]}" 2>"$STATE_DIR/pull-$stack.err"; then
    fail_once "pull-$stack" "Стек $stack: не удалось скачать образы. $(tail -n 3 "$STATE_DIR/pull-$stack.err")"
    return 0
  fi
  clear_fail "pull-$stack" "Стек $stack: образы снова скачиваются."

  local changes target reason flag from to
  changes=$(changed_services "$stack" "$env_file")
  [[ -n "$changes" ]] || return 0

  # Список читается из fd 3: docker и sqlite3 внутри цикла не должны съесть его со stdin.
  while read -r svc target reason <&3; do
    # Цель, на которой сервис уже не поднялся, повторно не ставится: иначе каждые 5 минут были бы
    # новая копия базы, пересоздание и сообщение. Следующая попытка — с новой сборкой или правкой compose.
    flag="$STATE_DIR/failed-target-$svc"
    if [[ "$(cat "$flag" 2>/dev/null)" == "$target" ]]; then
      log "$svc: эта версия уже не поднялась, пропускаю"
      continue
    fi
    old_cid=$(compose "$stack" "$env_file" ps -q "$svc")
    old_id=$(image_of "$stack" "$env_file" "$svc")
    from=$(short_version "$old_id")
    if has_db "$svc" && ! "$HOMELAB_DIR/scripts/backup.sh" "$svc" pre-update; then
      report+=("$svc: не обновлён — не удалось сделать копию базы")
      history "$stack" "$svc" "$from" "$from" fail "$reason" "не удалось сделать копию базы"
      echo "$target" >"$flag"
      failed=1
      continue
    fi
    # --no-deps: пересоздаётся только этот сервис, его зависимости не трогаются.
    if ! compose "$stack" "$env_file" up -d --no-deps "$svc"; then
      report+=("$svc: ошибка docker compose up, см. journalctl -u homelab-update")
      history "$stack" "$svc" "$from" "$from" fail "$reason" "ошибка docker compose up"
      echo "$target" >"$flag"
      failed=1
      continue
    fi
    # Контейнер тот же — compose не нашёл, что менять: сервис уже такой, как нужно. Не о чем сообщать.
    if [[ "$(compose "$stack" "$env_file" ps -q "$svc")" == "$old_cid" ]]; then
      log "$svc: compose не пересоздал контейнер ($reason), запоминаю текущее состояние"
      applied_set "$stack" "$svc" "$target"
      continue
    fi
    sleep "$CHECK_DELAY"
    new_id=$(image_of "$stack" "$env_file" "$svc")
    to=$(short_version "${new_id:-$old_id}")
    if result=$(verify "$stack" "$env_file" "$svc"); then
      applied_set "$stack" "$svc" "$target"
      report+=("$svc: $from → $to, $result")
      history "$stack" "$svc" "$from" "$to" ok "$reason" "$result"
      rm -f "$flag"
    else
      report+=("$svc: $result
$(docker logs --tail 20 "$svc" 2>&1)")
      history "$stack" "$svc" "$from" "$to" fail "$reason" "$result"
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

# legacy_unit SVC — сервис ещё работает по-старому, службой systemd с тем же именем. Автоустановка
# подняла бы рядом второй экземпляр с пустой базой и тем же токеном Telegram.
legacy_unit() {
  systemctl is-enabled --quiet "$1.service" 2>/dev/null || systemctl is-active --quiet "$1.service" 2>/dev/null
}

# secrets_hint SVC — где заполнить секреты: ссылка на Сейф в панели, если её адрес известен.
secrets_hint() {
  local url lan
  url=$(env_get PANEL_URL "$HOMELAB_ENV")
  lan=$(env_get LAN_IP "$HOMELAB_ENV")
  # Как в compose: без PANEL_URL панель живёт на LAN_IP:8800.
  [[ -n "$url" || -z "$lan" ]] || url="http://$lan:8800"
  if [[ -n "$url" ]]; then
    echo "${url%/}/vault/$1"
  else
    echo "заполни $(secrets_file "$1")"
  fi
}

# NEW_SERVICES — новые сервисы стека после prepare_new: описаны в compose, контейнера нет вообще.
NEW_SERVICES=()

# prepare_new STACK ENV — подготовить новые сервисы до обновления стека: каталоги, секреты из образца,
# ключ шлюза. Так шлюз с новым ключом пересоздастся в этом же прогоне, вместе с обновлением своего стека.
prepare_new() {
  local stack=$1 env_file=$2 model svc cname other gw_env
  NEW_SERVICES=()
  model=$(compose "$stack" "$env_file" config --format json)
  for svc in $(jq -r '.services | keys[]' <<<"$model"); do
    # Остановленный руками контейнер тоже контейнер: такой сервис не новый и не трогается.
    [[ -z "$(compose "$stack" "$env_file" ps -a -q "$svc")" ]] || continue
    if legacy_unit "$svc"; then
      log "$svc: есть старая служба $svc.service, автоустановка ждёт переезда"
      continue
    fi
    cname=$(jq -r --arg s "$svc" '.services[$s].container_name // empty' <<<"$model")
    if [[ -n "$cname" ]] && other=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$cname" 2>/dev/null); then
      fail_once "name-$svc" "$svc не установлен: уже есть контейнер $cname${other:+ из проекта $other}. Убери его или переименуй сервис."
      continue
    fi
    rm -f "$STATE_DIR/failed-name-$svc"
    # Каталоги и секреты — только тем, кому они нужны: у Dozzle нет ни образца секретов, ни данных.
    if [[ -r "$HOMELAB_DIR/env/$svc.env.example" ]] ||
      jq -e --arg s "$svc" --arg d "$SRV_DIR/$svc/data" '.services[$s].volumes[]? | select(.source == $d)' \
        <<<"$model" >/dev/null; then
      # В подоболочке: die внутри не должен оборвать обновление остальных сервисов.
      if ! (install_service "$svc"); then
        fail_once "install-$svc" "$svc: не удалось подготовить каталоги, см. journalctl -u homelab-update"
        continue
      fi
    fi
    if llm_clients | grep -qx "$svc"; then
      gw_env=$(secrets_file llm-gateway)
      if [[ -w "$gw_env" && -z "$(env_get "$(llm_var "$svc")" "$gw_env")" ]] && ! (llm_key "$svc"); then
        fail_once "install-$svc" "$svc: не удалось выдать ключ шлюза, см. journalctl -u homelab-update"
        continue
      fi
    fi
    rm -f "$STATE_DIR/failed-install-$svc"
    NEW_SERVICES+=("$svc")
  done
}

# gateway_ready — шлюз уже работает с актуальными секретами. Пока он не пересоздан с ключом нового
# сервиса, тот получал бы 401. Шлюза нет вовсе — ждать нечего.
gateway_ready() {
  local cid have
  cid=$(compose platform "$HOMELAB_ENV" ps -q llm-gateway)
  [[ -n "$cid" ]] || return 0
  have=$(applied_get platform llm-gateway)
  have=${have##*-}
  [[ -n "$have" ]] || have=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.config-hash"}}' "$cid")
  [[ "$have" == "$(want_hash platform "$HOMELAB_ENV" llm-gateway)" ]]
}

# launch_new STACK ENV — запустить подготовленные новые сервисы, у которых заполнены обязательные ключи.
launch_new() {
  local stack=$1 env_file=$2 svc missing target flag new_id version result
  for svc in "${NEW_SERVICES[@]}"; do
    mapfile -t missing < <(missing_keys "$svc")
    if [[ ${#missing[@]} -gt 0 ]]; then
      fail_once "wait-$svc" "$svc приехал, жду ${missing[*]}: $(secrets_hint "$svc")"
      continue
    fi
    rm -f "$STATE_DIR/failed-wait-$svc"
    if llm_clients | grep -qx "$svc" && ! gateway_ready; then
      log "$svc: жду, пока шлюз применит его ключ"
      continue
    fi
    # Сборка в ghcr могла ещё не закончиться: только запись в журнал, следующая попытка через 5 минут.
    if ! compose "$stack" "$env_file" pull --quiet "$svc" 2>"$STATE_DIR/pull-$svc.err"; then
      log "$svc: образ не скачался, попробую позже: $(tail -n 1 "$STATE_DIR/pull-$svc.err")"
      continue
    fi
    target="$(docker image inspect -f '{{.Id}}' "$(compose "$stack" "$env_file" config --format json |
      jq -r --arg s "$svc" '.services[$s].image')" 2>/dev/null)"
    target="${target#sha256:}-$(want_hash "$stack" "$env_file" "$svc")"
    flag="$STATE_DIR/failed-target-$svc"
    if [[ "$(cat "$flag" 2>/dev/null)" == "$target" ]]; then
      log "$svc: эта версия уже не поднялась, жду новую сборку или правку"
      continue
    fi
    if ! compose "$stack" "$env_file" up -d --no-deps "$svc"; then
      notify "$svc: установка не удалась, ошибка docker compose up. См. journalctl -u homelab-update" update-fail
      history "$stack" "$svc" "" "" fail install "ошибка docker compose up"
      echo "$target" >"$flag"
      continue
    fi
    sleep "$CHECK_DELAY"
    new_id=$(image_of "$stack" "$env_file" "$svc")
    version=$(short_version "$new_id")
    if result=$(verify "$stack" "$env_file" "$svc"); then
      applied_set "$stack" "$svc" "$target"
      notify "$svc установлен, $version, $result" update-ok
      history "$stack" "$svc" "" "$version" ok install "$result"
      rm -f "$flag"
    else
      notify "$svc: установка не удалась, $result
$(docker logs --tail 20 "$svc" 2>&1)" update-fail
      history "$stack" "$svc" "" "$version" fail install "$result"
      echo "$target" >"$flag"
    fi
  done
}

# check_orphans STACK ENV — контейнеры проекта, которых больше нет в compose. Сами не удаляются:
# --remove-orphans не используется, решение за человеком.
check_orphans() {
  local stack=$1 env_file=$2 model project svc flag orphans=()
  model=$(compose "$stack" "$env_file" config --format json)
  project=$(jq -r '.name' <<<"$model")
  while read -r svc; do
    [[ -n "$svc" ]] || continue
    jq -e --arg s "$svc" '.services | has($s)' <<<"$model" >/dev/null && continue
    orphans+=("$svc")
    fail_once "orphan-$svc" "$svc убран из compose $stack, контейнер остался. Удалить: docker rm -f $svc"
  done < <(docker ps -a --filter "label=com.docker.compose.project=$project" \
    --format '{{.Label "com.docker.compose.service"}}')
  for flag in "$STATE_DIR"/failed-orphan-*; do
    [[ -e "$flag" ]] || continue
    svc=${flag##*/failed-orphan-}
    printf '%s\n' "${orphans[@]}" | grep -qx "$svc" || rm -f "$flag"
  done
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
    # Медиастек сам не ставится: его сервисы переносятся руками, без пересоздания.
    if [[ "$policy" == auto ]]; then
      prepare_new "$stack" "$env_file"
      update_stack "$stack" "$env_file"
      launch_new "$stack" "$env_file"
      check_orphans "$stack" "$env_file"
    else
      update_stack "$stack" "$env_file"
    fi
  done

  prune_history
  # Только образы, которые не использует ни один контейнер. Старые версии своих сервисов
  # держим две недели: по ним можно откатиться без сборки.
  docker image prune -af --filter "until=336h" >/dev/null || true
}

main
