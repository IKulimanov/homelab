# Бэкап и восстановление

## Что копируется

`scripts/backup.sh` копирует все `*.db` из `/srv/<svc>/data` в `/mnt/backup/<svc>/`:
- каждый день в 3:30 (`homelab-backup.timer`), имя `<база>-ГГГГ-ММ-ДД.db.gz`, хранятся 30 дней;
- перед каждым обновлением сервиса, имя `<база>-ГГГГ-ММ-ДД-pre-update-ЧЧММСС.db.gz`.

Копия снимается через `sqlite3 .backup` от имени владельца базы и проверяется `PRAGMA quick_check`. Если
`/mnt/backup` не смонтирован, копия не делается и приходит сообщение: копия на том же диске не спасает от
потери диска.

Медиастек сюда не входит. Его конфиги лежат в `/data/configs`, а Sonarr, Radarr и Prowlarr делают свои копии сами
(Settings → General → Backup).

Копия вручную:

```bash
sudo /opt/homelab/scripts/backup.sh budget-bot manual
```

## Восстановление

```bash
ls -lt /mnt/backup/budget-bot/ | head
docker stop budget-bot
sudo mv /srv/budget-bot/data/budget.db /srv/budget-bot/data/budget.db.broken-$(date +%F)
sudo rm -f /srv/budget-bot/data/budget.db-wal /srv/budget-bot/data/budget.db-shm
gunzip -c /mnt/backup/budget-bot/budget-2026-10-01.db.gz | sudo tee /srv/budget-bot/data/budget.db >/dev/null
sudo chown 65532:65532 /srv/budget-bot/data/budget.db && sudo chmod 600 /srv/budget-bot/data/budget.db
docker start budget-bot
docker logs -f budget-bot
```

Файлы `-wal` и `-shm` удаляются: они относятся к прежней базе. Если их оставить, SQLite применит их к
восстановленной копии.

Если вместе с базой откатывается и версия программы, сначала закрепить тег ([update-rollback.md](update-rollback.md)),
потом восстанавливать базу.
