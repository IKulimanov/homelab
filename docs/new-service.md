# Новый сервис

Требования к сервису: один процесс, настройки из переменных окружения, данные в `/data`, логи в stdout.
Обращения к Gemini идут по адресу из `GEMINI_BASE_URL`.

## В репозитории сервиса

1. `Dockerfile` — копия `templates/Dockerfile.go`. Если пакет `main` лежит не в `./cmd/bot`, путь задаётся
   параметром `cmd` в workflow.
2. `.github/workflows/build.yml` — копия `templates/build.yml`, `SERVICE_NAME` заменить на имя сервиса.
3. В `main.go`: `var version = "dev"` и версия в строке лога при старте. Сборка подставит туда `sha-…`.
4. `.dockerignore`: `.git`, `.idea`, `.claude`, `build`, `*.db*`, `.env`.
5. Push в `main`, убедиться, что в Actions зелёная сборка и в ghcr появился образ.

## В homelab

1. Блок из `templates/compose-service.yaml` — в `stacks/apps/compose.yaml`.
2. `env/<svc>.env.example` — все переменные с комментариями, без значений секретов.
3. Пустой тег `<SVC>_TAG=` — в `env/homelab.env.example`.
4. Строка в таблице сервисов в `README.md`.
5. Push в `main`.

## На сервере

```bash
sudo /opt/homelab/scripts/install.sh service <svc>
sudo nano /srv/<svc>/.env
cd /opt/homelab && sudo git pull
sudo docker compose -f stacks/apps/compose.yaml --env-file .env up -d <svc>
docker logs -f <svc>
```

Дальше сервис обновляется сам после каждого push в `main`.
