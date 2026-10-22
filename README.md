# Zen Bridge

Небольшой Go-шлюз к OpenCode Zen: прозрачная труба с OpenAI-совместимым API, несколькими upstream-ключами и Responses API. Шлюз не ограничивает размеры, время и параллелизм, не проверяет схемы запросов и не пересобирает стрим. Только стандартная библиотека Go; в проде работает один статический контейнер.

## Как работает

```
клиент (IDE) → /v1/chat/completions или /v1/responses
             → локальный Bearer client_token
             → пул аккаунтов (round-robin, без лимитов и cooldown)
             → https://opencode.ai/zen/v1
```

Шлюз не хранит историю, не выполняет инструменты и не преобразует Chat ↔ Responses: клиент сам выбирает endpoint и передаёт полный контекст. `config.json` монтируется только для чтения, единственная запись — кэш каталога `/data/catalog.json`.

Запрос и ответ идут без изменений: `stream` передаётся как есть, SSE-байты и ошибки upstream возвращаются клиенту один в один. Модели вне списка аккаунта тоже маршрутизируются в любой доступный аккаунт, поэтому новые модели можно использовать сразу. Параметры reasoning (`reasoning_effort`, `reasoning`) шлюз не добавляет и не переписывает.

## Быстрый старт

```sh
cp config.example.json config.json
cp .env.example .env
# задайте client_token и добавьте реальные ключи в api_keys
docker compose up -d --build
curl -i http://127.0.0.1:18788/healthz
curl -i http://127.0.0.1:18788/readyz
```

### Порты

Адрес процесса задаётся только переменной окружения `ZEN_BRIDGE_LISTEN`, поля `listen` в `config.json` больше нет. По умолчанию используется `127.0.0.1:18788`, поэтому локальный запуск доступен только с этой машины:

```sh
ZEN_BRIDGE_LISTEN=127.0.0.1:19000 ./zen-bridge serve --config config.json
```

В Docker Compose контейнер обязан слушать все интерфейсы, поэтому compose задаёт `ZEN_BRIDGE_LISTEN=0.0.0.0:18788`. Наружу порт публикуется через `BRIDGE_PORT` из `.env`:

```sh
# .env
BRIDGE_PORT=19000
```

По умолчанию публикуется `127.0.0.1:18788`. Значение `BRIDGE_PORT=0` попросит Docker выбрать свободный порт; узнать его можно командой `docker compose port bridge 18788`. Внутренний порт контейнера фиксирован в compose и healthcheck; для его смены нужно править compose, а не `config.json`. Старые конфиги с полем `listen` отклоняются strict-парсером — удалите это поле.

`/readyz` возвращает 503, пока нет включённого аккаунта. Локальный ключ доступа и ключ Zen — разные секреты:

| Секрет | Поле | Назначение |
|---|---|---|
| Ключ клиента | `client_token` | Bearer-токен, которым IDE обращается к шлюзу. 8–256 символов без пробелов, обязателен. |
| Ключ Zen | `api_keys` | Upstream-ключ. Каждый получает стабильный ID `key_<hash>`. |

`config.json` содержит оба секрета в открытом виде: не коммитьте его и не публикуйте порт наружу без туннеля или VPN.

## Настройки

### Корневые поля

| Поле | По умолчанию | Назначение |
|---|---|---|
| `log_level` | `info` | `debug`, `info`, `warn` или `error`. Применяется после рестарта. |
| `client_token` | — | Локальный Bearer-токен клиентов. Обязателен, меняется после рестарта. |
| `api_keys` | `[]` | Upstream-ключи Zen. Пустой массив допустим: сервер запускается без inference-аккаунтов. |
| `accounts` | — | Явные аккаунты: свои ID, `public`, per-account настройки. Можно использовать вместе с `api_keys`. |
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
| `profile` | `opencode-v1` | Режим upstream-заголовков: `raw`, `opencode-v1`, `opencode-v2`, `zen-free-compat`. |
| `base_url` | зависит от профиля | Upstream endpoint. Для v1/free — `https://opencode.ai/zen/v1`, для v2 — `https://opencode.ai/inference/openai/v1`. |
| `auth_mode` | `key` | `key` — Bearer с ключом Zen. `public` здесь запрещён; анонимный режим задаётся только явным аккаунтом с профилем `zen-free-compat`. |
| `client_version` | `1.18.31` | Версия клиента для upstream-заголовков. Для `opencode-v2` по умолчанию `2.0.32`. |
| `client_name` | `cli` | Имя клиента для `x-opencode-client` и `User-Agent` профиля v2. |
| `client_channel` | `stable` | Канал релиза в `User-Agent` профиля v2. В других профилях не используется. |
| `inject_compat_tools` | `false` | Только для `zen-free-compat`: добавлять в `tools` схемы `bash` и `read`, если клиент их не передал. |
| `discovery` | выключено | Фоновое обновление каталога и проверки для этого аккаунта. |

