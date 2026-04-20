# Telegram Gemini Bot (Go)

Лёгкий Telegram-бот на Go с Gemini API или Vertex AI.

## Что внутри

- Go
- long polling
- официальный Gemini SDK: `google.golang.org/genai`
- in-memory контекст без БД
- SQLite-память чатов в одном файле
- поддержка текста и изображений
- пример `systemd` unit для VPS

## Требования

- Go 1.24+
- Telegram bot token
- API key из Google AI Studio или доступ к Vertex AI

## Быстрый старт

```bash
cp .env.example .env
go mod tidy
go run .
```

### Local testing для vertex-grok

```bash
gcloud auth application-default login
ENV_FILE=.env.vertex-grok.local go run .
```

Если хочешь стартовать от текущего `.env`, то `.env.vertex-grok.local` может содержать только overrides для backend/model/project — загрузчик сначала прочитает `.env`, а потом применит `ENV_FILE` поверх него.

Пока для `vertex-grok` есть ограничение: изображения не поддерживаются.

## Переменные окружения

- `TELEGRAM_BOT_TOKEN` — токен Telegram-бота
- `AI_BACKEND` — `gemini`, `vertex`, `vertex-gemini`, `openai-compat` или `vertex-grok`, по умолчанию `gemini`
- `GEMINI_API_KEY` — API key Gemini для `AI_BACKEND=gemini`
- `GOOGLE_CLOUD_PROJECT` — GCP project для `AI_BACKEND=vertex-gemini`
- `GOOGLE_CLOUD_LOCATION` — регион Vertex AI для `AI_BACKEND=vertex-gemini`
- `GEMINI_MODEL` — по умолчанию `gemini-3.1-flash-lite-preview`
- `TAVILY_API_KEY` — включает веб-поиск через Tavily; если не задан, бот работает без интернета
- `TRIGGER_ALIAS` — текстовый алиас для вызова бота в группе, по умолчанию `@grok`
- `SYSTEM_PROMPT_ENABLED` — включает или полностью отключает системный промпт, по умолчанию `true`
- `SYSTEM_PROMPT` — базовая инструкция для модели
- `SYSTEM_PROMPT_FILE` — путь к файлу с системным промптом; если задан, имеет приоритет над `SYSTEM_PROMPT`
- `MAX_HISTORY_MESSAGES` — сколько последних сообщений хранить в памяти на чат
- `SEARCH_MAX_RESULTS` — сколько результатов отдавать в web search tool
- `MAX_IMAGE_BYTES` — максимальный размер изображения для обработки, по умолчанию 8 MB
- `SQLITE_PATH` — путь к SQLite-файлу с историей чатов
- `POLL_TIMEOUT_SECONDS` — timeout long polling
- `GEMINI_PROXY` — HTTP proxy для Gemini Developer API; к Vertex AI не применяется
- `OPENAI_BASE_URL` — base URL для OpenAI-compatible chat completions API
- `OPENAI_API_KEY` — bearer token / API key для OpenAI-compatible backend
- `VERTEX_OPENAI_BASE_URL` — удобный alias для `OPENAI_BASE_URL` при Vertex OpenAI-compatible доступе

### Режим Gemini

```env
AI_BACKEND=gemini
GEMINI_API_KEY=your_google_ai_studio_api_key
GEMINI_MODEL=gemini-2.5-flash
```

### Режим Vertex AI

```env
AI_BACKEND=vertex-gemini
GOOGLE_CLOUD_PROJECT=your-gcp-project-id
GOOGLE_CLOUD_LOCATION=us-central1
GEMINI_MODEL=gemini-2.5-flash
```

Для `AI_BACKEND=vertex-gemini` нужны Application Default Credentials (например, через service account или `gcloud auth application-default login`).

### OpenAI-compatible режим

```env
AI_BACKEND=openai-compat
OPENAI_BASE_URL=https://api.example.com/v1
OPENAI_API_KEY=your_token
```

`vertex-grok` использует тот же OpenAI-compatible путь. Для Vertex AI в таком режиме на практике нужны Google auth / ADC и регулярно обновляемые access tokens.

## Команды бота

- `/start`
- `/help`
- `/reset`
- `/status`

## Поведение в группах

