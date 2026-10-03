# Новый сервис

Требования к сервису: один процесс, настройки из переменных окружения, данные в `/data`, логи в stdout.
Обращения к Gemini идут по адресу из `GEMINI_BASE_URL`. Всё это уже есть в шаблоне
[go-service-template](https://github.com/IKulimanov/go-service-template).

## 1. Репозиторий из шаблона

1. На GitHub в `go-service-template`: «Use this template», имя репозитория — имя сервиса, например `habit-bot`.
   Имя образа в ghcr берётся из имени репозитория.
2. Клонировать рядом с homelab: `~/dev/personal/habit-bot`.
3. Дописать логику. Переменные описать в `deploy/env.example`; над обязательной в комментарии — слово «Обязателен».
4. Push в `main`. В Actions зелёная сборка, в ghcr появился образ.

Сервис не из шаблона подходит, если в нём есть `Dockerfile` (копия `templates/Dockerfile`),
`.github/workflows/build.yml` (копия `templates/build.yml`, `SERVICE_NAME` заменить на имя) и `var version` в `main`.

## 2. homelab

```bash
scripts/new-service.sh habit-bot --llm 1
```

Скрипт дописывает блок сервиса в `stacks/apps/compose.yaml` и создаёт `env/habit-bot.env.example` из
`../habit-bot/deploy/env.example`. С `--llm` он добавляет клиента в `clients` шлюза с месячным лимитом в долларах.
Без `--llm` сервис в Gemini через шлюз не ходит. Дальше — строка в таблице сервисов `README.md`, commit и push в `main`.

## 3. Сервер

Руками ничего делать не нужно. В течение 5 минут `update.sh`:
1. создаёт `/srv/habit-bot/data` и `/srv/secrets/habit-bot.env` из образца;
2. если сервис есть в `clients`, выдаёт ему ключ шлюза и пересоздаёт шлюз;
3. если обязательные ключи пусты, присылает в служебный бот «habit-bot приехал, жду BOT_TOKEN» со ссылкой на Сейф в
   панели. Вписать значения и нажать «Применить»;
4. скачивает образ, запускает контейнер, через 30 секунд проверяет его и присылает «habit-bot установлен, sha-…».

Если образа в ghcr ещё нет (сборка не закончилась), `update.sh` попробует снова через 5 минут, без сообщений.
Если контейнер не поднялся, придёт сообщение с концом лога; следующая попытка — с новой сборкой или правкой секретов.

Автоустановка не трогает сервис, у которого на сервере есть служба systemd с тем же именем (`habit-bot.service`):
так при переезде старого бота не запустится второй экземпляр с пустой базой. Такой сервис переносится руками
по [migration.md](migration.md).

Ручной путь остаётся: `sudo /opt/homelab/scripts/install.sh service <svc>` и `install.sh llm-key <svc>`.

## Убрать сервис

Удалить блок из `stacks/apps/compose.yaml` и push. `update.sh` контейнер не удаляет, а присылает
«habit-bot убран из compose, контейнер остался». Дальше на сервере:

```bash
sudo docker rm -f habit-bot
```

Данные в `/srv/habit-bot/data` и секреты в `/srv/secrets/habit-bot.env` остаются, удалить их — отдельное решение.
