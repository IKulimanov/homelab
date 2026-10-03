#!/usr/bin/env bash
# Добавить новый сервис в homelab. Запускается на Mac в репозитории homelab, сервер не трогает.
#   scripts/new-service.sh <svc> [--llm <месячный лимит в долларах>]
# Дописывает блок сервиса в stacks/apps/compose.yaml, создаёт env/<svc>.env.example (из ../<svc>/deploy/env.example,
# если репозиторий сервиса лежит рядом), с --llm добавляет клиента шлюза. Дальше — commit и push: сервер
# сам создаст каталоги и секреты и попросит заполнить обязательные ключи.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
log() { printf '%s\n' "$*"; }
die() { printf 'ошибка: %s\n' "$*" >&2; exit 1; }

usage() { die "использование: scripts/new-service.sh <svc> [--llm <лимит>]"; }

svc=${1:-}
[[ -n "$svc" ]] || usage
shift
limit=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --llm) limit=${2:-}; [[ -n "$limit" ]] || usage; shift 2 ;;
    *) usage ;;
  esac
done

[[ "$svc" =~ ^[a-z][a-z0-9-]*$ ]] || die "имя сервиса: строчные латинские буквы, цифры, дефис"
[[ -z "$limit" || "$limit" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "лимит — число долларов, например 2 или 0.5"

compose="$ROOT/stacks/apps/compose.yaml"
example="$ROOT/env/$svc.env.example"
gateway="$ROOT/stacks/platform/config/llm-gateway.yaml"

grep -q "^  $svc:" "$compose" && die "$svc уже есть в stacks/apps/compose.yaml"
grep -rqs "container_name: $svc\$" "$ROOT"/stacks/*/compose.yaml && die "container_name $svc уже занят в другом стеке"

# Блок сервиса — перед секцией networks в конце файла.
tag="$(tr 'a-z-' 'A-Z_' <<<"$svc")_TAG"
block=$(sed -e '/^#/d' -e "s/SERVICE_NAME/$svc/g" -e "s/SERVICE_TAG/$tag/g" "$ROOT/templates/compose-service.yaml")
BLOCK=$block awk '/^networks:/ && !done { print substr(ENVIRON["BLOCK"], 2); print ""; done = 1 } { print }' \
  "$compose" >"$compose.tmp"
grep -q "^  $svc:" "$compose.tmp" || { rm -f "$compose.tmp"; die "не нашёл секцию networks в $compose"; }
mv "$compose.tmp" "$compose"
log "stacks/apps/compose.yaml: добавлен $svc"

# Образец секретов: из репозитория сервиса, иначе из шаблона.
if [[ -e "$example" ]]; then
  log "env/$svc.env.example уже есть, не трогаю"
else
  src=""
  for f in "$ROOT/../$svc/deploy/env.example" "$ROOT/../go-service-template/deploy/env.example"; do
    [[ -r "$f" ]] && { src=$f; break; }
  done
  if [[ -n "$src" ]]; then
    sed "s/SERVICE_NAME/$svc/g" "$src" >"$example"
    log "env/$svc.env.example: из $src"
  else
    cat >"$example" <<EOF
# /srv/secrets/$svc.env — секреты и настройки $svc. Права 0600, правится в панели («Сейф»).

# Токен из BotFather. Обязателен.
BOT_TOKEN=
EOF
    log "env/$svc.env.example: минимальный образец, допиши переменные сервиса"
  fi
fi
grep -q "Обязателен" "$example" ||
  log "внимание: в env/$svc.env.example нет ни одной переменной с пометкой «Обязателен» — сервис запустится сразу"

# Клиент шлюза — в конец секции clients, перед следующим ключом верхнего уровня и его комментариями.
if [[ -n "$limit" ]]; then
  if awk '/^clients:/ { c = 1; next } /^[^ #]/ { c = 0 } c' "$gateway" | grep -q "^  $svc:"; then
    log "$svc уже есть в clients шлюза"
  else
    SVC=$svc LIMIT=$limit awk '
      { line[NR] = $0 }
      END {
        for (i = 1; i <= NR; i++) if (line[i] ~ /^clients:/) start = i
        if (!start) exit 1
        end = NR + 1
        for (i = start + 1; i <= NR; i++) if (line[i] ~ /^[^ #]/) { end = i; break }
        at = end - 1
        while (at > start && (line[at] ~ /^[[:space:]]*$/ || line[at] ~ /^#/)) at--
        for (i = 1; i <= NR; i++) {
          print line[i]
          if (i == at) { print "  " ENVIRON["SVC"] ":"; print "    monthly_limit_usd: " ENVIRON["LIMIT"] }
        }
      }' "$gateway" >"$gateway.tmp" || { rm -f "$gateway.tmp"; die "не нашёл clients в $gateway"; }
    mv "$gateway.tmp" "$gateway"
    log "llm-gateway.yaml: клиент $svc, лимит \$$limit в месяц. Проверь total_monthly_limit_usd"
  fi
fi

cat <<EOF

Осталось:
1. Строка в таблице сервисов README.md.
2. git add -A && git commit -m "feat(apps): добавить $svc" && git push
3. Через 5 минут служебный бот пришлёт ссылку на Сейф: вписать обязательные ключи и нажать «Применить».
EOF
