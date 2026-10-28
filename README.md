# Zen Bridge

Небольшой Go-шлюз к OpenCode: прозрачная труба с OpenAI-совместимым API, несколькими upstream-ключами и Responses API. Шлюз не ограничивает размеры, время и параллелизм, не проверяет схемы запросов и не пересобирает стрим. Только стандартная библиотека Go; в проде работает один статический контейнер.

## Как работает

```
клиент (IDE) → /v1/chat/completions или /v1/responses
             → локальный Bearer client_token
             → пул аккаунтов (round-robin, без лимитов и cooldown)
             → https://opencode.ai/inference/openai/v1
```

Шлюз не хранит историю, не выполняет инструменты и не преобразует Chat ↔ Responses: клиент сам выбирает endpoint и передаёт полный контекст. `config.json` монтируется только для чтения, единственная запись — кэш каталога `/data/catalog.json`.

Запрос и ответ идут без изменений: `stream` передаётся как есть, SSE-байты и ошибки upstream возвращаются клиенту один в один. Модели вне списка аккаунта тоже маршрутизируются в любой доступный аккаунт, поэтому новые модели можно использовать сразу. Параметры reasoning (`reasoning_effort`, `reasoning`) шлюз не добавляет и не переписывает.

## Установка

### Готовый образ

```sh
cp config.example.json config.json
cp .env.example .env
# задайте client_token и добавьте реальные ключи в api_keys
docker pull ghcr.io/revellison/zen-bridge-go:latest
docker compose up -d
curl -i http://127.0.0.1:18788/healthz
curl -i http://127.0.0.1:18788/readyz
```

Compose использует готовый образ из GHCR и ничего не собирает локально. Для обновления повторите `docker pull ghcr.io/revellison/zen-bridge-go:latest` и `docker compose up -d`.

### Из исходников

```sh
git clone https://github.com/Revellison/zen-bridge-go.git
cd zen-bridge-go
cp config.example.json config.json
cp .env.example .env
# задайте client_token и добавьте реальные ключи в api_keys
docker build -t ghcr.io/revellison/zen-bridge-go:latest .
docker compose up -d
```

Локальная сборка использует тот же тег, что и готовый образ, поэтому после неё `docker pull` снова переключит Compose на опубликованную версию.

### Порты

Адрес процесса задаётся только переменной окружения `ZEN_BRIDGE_LISTEN`, поля `listen` в `config.json` нет. По умолчанию используется `127.0.0.1:18788`, поэтому локальный запуск доступен только с этой машины:

```sh
ZEN_BRIDGE_LISTEN=127.0.0.1:19000 ./zen-bridge serve --config config.json
```

В Docker Compose контейнер обязан слушать все интерфейсы, поэтому compose задаёт `ZEN_BRIDGE_LISTEN=0.0.0.0:18788`. Наружу порт публикуется через `BRIDGE_PORT` из `.env`:

```sh
# .env
BRIDGE_PORT=19000
```

По умолчанию публикуется `127.0.0.1:18788`. Значение `BRIDGE_PORT=0` попросит Docker выбрать свободный порт; узнать его можно командой `docker compose port bridge 18788`. Внутренний порт контейнера фиксирован в compose и healthcheck; для его смены нужно править compose, а не `config.json`. Старые конфиги с полем `listen` отклоняются strict-парсером — удалите это поле.

`/readyz` возвращает 503, пока нет включённого аккаунта. Локальный ключ доступа и upstream-ключ — разные секреты:

| Секрет | Поле | Назначение |
|---|---|---|
| Ключ клиента | `client_token` | Bearer-токен, которым IDE обращается к шлюзу. 8–256 символов без пробелов, обязателен. |
| Ключ OpenCode | `api_keys` | Upstream-ключ. Каждый получает стабильный ID `key_<hash>`. |

`config.json` содержит оба секрета в открытом виде: не коммитьте его и не публикуйте порт наружу без туннеля или VPN.

## Настройки

