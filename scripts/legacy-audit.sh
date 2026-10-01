#!/usr/bin/env bash
# Опись того, что осталось на сервере от установки до homelab. Только читает, ничего не меняет.
# Запуск: sudo scripts/legacy-audit.sh | tee /mnt/backup/legacy/audit-$(date +%F).txt
# Пустой раздел печатается как «нет», чтобы по выводу было видно: проверено и чисто.
set -uo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "Нужен root: crontab root, /etc и чужие каталоги без него не читаются." >&2
  exit 1
fi

LEGACY_UNITS=(budget-bot.service wellbeing-bot.service nutrition-assistant.service
  monitor-check.service monitor-check.timer monitor-report.service monitor-report.timer)
LEGACY_PATHS=(/opt/budget-bot /opt/wellbeing-bot /opt/monitor
  /usr/local/bin/nutrition-assistant /etc/nutrition-assistant /var/lib/nutrition-assistant
  /etc/budget-bot.env /etc/wellbeing-bot.env /var/backups/budget-bot /var/backups/wellbeing-bot)
LEGACY_USERS=(budgetbot wellbeingbot nutrition-assistant)
# Контейнеры, которые homelab знает. Всё прочее попадёт в раздел «неизвестное».
KNOWN_CONTAINERS='^(ops-bot|llm-gateway|budget-bot|nutrition-assistant|wellbeing-bot|jellyfin|sonarr|radarr|prowlarr|qbittorrent|bazarr|jellyseerr|flaresolverr|notifiarr|watchtower)$'
# Юниты из поставки Ubuntu и Docker не интересны: ищем то, что ставили руками.
KNOWN_UNITS_RE='^(homelab-|docker|containerd|snap|ssh|systemd-|cron|rsyslog|ufw|apparmor|nvidia|getty|networkd|NetworkManager|wpa_supplicant|unattended|apt-|e2scrub|fstrim|logrotate|man-db|motd|dpkg|sysstat|thermald|irqbalance|multipathd|open-iscsi|iscsid|lvm2|udisks2|polkit|ModemManager|accounts-daemon|fwupd|power-profiles|upower|smartmontools|lm-sensors|console-setup|keyboard-setup|setvbuf|blk-availability|finalrd|grub|plymouth|secureboot|cloud-|lxd|pollinate|ua-|ubuntu-|rsync|dmesg|anacron|bluetooth|avahi|cups|kerneloops|switcheroo|gpu-manager|whoopsie|apport|vgauth|open-vm-tools|qemu-guest|wtmp|update-notifier|tpm-udev|systemd)'

section() { printf '\n== %s\n' "$1"; }
none_if_empty() { local out; out=$(cat); if [[ -z "$out" ]]; then echo "нет"; else echo "$out"; fi; }

echo "Опись старой установки, $(hostname), $(date '+%F %T')"

section "systemd: старые юниты (состояние, включён ли)"
for u in "${LEGACY_UNITS[@]}"; do
  if systemctl cat "$u" >/dev/null 2>&1; then
    printf '%-32s active=%-9s enabled=%s\n' "$u" "$(systemctl is-active "$u" 2>/dev/null)" \
      "$(systemctl is-enabled "$u" 2>/dev/null)"
  fi
done | none_if_empty

section "файлы и каталоги"
for p in "${LEGACY_PATHS[@]}"; do
  [[ -e "$p" ]] && printf '%-40s %s\n' "$p" "$(du -sh "$p" 2>/dev/null | cut -f1)"
done | none_if_empty

section "файлы баз в старых каталогах"
find /opt/budget-bot /opt/wellbeing-bot /var/lib/nutrition-assistant -maxdepth 2 -name '*.db' \
  -printf '%p  %s байт  изменён %TY-%Tm-%Td %TH:%TM\n' 2>/dev/null | none_if_empty

section "системные пользователи"
for u in "${LEGACY_USERS[@]}"; do
  id "$u" >/dev/null 2>&1 && echo "$u ($(id "$u"))"
done | none_if_empty

section "crontab root: строки с backup.sh и monitor"
crontab -l -u root 2>/dev/null | grep -nE 'backup\.sh|monitor' | none_if_empty

section "/etc/fstab: строки со старыми службами"
grep -nE 'nutrition-assistant|budget-bot|wellbeing-bot' /etc/fstab | none_if_empty

section "/mnt/backup"
findmnt /mnt/backup 2>/dev/null | none_if_empty

section "logind: крышка ноутбука (должно остаться ignore)"
grep -E '^HandleLidSwitch' /etc/systemd/logind.conf 2>/dev/null | none_if_empty

if command -v docker >/dev/null 2>&1; then
  section "Docker: контейнеры по проектам compose"
  docker ps -a --format '{{.Label "com.docker.compose.project"}}\t{{.Names}}\t{{.Status}}\t{{.Image}}' |
    sort | none_if_empty

  section "Docker: где лежит compose медиастека"
  if docker inspect jellyfin >/dev/null 2>&1; then
    docker inspect jellyfin --format \
      'каталог: {{index .Config.Labels "com.docker.compose.project.working_dir"}}
файлы:   {{index .Config.Labels "com.docker.compose.project.config_files"}}
env:     {{index .Config.Labels "com.docker.compose.project.environment_file"}}'
  else
    echo "контейнера jellyfin нет"
  fi

  section "Docker: watchtower"
  docker ps -a --filter name='^watchtower$' --format '{{.Names}} {{.Status}}' | none_if_empty

  section "Docker: неизвестные контейнеры"
  docker ps -a --format '{{.Names}}' | grep -vE "$KNOWN_CONTAINERS" | none_if_empty
else
  section "Docker"
  echo "docker не установлен"
fi

section "systemd: прочие службы и таймеры, включённые вручную"
systemctl list-unit-files --type=service,timer --state=enabled --no-legend 2>/dev/null |
  awk '{print $1}' | grep -vE "$KNOWN_UNITS_RE" |
  grep -vxF -f <(printf '%s\n' "${LEGACY_UNITS[@]}") | none_if_empty

section "/opt: всё содержимое"
find /opt -mindepth 1 -maxdepth 1 -printf '%M %u %p\n' 2>/dev/null | sort -k3 | none_if_empty
