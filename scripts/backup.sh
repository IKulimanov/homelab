#!/usr/bin/env bash
# Копия баз SQLite сервиса: все *.db из /srv/<svc>/data.
#   backup.sh <svc> [метка]   — один сервис; метка попадает в имя файла (pre-update, manual)
#   backup.sh --all           — все сервисы, у которых есть базы; так запускает homelab-backup.timer
# Копия снимается через sqlite3 .backup: простое копирование файла в режиме WAL даёт битый снимок.
set -euo pipefail
# shellcheck source=SCRIPTDIR/lib.sh
source "$(dirname "$(readlink -f "$0")")/lib.sh"

BACKUP_ROOT=$(env_get BACKUP_ROOT "$HOMELAB_ENV"); BACKUP_ROOT=${BACKUP_ROOT:-/mnt/backup}
KEEP_DAYS=$(env_get BACKUP_KEEP_DAYS "$HOMELAB_ENV"); KEEP_DAYS=${KEEP_DAYS:-30}
# Копия рядом с базой не спасает от потери диска, поэтому по умолчанию без смонтированного тома копий не делаем.
REQUIRE_MOUNT=$(env_get BACKUP_REQUIRE_MOUNT "$HOMELAB_ENV"); REQUIRE_MOUNT=${REQUIRE_MOUNT:-yes}

# В базах личные данные: копии доступны только root.
umask 077

backup_db() {
  local svc=$1 db=$2 tag=$3 dest name out owner tmp check
  dest="$BACKUP_ROOT/$svc"
  name=$(basename "$db" .db)
  out="$dest/$name-$(date +%Y-%m-%d)${tag:+-$tag-$(date +%H%M%S)}.db"
  mkdir -p "$dest"

  # sqlite3 запускается от владельца базы: если root создаст файлы -wal и -shm, служба потом не откроет базу.
  owner=$(stat -c '%u:%g' "$db")
  tmp=$(mktemp -d)
  chown "$owner" "$tmp"
  setpriv --reuid="${owner%:*}" --regid="${owner#*:}" --clear-groups \
    sqlite3 "$db" ".backup '$tmp/copy.db'" || { rm -rf "$tmp"; die "$svc: sqlite3 .backup для $name не сработал"; }

  check=$(sqlite3 "$tmp/copy.db" 'PRAGMA quick_check;')
  if [[ "$check" != "ok" ]]; then
    rm -rf "$tmp"
    die "$svc: копия $name не прошла проверку: $check"
  fi
  mv "$tmp/copy.db" "$out"
  rm -rf "$tmp"
  gzip -f "$out"
  log "$svc: копия $out.gz"
}

backup_service() {
  local svc=$1 tag=${2:-} data="$SRV_DIR/$1/data" found=0 db
  [[ -d "$data" ]] || die "$svc: нет каталога $data"
  while IFS= read -r -d '' db; do
    backup_db "$svc" "$db" "$tag"
    found=1
  done < <(find "$data" -maxdepth 1 -name '*.db' -print0)
  [[ $found -eq 1 ]] || log "$svc: баз в $data нет, копировать нечего"
}

# prune_service SVC — удалить старые копии. Только файлы с именами наших баз: в каталоге могут лежать копии,
# которые сервис делает сам, а /mnt/backup/legacy не трогается вовсе.
prune_service() {
  local svc=$1 db name
  while IFS= read -r -d '' db; do
    name=$(basename "$db" .db)
    find "$BACKUP_ROOT/$svc" -maxdepth 1 -name "$name-20??-??-??*.db.gz" -mtime "+$KEEP_DAYS" -delete
  done < <(find "$SRV_DIR/$svc/data" -maxdepth 1 -name '*.db' -print0)
}

main() {
  [[ $EUID -eq 0 ]] || die "нужен root"
  [[ $# -ge 1 ]] || die "использование: backup.sh <svc> [метка] | --all"
  if [[ "$REQUIRE_MOUNT" == "yes" ]] && ! mountpoint -q "$BACKUP_ROOT"; then
    notify "Бэкап не сделан: $BACKUP_ROOT не смонтирован."
    die "$BACKUP_ROOT не смонтирован"
  fi

  if [[ "$1" == "--all" ]]; then
    local failed=() dir svc
    for dir in "$SRV_DIR"/*/data; do
      svc=$(basename "$(dirname "$dir")")
      find "$dir" -maxdepth 1 -name '*.db' | grep -q . || continue
      # Сбой одного сервиса не должен оставить без копии остальные.
      if ( backup_service "$svc" ); then
        prune_service "$svc"
      else
        failed+=("$svc")
      fi
    done
    if [[ ${#failed[@]} -gt 0 ]]; then
      notify "Бэкап с ошибками: ${failed[*]}. Подробности: journalctl -u homelab-backup"
      exit 1
    fi
  else
    backup_service "$1" "${2:-}"
  fi
}

main "$@"