- Контекст хранится **на весь групповой чат**, а не по пользователям.
- Бот сохраняет в SQLite **все сообщения группы**, даже если его не тегали.
- Бот отвечает в группе только если:
  - его упомянули через `@username`
  - или написали текстовый алиас, например `@grok`
  - ему ответили реплаем на его сообщение
  - вызвали команду
- Если пользователь пишет боту реплаем на чужое сообщение, текст этого сообщения тоже передается в модель как контекст.
- В сам prompt уходит не весь архив, а только ограниченное недавнее окно сообщений из SQLite.

## Изображения

- Бот умеет принимать фото из Telegram.
- Бот умеет принимать изображения, отправленные как file/document (`image/jpeg`, `image/png`, `image/webp`).
- Можно отправить фото с подписью или без подписи.
- Если есть подпись, она уйдет в Gemini вместе с изображением.
- Если подписи нет, бот сам попросит модель описать изображение по существу.
- Если написать боту реплаем на чужую картинку в группе, он сможет использовать это изображение как контекст.
- Слишком большие изображения бот отклоняет по лимиту `MAX_IMAGE_BYTES`.

## Готовые промпты

В проекте есть готовые варианты:

- `prompts/grok-soft.txt`
- `prompts/grok-balanced.txt`
- `prompts/grok-sharp.txt`

По умолчанию в `.env.example` выбран `grok-balanced.txt`.

## Сборка бинарника

```bash
mkdir -p bin
go build -o bin/telegram-gemini-bot .
```

## Деплой на HomeVpn / systemd

1. Собери бинарник.
2. Скопируй бинарник в `/opt/telegram-gemini-bot/`.
3. Скопируй `deploy/telegram-gemini-bot-homevpn.env.example` как `.env` и заполни значения.
4. Скопируй service-account JSON в `/etc/telegram-gemini-bot/service-account.json`.
5. Скопируй unit из `deploy/telegram-gemini-bot-homevpn.service` в `/etc/systemd/system/`.
6. Убедись, что `GOOGLE_APPLICATION_CREDENTIALS` указывает на JSON — это и есть ADC для сервера.
7. Имей в виду: `vertex-grok` в этом приложении поддерживает только текст.

Команды:

```bash
sudo systemctl daemon-reload
sudo systemctl enable telegram-gemini-bot
sudo systemctl restart telegram-gemini-bot
sudo systemctl status telegram-gemini-bot
```

### SSH deploy script

Есть helper-скрипт для полного деплоя по SSH:

```bash
chmod +x deploy/deploy.sh
deploy/deploy.sh --host your-server --env-file .env
```

Пример для HomeVpn / Vertex Grok:

```bash
deploy/deploy.sh \
  --host your-server \
  --env-file .env.homevpn \
  --service-file deploy/telegram-gemini-bot-homevpn.service \
  --credentials-file ~/service-account.json
```

Что делает скрипт:

- локально собирает Linux-бинарник `bin/telegram-gemini-bot`
- копирует бинарник, `.env` и `systemd` unit на сервер по SSH/SCP
- создаёт пользователя `telegrambot`, если его ещё нет
- обновляет файлы в `/opt/telegram-gemini-bot`
- при необходимости загружает service-account JSON в `/etc/telegram-gemini-bot/service-account.json`
- делает `systemctl daemon-reload`, `enable`, `restart`, `status`

Справка:

```bash
deploy/deploy.sh --help
```

## Примечания

- История чатов хранится в SQLite и сохраняется после рестарта.
- Для очень маленького VPS это проще и легче, чем Python/Node + БД + Docker.
- На старте бот сам пытается выбрать доступную `generateContent` модель через `ListModels`, поэтому можно не угадывать точное имя модели вручную.
- Если для модели приходит 429 с `limit: 0`, обычно free tier для этой модели недоступен для текущего проекта/аккаунта. Тогда нужен другой API key/проект или paid tier.
- Если задан `TAVILY_API_KEY`, бот использует function calling: Gemini сам решает, когда нужно сходить в веб-поиск за актуальной информацией.
- Бот умеет получать меню столовой через API `api.arambyeol.com` и может отвечать на запросы вроде «какое сегодня меню в столовке».
- Для Vertex AI доступность моделей зависит от проекта и региона.
