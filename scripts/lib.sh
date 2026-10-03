# Общие настройки и функции скриптов homelab. Подключается через source, сам не запускается.
# shellcheck shell=bash

HOMELAB_DIR=${HOMELAB_DIR:-/opt/homelab}
SRV_DIR=${SRV_DIR:-/srv}
# Все секреты в одном каталоге: <имя>.env. Владелец — пользователь панели (65532), root читает всё.
SECRETS_DIR=${SECRETS_DIR:-$SRV_DIR/secrets}
HOMELAB_ENV=${HOMELAB_ENV:-$SECRETS_DIR/homelab.env}
# Журнал обновлений, сводка копий и итог последнего прогона. Читает панель, поэтому каталог открыт на чтение.
STATE_DIR=${STATE_DIR:-/var/lib/homelab}
# Пользователь nonroot в образах distroless: от него работают свои сервисы и панель.
APP_UID=65532
GATEWAY_URL=http://llm-gateway:8080

# Стеки: каталог в stacks/, политика обновления, файл переменных для подстановки в compose.
# auto — проверка раз в 5 минут, nightly — в 4:00. Политика задаётся здесь, а не меткой в compose:
# новая метка у сервиса медиастека пересоздала бы его контейнер.
# shellcheck disable=SC2034 # используется в скриптах, которые подключают lib.sh
STACKS=(
  "platform auto $HOMELAB_ENV"
  "apps auto $HOMELAB_ENV"
  "media nightly $SECRETS_DIR/media.env"
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
  # От владельца базы: sqlite3 от root может оставить файл -shm, который ops-bot потом не откроет на запись.
  setpriv --reuid="$APP_UID" --regid="$APP_UID" --clear-groups \
    sqlite3 -init /dev/null -readonly "$OPS_DB" "SELECT file_id FROM gifs WHERE event = '$1' ORDER BY random() LIMIT 1" 2>/dev/null || true
}