### Корневые поля

| Поле | По умолчанию | Назначение |
|---|---|---|
| `log_level` | `info` | `debug`, `info`, `warn` или `error`. Применяется после рестарта. |
| `client_token` | — | Локальный Bearer-токен клиентов. Обязателен, меняется после рестарта. |
| `api_keys` | `[]` | Upstream-ключи OpenCode. Пустой массив допустим: сервер запускается без inference-аккаунтов. |
| `accounts` | — | Явные аккаунты: свои ID, per-account настройки. Можно использовать вместе с `api_keys`. |
| `base_models` | список free-моделей | Что показывать в `/v1/models`. Это только объявления для IDE, а не whitelist: запросы к другим моделям проходят. |
| `reload_seconds` | `5` | Период проверки изменений аккаунтов и каталога. |
| `catalog_file` | `data/catalog.json` | Путь к кэшу проверенных моделей. В контейнере `data/...` разрешается в `/data/...`. |
| `allowed_hosts` | `["opencode.ai"]` | Хосты, разрешённые для `base_url` аккаунтов. |
| `allow_http_upstream` | `false` | Разрешить `http://` для `base_url`. Только для локальных тестов. |
| `defaults` | — | Общие настройки аккаунтов, созданных из `api_keys`. |

`api_keys` и `accounts` можно использовать вместе: сгенерированные аккаунты добавляются к явным. Внешний `accounts_file` нельзя смешивать ни с одним inline-полем (`defaults`, `api_keys`, `accounts`). Явный аккаунт включается через `"enabled": true`; у аккаунтов из `api_keys` включение по умолчанию `true`.

### `defaults`

Эти поля применяются только к аккаунтам, созданным из `api_keys`. В `accounts` их можно переопределить для каждого аккаунта.

| Поле | По умолчанию | Назначение |
|---|---|---|
| `enabled` | `true` | Создавать ли аккаунты из `api_keys` включёнными. |
| `profile` | `opencode-v2` | Режим upstream-заголовков: `raw` или `opencode-v2`. |
| `base_url` | `https://opencode.ai/inference/openai/v1` | Upstream endpoint. |
| `auth_mode` | `key` | Единственный допустимый режим: Bearer с ключом OpenCode. |
| `client_version` | `2.1.4` | Версия клиента для `User-Agent`. |
| `client_name` | `cli` | Имя клиента для `User-Agent` и `x-opencode-client`. |
| `client_channel` | `latest` | Канал релиза в `User-Agent`: `latest`, `dev`, `beta`, `local`, `next` или имя ветки. |
| `discovery` | выключено | Фоновое обновление каталога и проверки для этого аккаунта. |

Поля `client_version`, `client_name`, `client_channel` задают метаданные для upstream. Они формируют `User-Agent` в формате `opencode/{channel}/{version}/{name}` и отправляются в заголовке `x-opencode-client`.

### `accounts[]`

| Поле | Назначение |
|---|---|
| `id` | Уникальный ID: буквы, цифры, `_` и `-`; начинается с буквы или цифры. |
| `api_key` | Ключ OpenCode. |
| `enabled` | Включает аккаунт в пул. Для явной записи нужно указать `true`. |
| `models` | Необязательный список моделей для предпочтения этого аккаунта. Не ограничивает запросы: неизвестная модель уйдёт в любой доступный аккаунт. |
| `profile` | Переопределяет `defaults.profile`. |
| `base_url` | Переопределяет upstream endpoint; хост должен входить в `allowed_hosts`. |
| `auth_mode` | Только `key`. |
| `client_version`, `client_name`, `client_channel` | Переопределяют метаданные клиента. |
| `discovery` | Индивидуальные настройки фонового каталога. |

### `discovery`

