#!/usr/bin/env bash
# Что изменится, если сейчас выполнить docker compose up для стека. Ничего не меняет и ничего не скачивает.
#   diff.sh media
# По каждому сервису: ok — контейнер совпадает с compose-файлом и не будет пересоздан;
# config — отличается конфигурация; image — отличается образ; нет контейнера — up его создаст.
# Выход 0 — всё ok, 1 — есть отличия.
set -euo pipefail
# shellcheck source=SCRIPTDIR/lib.sh
source "$(dirname "$(readlink -f "$0")")/lib.sh"

stack=${1:-}
[[ -n "$stack" ]] || die "использование: diff.sh <стек>"
env_file=""
for entry in "${STACKS[@]}"; do
  read -r s _ e <<<"$entry"
  [[ "$s" == "$stack" ]] && env_file=$e
done
[[ -n "$env_file" ]] || die "стека $stack нет в lib.sh"

model=$(compose "$stack" "$env_file" config --format json)
project=$(jq -r '.name' <<<"$model")
differs=0
printf '%-16s %-14s %s\n' СЕРВИС ИТОГ ПОДРОБНО
for svc in $(jq -r '.services | keys[]' <<<"$model"); do
  cid=$(docker ps -aq --filter "label=com.docker.compose.project=$project" \
    --filter "label=com.docker.compose.service=$svc" | head -n 1)
  if [[ -z "$cid" ]]; then
    printf '%-16s %-14s %s\n' "$svc" "нет контейнера" "up создаст новый"
    differs=1
    continue
  fi
  want_hash=$(compose "$stack" "$env_file" config --hash "$svc" | awk '{print $2}')
  have_hash=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.config-hash"}}' "$cid")
  want_image=$(jq -r --arg s "$svc" '.services[$s].image' <<<"$model")
  want_id=$(docker image inspect -f '{{.Id}}' "$want_image" 2>/dev/null || echo "")
  have_id=$(docker inspect -f '{{.Image}}' "$cid")
  result=ok details=""
  if [[ "$want_hash" != "$have_hash" ]]; then
    result=config details="хэш у контейнера ${have_hash:0:12}, по файлу ${want_hash:0:12}"
  fi
  if [[ -n "$want_id" && "$want_id" != "$have_id" ]]; then
    result=${result/ok/}; result=${result:+$result+}image
    details="${details:+$details; }локальный образ $want_image новее запущенного"
  fi
  [[ "$result" == "ok" ]] || differs=1
  printf '%-16s %-14s %s\n' "$svc" "$result" "$details"
done

if [[ $differs -eq 1 ]]; then
  echo
  echo "Есть отличия. Разница конфигурации: docker inspect <контейнер> против"
  echo "docker compose -f stacks/$stack/compose.yaml --env-file $env_file config <сервис>"
  exit 1
fi