Поля `client_*` — это метаданные для upstream. Они не влияют на тело запроса, выбор модели или аккаунта. Исключение — `inject_compat_tools`: он действительно добавляет описания инструментов в запрос. Реализацию `bash`/`read` всё равно выполняет клиент.

### `accounts[]`

| Поле | Назначение |
|---|---|
| `id` | Уникальный ID: буквы, цифры, `_` и `-`; начинается с буквы или цифры. |
| `api_key` | Ключ Zen для `auth_mode: "key"`. Для `public` не нужен. |
| `enabled` | Включает аккаунт в пул. Для явной записи нужно указать `true`. |
| `models` | Необязательный список моделей для предпочтения этого аккаунта. Не ограничивает запросы: неизвестная модель уйдёт в любой доступный аккаунт. |
| `profile` | Переопределяет `defaults.profile`. |
| `base_url` | Переопределяет upstream endpoint; хост должен входить в `allowed_hosts`. |
| `auth_mode` | `key` или `public`; `public` разрешён только с `zen-free-compat`. |
| `client_version`, `client_name`, `client_channel` | Переопределяют соответствующие метаданные. |
| `inject_compat_tools` | Переопределяет инъекцию `bash`/`read`; допустимо только для `zen-free-compat`. |
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

`defaults`, `api_keys` и `accounts` перечитываются автоматически каждые `reload_seconds`. `log_level`, `base_models`, `catalog_file`, `allowed_hosts`, `allow_http_upstream`, `client_token` и `ZEN_BRIDGE_LISTEN` применяются только после рестарта. Битый файл целиком отклоняется, последняя рабочая конфигурация сохраняется.

## Профили

Профиль определяет только набор upstream-заголовков и базовый URL. Тело запроса, `stream` и ответ не меняются.

| Профиль | Base URL по умолчанию | Заголовки |
|---|---|---|
| `raw` | `https://opencode.ai/zen/v1` | `User-Agent: zenbridge`; OpenCode-метаданные не отправляются. |
| `opencode-v1` | `https://opencode.ai/zen/v1` | `User-Agent: opencode/<client_version>`, `x-opencode-session`, `x-opencode-request`, `x-opencode-client`. |
| `opencode-v2` | `https://opencode.ai/inference/openai/v1` | `User-Agent: opencode/<client_channel>/<client_version>/<client_name>`, `x-opencode-client`, `x-opencode-session-id`, `x-opencode-session`, `X-Session-Id`, `x-session-affinity`. |
| `zen-free-compat` | `https://opencode.ai/zen/v1` | `User-Agent: opencode/<client_version> ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14`, `x-opencode-session`, `x-opencode-session-id`; допускает `inject_compat_tools`. |

`opencode-v1` — безопасный выбор по умолчанию. `opencode-v2` нужен для нового endpoint и v2-метаданных. `zen-free-compat` — для бесплатных моделей и режима `public`. `raw` полезен для отладки или другого OpenAI-совместимого upstream.

### `X-Bridge-Session`

Необязательный заголовок клиента привязывает последующие запросы к тому же аккаунту на 15 минут после последнего использования. В upstream этот заголовок не передаётся: вместо него шлюз генерирует собственный session ID для профилей OpenCode. Если привязка истекла или аккаунт выключен, выбирается новый аккаунт.

## API

| Endpoint | Описание |
|---|---|
| `GET /v1/models` | Модели из `base_models`, включённых аккаунтов и проверенного кэша. |
| `POST /v1/chat/completions` | Chat Completions, JSON или SSE. |
| `POST /v1/responses` | Responses API, JSON или SSE; поля передаются upstream без проверки. |
| `GET /healthz` / `GET /readyz` | Живость процесса / наличие включённого аккаунта. |

`stream: true` запрашивает SSE у upstream и возвращает его клиенту без разбора и агрегации; `stream: false` возвращает JSON как есть. Инструменты выполняет только клиент.

Шлюз сам возвращает ошибки только для своих проверок: неверный `client_token` (401), отсутствие аккаунтов (503), недоступный upstream (502), нечитаемый JSON для выбора модели (400). Всё остальное — статус, заголовки и тело upstream — проходит клиенту без изменений.

## Сборка и тесты

```sh
docker run --rm -v "${PWD}:/src" -v gomodcache:/go/pkg/mod -w /src golang:1.27.3-bookworm \
  bash -c "gofmt -l cmd internal; go vet ./... && CGO_ENABLED=1 go test -race -count=1 ./..."
docker compose up -d --build
```

CI дополнительно запускает Docker smoke-тест без реальных ключей: `/healthz` 200, `/readyz` 503, `/v1/models` без токена 401. Это не live-проверка Zen.

Контейнер: scratch, UID/GID 10001, read-only root, без capabilities. Ресурсные лимиты Docker не задаются. Лицензия — MIT (`LICENSE`, `NOTICE`).
