# Медиастек

Jellyfin с автоматической загрузкой: qBittorrent качает, Prowlarr ищет по трекерам, Sonarr и Radarr управляют
сериалами и фильмами, Bazarr подбирает субтитры, Jellyseerr принимает запросы, Notifiarr пишет о загрузках
в Telegram. Compose-проект `media-stack`, файл `stacks/media/compose.yaml`, переменные `/srv/media/.env`.

Обновление — `update.sh nightly` в 4:00, образы `:latest` от linuxserver и авторов приложений.

## Адреса

| Сервис | Порт | Зачем |
|---|---|---|
| Jellyfin | 8096 | просмотр; 8920 — https, 7359/udp — поиск в сети, 1900/udp — DLNA |
| Jellyseerr | 5055 | запросы фильмов и сериалов |
| qBittorrent | 8080 | загрузки; 6881 tcp/udp — торрент-трафик |
| Prowlarr | 9696 | трекеры |
| Sonarr | 8989 | сериалы |
| Radarr | 7878 | фильмы |
| Bazarr | 6767 | субтитры |
| FlareSolverr | 8191 | обход Cloudflare для трекеров |
| Notifiarr | 5454 | уведомления о загрузках |

Адрес в сети: `http://<IP сервера>:<порт>`, IP — `ip -4 addr show | grep inet`.

## Каталоги и hardlink

```
/data                   DATA_ROOT, один раздел диска
├── torrents/movies     qBittorrent, категория radarr
├── torrents/tv         qBittorrent, категория sonarr
├── media/movies        Radarr, hardlink из torrents/movies
├── media/tv            Sonarr, hardlink из torrents/tv
└── configs/<сервис>    CONFIG_ROOT, настройки и базы приложений
```

Sonarr и Radarr переносят скачанное в медиатеку через hardlink: файл не копируется и занимает место один раз.
Это работает при двух условиях: `torrents` и `media` лежат на одном разделе, и Sonarr с Radarr монтируют
`/data` целиком, одним томом. Поэтому тома в compose устроены так:

| Контейнер | Том | Внутри |
|---|---|---|
| qBittorrent | `${DATA_ROOT}/torrents` | `/data/torrents` |
| Sonarr, Radarr | `${DATA_ROOT}` | `/data` |
| Bazarr | `${DATA_ROOT}/media` | `/data/media` |
| Jellyfin | `${DATA_ROOT}/media` | `/data/media`, только чтение |

Проверка: у файла в `torrents` и в `media` один inode.

```bash
ls -li /data/torrents/movies/<фильм>/ /data/media/movies/<фильм>/
df /data/torrents /data/media        # одно устройство
```

## GPU

Jellyfin транскодирует на NVIDIA (NVENC/NVDEC). Нужны драйвер и `nvidia-container-toolkit`. Проверка, что
контейнеры видят карту:

```bash
docker exec jellyfin nvidia-smi
```

В Jellyfin: Панель управления → Воспроизведение → Транскодирование → NVENC, включить NVDEC и нужные кодеки.

Обновление драйвера или toolkit требует перезапуска Docker, а значит, всех контейнеров. Его делают только
в выбранное время, не во время переезда.

## Настройка с нуля

Порядок: qBittorrent → Prowlarr → Radarr, Sonarr → Bazarr → Jellyfin → Jellyseerr → Notifiarr.

- **qBittorrent.** Временный пароль: `docker logs qbittorrent 2>&1 | grep "temporary password"`. Загрузки:
  Default Torrent Management Mode — Automatic, путь `/data/torrents`. Категории `radarr` → `/data/torrents/movies`,
  `sonarr` → `/data/torrents/tv`.
- **Prowlarr.** Indexers — трекеры. Settings → Apps: Radarr `http://radarr:7878`, Sonarr `http://sonarr:8989`
  с их API-ключами; Prowlarr сам раздаёт трекеры в Radarr и Sonarr.
- **Radarr, Sonarr.** Root Folder `/data/media/movies` и `/data/media/tv`. Media Management → Show Advanced →
  Use Hardlinks instead of Copy. Download Client: qBittorrent, хост `qbittorrent`, порт 8080, категория
  `radarr` или `sonarr`.
- **Bazarr.** Подключить Sonarr и Radarr по API-ключам, выбрать языки и источники субтитров.
- **Jellyfin.** Библиотеки: Movies — `/data/media/movies`, Shows — `/data/media/tv`. Remote Access: разрешить
  подключения, автоматический проброс портов выключить.
- **Jellyseerr.** Вход через Jellyfin `http://jellyfin:8096`, синхронизировать библиотеки, добавить Radarr и Sonarr.
- **Notifiarr.** Нужен аккаунт на notifiarr.com: оттуда API-ключ и настройка Telegram. Образец конфига —
  `stacks/media/notifiarr.conf`, рабочий лежит в `/data/configs/notifiarr/notifiarr.conf`. В Sonarr и Radarr:
  Settings → Connect → Notifiarr.

## Копии настроек

Медиафайлы не копируются. Настройки приложений — одним архивом:

```bash
sudo tar -czf /mnt/backup/media-configs-$(date +%F).tar.gz -C /data configs
```

## Неполадки

- **Файлы копируются, а не связываются.** `df /data/torrents /data/media` показывает разные устройства — значит,
  каталоги на разных разделах. Hardlink между разделами невозможен.
- **Контейнер не стартует.** `docker logs <контейнер>`, `docker inspect -f '{{json .State.Health}}' <контейнер>`.
  Sonarr и Radarr ждут, пока qBittorrent и Prowlarr станут healthy.
- **Нет прав на файлы.** `PUID` и `PGID` в `/srv/media/.env` должны совпадать с владельцем `/data`.