| Поле | По умолчанию | Назначение |
|---|---|---|
| `enabled` | `false` | Включить фоновую работу с каталогом для аккаунта. |
| `check_enabled` | `false` | Выполнять реальные inference-пробы моделей. Тратит квоту. |
| `refresh_interval_seconds` | `21600` | Как часто обновлять список моделей. |
| `check_interval_seconds` | `3600` | Как часто проверять ещё не проверенные модели. |
| `max_checks_per_run` | `3` | Сколько моделей проверять за один проход. |
| `probe_timeout_seconds` | `30` | Таймаут одной проверки. |
| `delay_milliseconds` | `1000` | Пауза между проверками. |
| `recheck_after_seconds` | `86400` | Когда повторно проверять уже проверенную модель. |
| `failure_backoff_seconds` | `900` | Пауза после ошибки проверки. |

Фоновый каталог по умолчанию обновляется раз в 6 часов, inference-пробы выключены. Команды `models discover` и `models check` запускают то же самое вручную:

```sh
docker compose exec -T bridge /zen-bridge accounts list --config /config/config.json
docker compose exec -T bridge /zen-bridge models discover --config /config/config.json --account ID
docker compose exec -T bridge /zen-bridge models check --config /config/config.json --account ID
```

`discover` только читает upstream-каталог. `check` выполняет короткие генерации и сохраняет результат в `catalog_file`.

### Что перечитывается без рестарта

`defaults`, `api_keys`, `accounts` и настройки `discovery` перечитываются автоматически каждые `reload_seconds`: пул аккаунтов обновляется на месте, открытые диалоги сохраняют свой аккаунт. `log_level`, `base_models`, `catalog_file`, `allowed_hosts`, `allow_http_upstream`, `client_token` и `ZEN_BRIDGE_LISTEN` применяются только после рестарта. Битый файл целиком отклоняется, последняя рабочая конфигурация сохраняется.

## Профили

Профиль определяет набор upstream-заголовков. Тело запроса, `stream` и ответ не меняются.

| Профиль | Base URL по умолчанию | Заголовки |
|---|---|---|
| `opencode-v2` | `https://opencode.ai/inference/openai/v1` | `User-Agent: opencode/{channel}/{version}/{name}`, `x-opencode-session-id`, `x-opencode-session`, `x-session-affinity`, `X-Session-Id`, `x-opencode-project`, `x-opencode-client`, `x-opencode-parent-session-id` и `x-parent-session-id` для дочерних сессий. |
| `raw` | `https://opencode.ai/inference/openai/v1` | `User-Agent: zenbridge`. Без OpenCode-метаданных. Полезен для отладки или другого OpenAI-совместимого upstream. |

`opencode-v2` — профиль по умолчанию. Он воспроизводит заголовки, которые отправляет официальный клиент opencode при inference-запросах:

- `x-opencode-session-id` — ID диалога. Один диалог всегда даёт один и тот же `ses_...` (30 символов: `ses_` + 12 hex + 14 base62).
- `x-opencode-session` / `x-session-affinity` / `X-Session-Id` — affinity: ID родительской сессии, если она есть, иначе ID самой сессии. Именно так считает сам клиент.
- `x-opencode-parent-session-id` / `x-parent-session-id` — только для дочерних (subagent) сессий.
- `x-opencode-project` — проект: `global`, если проекта нет; 40 hex-символов передаются как есть; любое другое значение хэшируется в SHA-1 от `git-remote:<значение>`.
- `x-opencode-client` — значение `client_name` из конфига.
- `User-Agent` — формат `opencode/{client_channel}/{client_version}/{client_name}`.

Тело запроса уходит upstream байт в байт: шлюз не перекодирует JSON, не сортирует ключи и не переэкранирует `<`, `>`, `&`. `Accept` и `TypeSafe-Danger-Zone-Images` не добавляются — настоящий клиент их при обычном inference не отправляет. Заголовок `Accept-Encoding` повторяет то, что прислал клиент; если клиент его не прислал, шлюз запрашивает `gzip` сам и распаковывает ответ.

### `X-Bridge-Session` и определение диалога

