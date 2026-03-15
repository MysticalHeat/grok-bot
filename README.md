# Telegram Gemini Bot (Go)

Лёгкий Telegram-бот на Go с Gemini API.

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
- API key из Google AI Studio

## Быстрый старт

```bash
cp .env.example .env
go mod tidy
go run .
```

## Переменные окружения

- `TELEGRAM_BOT_TOKEN` — токен Telegram-бота
- `GEMINI_API_KEY` — API key Gemini
- `GEMINI_MODEL` — по умолчанию `gemini-2.5-flash`
- `TAVILY_API_KEY` — включает веб-поиск через Tavily; если не задан, бот работает без интернета
- `TRIGGER_ALIAS` — текстовый алиас для вызова бота в группе, по умолчанию `@grok`
- `SYSTEM_PROMPT` — базовая инструкция для модели
- `SYSTEM_PROMPT_FILE` — путь к файлу с системным промптом; если задан, имеет приоритет над `SYSTEM_PROMPT`
- `MAX_HISTORY_MESSAGES` — сколько последних сообщений хранить в памяти на чат
- `SEARCH_MAX_RESULTS` — сколько результатов отдавать в web search tool
- `MAX_IMAGE_BYTES` — максимальный размер изображения для обработки, по умолчанию 8 MB
- `SQLITE_PATH` — путь к SQLite-файлу с историей чатов
- `POLL_TIMEOUT_SECONDS` — timeout long polling

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

## Запуск на VPS через systemd

1. Собери бинарник.
2. Положи `.env` рядом с бинарником или пропиши `Environment=` в unit.
3. Скопируй unit из `deploy/telegram-gemini-bot.service` в `/etc/systemd/system/`.

Команды:

```bash
sudo systemctl daemon-reload
sudo systemctl enable telegram-gemini-bot
sudo systemctl start telegram-gemini-bot
sudo systemctl status telegram-gemini-bot
```

## Примечания

- История чатов хранится в SQLite и сохраняется после рестарта.
- Для очень маленького VPS это проще и легче, чем Python/Node + БД + Docker.
- На старте бот сам пытается выбрать доступную `generateContent` модель через `ListModels`, поэтому можно не угадывать точное имя модели вручную.
- Если для модели приходит 429 с `limit: 0`, обычно free tier для этой модели недоступен для текущего проекта/аккаунта. Тогда нужен другой API key/проект или paid tier.
- Если задан `TAVILY_API_KEY`, бот использует function calling: Gemini сам решает, когда нужно сходить в веб-поиск за актуальной информацией.