# notify TEXT [EVENT] — сообщение служебному боту, с фразой и GIF события, если они есть. Без токена только
# пишет в журнал: обновление не должно падать из-за уведомления.
notify() {
  local text=$1 event=${2:-} token chat line gif="" api
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

# secrets_file NAME — файл секретов сервиса или стека.
secrets_file() { echo "$SECRETS_DIR/$1.env"; }

# env_set KEY VALUE FILE — записать значение ключа, заменив прежнее. Комментарии и порядок строк сохраняются,
# владелец и права файла тоже: панель (65532) должна и дальше его править.
env_set() {
  local key=$1 value=$2 file=$3 tmp
  [[ "$value" != *$'\n'* ]] || die "env_set $key: перевод строки в значении"
  # compose подставляет $ в значениях env_file; в одинарных кавычках значение берётся как есть.
  if [[ "$value" == *'$'* ]]; then
    [[ "$value" != *"'"* ]] || die "env_set $key: в значении одновременно \$ и одинарная кавычка"
    value="'$value'"
  fi
  tmp=$(mktemp "$file.XXXXXX")
  KEY=$key VALUE=$value awk '
    BEGIN { k = ENVIRON["KEY"]; v = ENVIRON["VALUE"] }
    index($0, k "=") == 1 { if (!done) print k "=" v; done = 1; next }
    { print }
    END { if (!done) print k "=" v }
  ' "$file" >"$tmp"
  chown --reference="$file" "$tmp"
  chmod --reference="$file" "$tmp"
  mv "$tmp" "$file"
}

# env_default KEY VALUE FILE — записать значение, только если ключ пустой. Заданное руками не трогается.
env_default() {
  [[ -n "$(env_get "$1" "$3")" ]] || env_set "$@"
}

# required_keys EXAMPLE — обязательные ключи образца: в комментарии над ними есть слово «Обязателен».
# Комментарий относится ко всем переменным сразу под ним, до пустой строки. Тот же разбор — internal/envfile.
required_keys() {
  [[ -r "$1" ]] || return 0
  awk '
    /^[[:space:]]*$/ { req = 0; prev = ""; next }
    /^#/ { if (prev != "comment") req = 0; if ($0 ~ /Обязателен/) req = 1; prev = "comment"; next }
    /^[A-Za-z_][A-Za-z0-9_]*=/ { if (req) { k = $0; sub(/=.*/, "", k); print k }; prev = "var"; next }
  ' "$1"
}

# missing_keys SVC — обязательные ключи, которые пусты: в секретах сервиса и в общих настройках homelab.
missing_keys() {
  local svc=$1 key
  for key in $(required_keys "$HOMELAB_DIR/env/homelab.env.example"); do
    [[ -n "$(env_get "$key" "$HOMELAB_ENV")" ]] || echo "$key"
  done
  for key in $(required_keys "$HOMELAB_DIR/env/$svc.env.example"); do
    [[ -n "$(env_get "$key" "$(secrets_file "$svc")")" ]] || echo "$key"
  done
}

# llm_clients — сервисы из clients в конфиге шлюза: только им шлюз выдаёт ответы.
llm_clients() {
  awk '/^clients:/ { c = 1; next } /^[^ #]/ { c = 0 } c && /^  [a-z0-9-]+:/ { sub(/^  /, ""); sub(/:.*/, ""); print }' \
    "$HOMELAB_DIR/stacks/platform/config/llm-gateway.yaml"
}

# llm_var SVC — имя переменной ключа сервиса в секретах шлюза.
llm_var() { echo "LLM_KEY_$(tr 'a-z-' 'A-Z_' <<<"$1")"; }

# install_secrets_dir — каталог секретов. Владелец — пользователь панели, иначе она не сможет их править.
install_secrets_dir() {
  install -d -m 0700 -o "$APP_UID" -g "$APP_UID" "$SECRETS_DIR"
}

# install_service SVC — каталог данных и файл секретов из образца. Существующее не перезаписывается.
install_service() {
  local svc=$1 example="$HOMELAB_DIR/env/$1.env.example" env_file
  [[ "$svc" =~ ^[a-z0-9-]+$ ]] || die "имя сервиса: строчные буквы, цифры, дефис"
  env_file=$(secrets_file "$svc")
  install_secrets_dir
  install -d -m 0755 "$SRV_DIR/$svc"
  install -d -m 0700 -o "$APP_UID" -g "$APP_UID" "$SRV_DIR/$svc/data"
  if [[ ! -e "$env_file" ]]; then
    if [[ -r "$example" ]]; then
      install -m 0600 -o "$APP_UID" -g "$APP_UID" "$example" "$env_file"
    else
      install -m 0600 -o "$APP_UID" -g "$APP_UID" /dev/null "$env_file"
    fi
    log "создан $env_file — заполни его в панели или руками"
  fi

  case "$svc" in
    ops-bot)
      # Сюда ops-bot кладёт файл-триггер команды /update, его ждёт homelab-update.path.
      install -d -m 0700 -o "$APP_UID" -g "$APP_UID" "$SRV_DIR/ops-bot/trigger"
      ;;
    panel)
      # Файлы-триггеры панели: apply (применить секреты, update.sh auto) и backup (копия баз сейчас).
      install -d -m 0700 -o "$APP_UID" -g "$APP_UID" "$SRV_DIR/panel/trigger"
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
  local svc=$1 var key gw_env svc_env
  gw_env=$(secrets_file llm-gateway)
  svc_env=$(secrets_file "$svc")
  [[ -w "$gw_env" ]] || die "нет $gw_env: сначала install.sh"
  [[ -w "$svc_env" ]] || die "нет $svc_env: сначала install.sh service $svc"
  llm_clients | grep -qx "$svc" ||
    die "$svc нет в clients в stacks/platform/config/llm-gateway.yaml: шлюз ответит 403"

  var=$(llm_var "$svc")
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