Необязательный заголовок клиента задаёт ID диалога. Также принимаются `X-Conversation-Id`, `Conversation-Id`, `Session-Id` и `X-Session-Id`. Если ни одного нет, диалог определяется по первому сообщению пользователя: каждый следующий запрос того же диалога повторяет его, поэтому session ID и аккаунт остаются прежними даже при смене модели. Диалог помнится, пока он используется (LRU до 4096 записей), без таймаута: смена аккаунта посреди диалога ломает prompt cache.

`X-Bridge-Parent` (или `X-Parent-Session`) помечает запрос как дочерний: он получает свой `x-opencode-session-id`, а affinity и parent-заголовки указывают на родителя. `X-Bridge-Project` задаёт проект: 40 hex-символов передаются как есть, любое другое значение хэшируется в SHA-1 проекта.

В upstream локальные заголовки не передаются.

## API

| Endpoint | Описание |
|---|---|
| `GET /v1/models` | Модели из `base_models`, включённых аккаунтов и проверенного кэша. |
| `POST /v1/chat/completions` | Chat Completions, JSON или SSE. |
| `POST /v1/responses` | Responses API, JSON или SSE; поля передаются upstream без проверки. |
| `GET /healthz` / `GET /readyz` | Живость процесса / наличие включённого аккаунта. |

`stream: true` запрашивает SSE у upstream и возвращает его клиенту без разбора и агрегации; `stream: false` возвращает JSON как есть. Инструменты выполняет только клиент.

Шлюз сам возвращает ошибки только для своих проверок: неверный `client_token` (401), отсутствие аккаунтов (503), недоступный upstream (502), нечитаемый JSON для выбора модели (400). Всё остальное — статус, заголовки и тело upstream — проходит клиенту без изменений.

## Ограничения совместимости

Шлюз воспроизводит транспортный слой клиента: формат ID, набор заголовков, тело байт в байт, SSE. Он не воспроизводит содержимое, которое формирует сам opencode: системный промпт, список инструментов, служебные генерации (title, summary, subagent) и обращения к Console API (`/console/api/v2/config`, `/api/policies/current-budgets`, `/zen/go/v1/usage`). Это ограничение содержимого запроса, а не транспорта.

Собственные запросы шлюза отличаются от клиентских намеренно: каталог запрашивается обычным `GET /models` (только `Authorization`, `Accept` и `User-Agent`, без заголовков диалога), а inference-пробы идут одной сессией на проход.

### Лимиты бесплатных моделей

Бесплатные модели OpenCode Zen ограничены на стороне шлюза: дневная квота выдаётся на сеть и аккаунт, а не на ключ, и сбрасывается в 00:00 UTC. Шлюз на это не влияет — отказ приходит обычным ответом (`429` с телом `FreeUsageLimitError` и заголовком `Retry-After`, `402` при пустом балансе, `403` без подписки Go) и уходит клиенту без изменений. Причина видна в логе строкой `upstream rejected request`: там есть тело отказа и `Retry-After`.

Проверить, что дело именно в квоте, а не в заголовках моста, можно тем же запросом без авторизации: если он возвращает тот же `FreeUsageLimitError`, ограничение относится к сети, из которой идут запросы.

## Сборка и тесты

```sh
docker run --rm -v "${PWD}:/src" -v gomodcache:/go/pkg/mod -w /src golang:1.27.3-bookworm \
  bash -c "gofmt -l cmd internal; go vet ./... && CGO_ENABLED=1 go test -race -count=1 ./..."
docker build -t ghcr.io/revellison/zen-bridge-go:latest .
docker compose up -d
```

CI выполняет `go vet`, тесты и сборку. На push в `main` после успешного теста образ сразу публикуется в GHCR с тегами `latest` и полным SHA коммита; теги `v*` публикуются так же. Pull request проходит только проверку, без публикации.

Контейнер: scratch, UID/GID 10001, read-only root, без capabilities. Ресурсные лимиты Docker не задаются. Лицензия — MIT (`LICENSE`, `NOTICE`).
