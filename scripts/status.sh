#!/usr/bin/env bash
# Сводка по сервисам homelab: стек, состояние, health, версия, когда запущен.
# Root не нужен, достаточно доступа к docker.
set -euo pipefail
# shellcheck source=SCRIPTDIR/lib.sh
source "$(dirname "$(readlink -f "$0")")/lib.sh"

printf '%-20s %-12s %-10s %-10s %-16s %s\n' СЕРВИС СТЕК СОСТОЯНИЕ HEALTH ВЕРСИЯ ЗАПУЩЕН
for entry in "${STACKS[@]}"; do
  read -r stack _ _ <<<"$entry"
  project=$(sed -n 's/^name: *//p' "$HOMELAB_DIR/stacks/$stack/compose.yaml" | head -n 1)
  project=${project:-$stack}
  docker ps -a --filter "label=com.docker.compose.project=$project" --format '{{.ID}}' |
    while read -r cid; do
      IFS='|' read -r name state health image started < <(docker inspect -f \
        '{{.Name}}|{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}|{{.Image}}|{{.State.StartedAt}}' "$cid")
      printf '%-20s %-12s %-10s %-10s %-16s %s\n' "${name#/}" "$stack" "$state" "$health" \
        "$(short_version "$image")" "$(date -d "$started" '+%F %R' 2>/dev/null || echo "$started")"
    done | sort
done
