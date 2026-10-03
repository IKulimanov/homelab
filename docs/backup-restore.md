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

Секреты: каждый день в то же время весь каталог `/srv/secrets` упаковывается и шифруется открытым ключом
`AGE_RECIPIENT` из `/srv/secrets/homelab.env`, файл `/mnt/backup/secrets/secrets-ГГГГ-ММ-ДД.tar.age`, хранятся 30 дней.
Расшифровать можно только закрытым ключом, который лежит у тебя в менеджере паролей, а не на сервере. Без
`AGE_RECIPIENT` копия секретов не делается, в журнал пишется предупреждение.

Копия вручную:

```bash
sudo /opt/homelab/scripts/backup.sh budget-bot manual
sudo /opt/homelab/scripts/backup.sh secrets
```

Список копий для панели (экран «Морозильник») — `/var/lib/homelab/backups.json`, обновляется после каждого `--all`.

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

## Восстановление секретов

Файл зашифрован, копировать его можно куда угодно. На сервере:

```bash
sudo install -m 0644 -o "$USER" /mnt/backup/secrets/secrets-2026-10-01.tar.age ~/
```

На Mac, с закрытым ключом из менеджера паролей в файле `key.txt`:

```bash
scp server:secrets-2026-10-01.tar.age .
age -d -i key.txt secrets-2026-10-01.tar.age | tar -tzf -
age -d -i key.txt secrets-2026-10-01.tar.age > secrets.tar.gz
```

Первая команда `age` только показывает список файлов. Нужный файл — на сервер в `/srv/secrets/`, права `0600`,
владелец `65532:65532`, затем «Применить» в панели или `sudo systemctl start homelab-update-now`. После этого
удалить `key.txt` и `secrets.tar.gz` с Mac.
