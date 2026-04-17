package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	"google.golang.org/genai"
	_ "modernc.org/sqlite"
	"telegram-gemini-bot/internal/llm"
)

const (
	defaultModel          = "gemini-3.1-flash-lite-preview"
	defaultAIBackend      = "gemini"
	defaultPollTimeoutSec = 30
	defaultHistoryLimit   = 12
	defaultSystemPrompt   = "Отвечай кратко и по делу на русском языке, если пользователь не просит иначе."
	maxReplyRunes         = 4000
	defaultSearchResults  = 5
	defaultMaxImageBytes  = 8 * 1024 * 1024
)

var seoulLocation = mustLoadLocation("Asia/Seoul")

var preferredModels = []string{
	"gemini-3.1-flash-lite-preview",
	"gemini-3.1-flash-lite",
	"gemini-2.5-flash",
	"gemini-2.0-flash-001",
	"gemini-2.0-flash",
	"gemini-1.5-flash",
}

type Config struct {
	TelegramToken       string
	AIBackend           string
	GeminiAPIKey        string
	OpenAIBaseURL       string
	OpenAIAPIKey        string
	GeminiModel         string
	GoogleCloudProject  string
	GoogleCloudLocation string
	TavilyAPIKey        string
	TriggerAlias        string
	GeminiProxy         string
	SystemPromptEnabled bool
	SystemPrompt        string
	SystemPromptFile    string
	PollTimeoutSec      int
	HistoryLimit        int
	SearchMaxResults    int
	MaxImageBytes       int
	SQLitePath          string
	ReadinessNotice     bool
}

type ChatMessage struct {
	Role string
	Text string
}

type ChatStore interface {
	Get(chatID int64) []ChatMessage
	Append(chatID int64, message ChatMessage) error
	Clear(chatID int64) error
	Close() error
}

type SQLiteStore struct {
	db       *sql.DB
	mu       sync.Mutex
	maxItems int
}

type BotService struct {
	bot        *tgbotapi.BotAPI
	llm        llm.Client
	httpClient *http.Client
	config     Config
	memory     ChatStore
	started    time.Time
}

type TavilySearchRequest struct {
	Query         string `json:"query"`
	SearchDepth   string `json:"search_depth,omitempty"`
	IncludeAnswer bool   `json:"include_answer,omitempty"`
	MaxResults    int    `json:"max_results,omitempty"`
}

type TavilySearchResponse struct {
	Answer  string               `json:"answer"`
	Results []TavilySearchResult `json:"results"`
	Query   string               `json:"query"`
}

type TavilySearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
	Score   any    `json:"score,omitempty"`
}

type CafeteriaWeeklyPlan struct {
	Date            string                     `json:"date"`
	MenusByMealType map[string][]CafeteriaMenu `json:"menusByMealType"`
}

type CafeteriaMenu struct {
	MenuID       int     `json:"menuId"`
	MenuName     string  `json:"menuName"`
	MealType     string  `json:"mealType"`
	Course       string  `json:"course"`
	AverageScore float64 `json:"averageScore"`
	ReviewCount  int     `json:"reviewCount"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := loadEnvFile(); err != nil {
		log.Fatalf("load env file: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	store, err := NewSQLiteStore(cfg.SQLitePath, cfg.HistoryLimit)
	if err != nil {
		log.Fatalf("create sqlite store: %v", err)
	}
	defer store.Close()

	resolvedModel, err := llm.ResolveModelName(ctx, llmConfig(cfg), cfg.GeminiModel, preferredModels)
	if err != nil {
		log.Fatalf("resolve model: %v", err)
	}
	cfg.GeminiModel = resolvedModel

	llmClient, err := llm.NewClient(ctx, llmConfig(cfg))
	if err != nil {
		log.Fatalf("create llm client: %v", err)
	}

	bot, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		log.Fatalf("create telegram bot: %v", err)
	}
	defer bot.StopReceivingUpdates()

	service := &BotService{
		bot: bot,
		llm: llmClient,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
		config:  cfg,
		memory:  store,
		started: time.Now(),
	}

	log.Printf("bot authorized as @%s", bot.Self.UserName)
	log.Printf("ai backend: %s", cfg.AIBackend)
	log.Printf("model: %s", cfg.GeminiModel)
	if cfg.TriggerAlias != "" {
		log.Printf("trigger alias: %s", cfg.TriggerAlias)
	}
	if cfg.TavilyAPIKey != "" {
		log.Printf("web search: enabled (Tavily)")
	} else {
		log.Printf("web search: disabled")
	}
	if cfg.GeminiProxy != "" {
		log.Printf("gemini proxy: enabled")
	}
	if cfg.AIBackend == "vertex-gemini" {
		log.Printf("vertex project: %s, location: %s", cfg.GoogleCloudProject, cfg.GoogleCloudLocation)
	}
	log.Printf("sqlite: %s", cfg.SQLitePath)

	updatesCfg := tgbotapi.NewUpdate(0)
	updatesCfg.Timeout = cfg.PollTimeoutSec
	updates := bot.GetUpdatesChan(updatesCfg)

	for {
		select {
		case <-ctx.Done():
			log.Println("shutdown signal received")
			return
		case update, ok := <-updates:
			if !ok {
				log.Println("telegram updates channel closed")
				return
			}

			if update.Message == nil {
				continue
			}

			if err := service.HandleMessage(ctx, update.Message); err != nil {
				log.Printf("handle message: %v", err)
			}
		}
	}
}

func loadConfig() (Config, error) {
	cfg := Config{
		AIBackend:           normalizeAIBackend(getEnv("AI_BACKEND", defaultAIBackend)),
		GeminiModel:         getEnv("GEMINI_MODEL", defaultModel),
		OpenAIBaseURL:       strings.TrimSpace(firstNonEmpty(os.Getenv("OPENAI_BASE_URL"), os.Getenv("VERTEX_OPENAI_BASE_URL"))),
		OpenAIAPIKey:        strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		GoogleCloudProject:  strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT")),
		GoogleCloudLocation: strings.TrimSpace(firstNonEmpty(os.Getenv("GOOGLE_CLOUD_LOCATION"), os.Getenv("GOOGLE_CLOUD_REGION"))),
		TavilyAPIKey:        strings.TrimSpace(os.Getenv("TAVILY_API_KEY")),
		TriggerAlias:        normalizeTriggerAlias(getEnv("TRIGGER_ALIAS", "@grok")),
		GeminiProxy:         strings.TrimSpace(os.Getenv("GEMINI_PROXY")),
		SystemPromptEnabled: getEnvAsBool("SYSTEM_PROMPT_ENABLED", true),
		SystemPrompt:        getEnv("SYSTEM_PROMPT", defaultSystemPrompt),
		SystemPromptFile:    getEnv("SYSTEM_PROMPT_FILE", ""),
		PollTimeoutSec:      getEnvAsInt("POLL_TIMEOUT_SECONDS", defaultPollTimeoutSec),
		HistoryLimit:        getEnvAsInt("MAX_HISTORY_MESSAGES", defaultHistoryLimit),
		SearchMaxResults:    getEnvAsInt("SEARCH_MAX_RESULTS", defaultSearchResults),
		MaxImageBytes:       getEnvAsInt("MAX_IMAGE_BYTES", defaultMaxImageBytes),
		SQLitePath:          getEnv("SQLITE_PATH", "bot.db"),
	}

	cfg.TelegramToken = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if cfg.TelegramToken == "" {
		return Config{}, errors.New("TELEGRAM_BOT_TOKEN is required")
	}

	switch cfg.AIBackend {
	case "gemini":
		cfg.GeminiAPIKey = strings.TrimSpace(firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY")))
		if cfg.GeminiAPIKey == "" {
			return Config{}, errors.New("GEMINI_API_KEY or GOOGLE_API_KEY is required for AI_BACKEND=gemini")
		}
	case "vertex-gemini":
		if cfg.GoogleCloudProject == "" {
			return Config{}, errors.New("GOOGLE_CLOUD_PROJECT is required for AI_BACKEND=vertex-gemini")
		}
		if cfg.GoogleCloudLocation == "" {
			return Config{}, errors.New("GOOGLE_CLOUD_LOCATION or GOOGLE_CLOUD_REGION is required for AI_BACKEND=vertex-gemini")
		}
	case "openai-compat", "vertex-grok":
		if cfg.OpenAIBaseURL == "" {
			if cfg.AIBackend == "vertex-grok" && cfg.GoogleCloudProject != "" {
				cfg.OpenAIBaseURL = defaultVertexOpenAIBaseURL(cfg.GoogleCloudProject, cfg.GoogleCloudLocation)
			}
		}
		if cfg.OpenAIBaseURL == "" {
			if cfg.AIBackend == "openai-compat" {
				return Config{}, errors.New("OPENAI_BASE_URL or VERTEX_OPENAI_BASE_URL is required for AI_BACKEND=openai-compat")
			}
			return Config{}, errors.New("OPENAI_BASE_URL, VERTEX_OPENAI_BASE_URL, or GOOGLE_CLOUD_PROJECT is required for AI_BACKEND=vertex-grok")
		}
		if cfg.AIBackend == "openai-compat" && cfg.OpenAIAPIKey == "" {
			return Config{}, errors.New("OPENAI_API_KEY is required for AI_BACKEND=openai-compat")
		}
	default:
		return Config{}, fmt.Errorf("unsupported AI_BACKEND %q (expected gemini, vertex, vertex-gemini, openai-compat, or vertex-grok)", cfg.AIBackend)
	}

	if cfg.HistoryLimit < 0 {
		cfg.HistoryLimit = 0
	}

	if cfg.PollTimeoutSec <= 0 {
		cfg.PollTimeoutSec = defaultPollTimeoutSec
	}

	if cfg.SearchMaxResults <= 0 {
		cfg.SearchMaxResults = defaultSearchResults
	}

	if cfg.MaxImageBytes <= 0 {
		cfg.MaxImageBytes = defaultMaxImageBytes
	}

	if cfg.SystemPromptEnabled && cfg.SystemPromptFile != "" {
		promptFromFile, err := os.ReadFile(cfg.SystemPromptFile)
		if err != nil {
			return Config{}, fmt.Errorf("read SYSTEM_PROMPT_FILE: %w", err)
		}

		trimmed := strings.TrimSpace(string(promptFromFile))
		if trimmed == "" {
			return Config{}, errors.New("SYSTEM_PROMPT_FILE is empty")
		}

		cfg.SystemPrompt = trimmed
	}

	if !cfg.SystemPromptEnabled {
		cfg.SystemPrompt = ""
		cfg.SystemPromptFile = ""
	}

	return cfg, nil
}

func (s *BotService) HandleMessage(ctx context.Context, message *tgbotapi.Message) error {
	text := strings.TrimSpace(extractMessageText(message))
	imageSource, hasImage := s.resolveImageSource(message)
	cleanText := s.cleanIncomingText(text)

	if message.IsCommand() {
		return s.handleCommand(message)
	}

	if !s.shouldRespond(message) {
		s.archiveObservedMessage(message, cleanText)
		return nil
	}

	if text == "" && !hasImage {
		return s.reply(message.Chat.ID, "Пока понимаю только текстовые сообщения и картинки.")
	}

	if _, err := s.bot.Request(tgbotapi.NewChatAction(message.Chat.ID, tgbotapi.ChatTyping)); err != nil {
		log.Printf("send chat action: %v", err)
	}

	history := s.memory.Get(message.Chat.ID)

	var replyText string
	var err error
	if hasImage {
		replyText, err = s.generateReplyToImage(ctx, message, history, cleanText, imageSource)
	} else {
		replyText, err = s.generateReply(ctx, history, buildUserInput(message, cleanText))
	}
	if err != nil {
		log.Printf("llm request failed: %v", err)
		return s.reply(message.Chat.ID, userFacingLLMError(err))
	}

	if err := s.memory.Append(message.Chat.ID, ChatMessage{Role: "user", Text: buildHistoryEntry(message, cleanText)}); err != nil {
		log.Printf("append user message to store: %v", err)
	}
	if err := s.memory.Append(message.Chat.ID, ChatMessage{Role: "assistant", Text: replyText}); err != nil {
		log.Printf("append assistant message to store: %v", err)
	}

	return s.reply(message.Chat.ID, replyText)
}

func (s *BotService) archiveObservedMessage(message *tgbotapi.Message, cleanText string) {
	if message == nil || message.Chat == nil {
		return
	}

	entry := buildHistoryEntry(message, cleanText)
	if strings.TrimSpace(entry) == "" {
		return
	}

	if err := s.memory.Append(message.Chat.ID, ChatMessage{Role: "user", Text: entry}); err != nil {
		log.Printf("append observed message to store: %v", err)
	}
}

func (s *BotService) handleCommand(message *tgbotapi.Message) error {
	switch message.Command() {
	case "start":
		if err := s.memory.Clear(message.Chat.ID); err != nil {
			log.Printf("clear store: %v", err)
		}
		return s.reply(message.Chat.ID, "Привет. Я Telegram-бот с AI-моделью. Напиши сообщение — и я отвечу. Команда /reset очищает контекст.")
	case "help":
		return s.reply(message.Chat.ID, "Команды:\n/start — перезапуск\n/help — помощь\n/reset — очистить контекст\n/status — статус")
	case "reset":
		if err := s.memory.Clear(message.Chat.ID); err != nil {
			log.Printf("clear store: %v", err)
		}
		return s.reply(message.Chat.ID, "Контекст очищен.")
	case "status":
		uptime := time.Since(s.started).Round(time.Second)
		return s.reply(message.Chat.ID, fmt.Sprintf("Работаю. Аптайм: %s. Модель: %s", uptime, s.config.GeminiModel))
	default:
		return s.reply(message.Chat.ID, "Неизвестная команда. Используй /help.")
	}
}

func (s *BotService) shouldRespond(message *tgbotapi.Message) bool {
	if message.Chat.IsPrivate() {
		return true
	}

	if message.IsCommand() {
		return true
	}

	if s.isReplyToBot(message) {
		return true
	}

	return s.isBotMentioned(extractMessageText(message))
}

func (s *BotService) isReplyToBot(message *tgbotapi.Message) bool {
	if message.ReplyToMessage == nil || message.ReplyToMessage.From == nil {
		return false
	}

	return message.ReplyToMessage.From.ID == s.bot.Self.ID
}

func (s *BotService) isBotMentioned(text string) bool {
	username := strings.TrimSpace(s.bot.Self.UserName)
	textLower := strings.ToLower(text)

	if username != "" && strings.Contains(textLower, "@"+strings.ToLower(username)) {
		return true
	}

	if s.config.TriggerAlias != "" && strings.Contains(textLower, strings.ToLower(s.config.TriggerAlias)) {
		return true
	}

	return false
}

func (s *BotService) cleanIncomingText(text string) string {
	username := strings.TrimSpace(s.bot.Self.UserName)
	cleaned := text
	if username != "" {
		cleaned = strings.ReplaceAll(cleaned, "@"+username, "")
		cleaned = strings.ReplaceAll(cleaned, "@"+strings.ToLower(username), "")
	}

	if s.config.TriggerAlias != "" {
		cleaned = replaceCaseInsensitive(cleaned, s.config.TriggerAlias, "")
	}

	return strings.TrimSpace(cleaned)
}

func (s *BotService) generateReply(ctx context.Context, history []ChatMessage, userText string) (string, error) {
	contents := buildContents(s.config.SystemPrompt, history, userText)
	return s.generateReplyFromContents(ctx, contents)
}

func (s *BotService) generateReplyToImage(ctx context.Context, message *tgbotapi.Message, history []ChatMessage, userText string, imageSource *tgbotapi.Message) (string, error) {
	imageBytes, mimeType, err := s.downloadImage(ctx, imageSource)
	if err != nil {
		return "", err
	}

	contents := buildContentsWithPhoto(s.config.SystemPrompt, history, buildUserInput(message, userText), imageBytes, mimeType)
	return s.generateReplyFromContents(ctx, contents)
}

func (s *BotService) generateReplyFromContents(ctx context.Context, contents []llm.Content) (string, error) {
	var lastErr error
	tools := s.toolDeclarations()

	for attempt := 1; attempt <= 3; attempt++ {
		result, err := s.llm.Generate(ctx, llm.Request{
			Model:    s.config.GeminiModel,
			Contents: contents,
			Tools:    tools,
		})
		if err == nil {
			if len(result.FunctionCalls) > 0 {
				toolReply, handled, toolErr := s.completeToolFlow(ctx, contents, result, tools)
				if toolErr != nil {
					lastErr = toolErr
					break
				}
				if handled {
					return truncateRunes(toolReply, maxReplyRunes), nil
				}
			}

			text := strings.TrimSpace(result.Text)
			if text == "" {
				return "", errors.New("empty response from model")
			}
			return truncateRunes(text, maxReplyRunes), nil
		}

		lastErr = err

		if isModelNotFoundError(err) {
			fallbackModel, resolveErr := llm.ResolveModelName(ctx, llmConfig(s.config), "", preferredModels)
			if resolveErr != nil {
				return "", err
			}

			if fallbackModel != "" && fallbackModel != s.config.GeminiModel {
				log.Printf("switching model from %s to %s", s.config.GeminiModel, fallbackModel)
				s.config.GeminiModel = fallbackModel
				continue
			}
		}

		if isQuotaError(err) {
			break
		}

		if attempt == 3 {
			break
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
	}

	return "", lastErr
}

func (s *BotService) downloadImage(ctx context.Context, message *tgbotapi.Message) ([]byte, string, error) {
	if message == nil {
		return nil, "", errors.New("image message is missing")
	}

	fileID := ""
	mimeType := "image/jpeg"

	if len(message.Photo) > 0 {
		photo := largestPhoto(message.Photo)
		fileID = photo.FileID
	} else if isImageDocument(message.Document) {
		fileID = message.Document.FileID
		if strings.TrimSpace(message.Document.MimeType) != "" {
			mimeType = strings.TrimSpace(message.Document.MimeType)
		}
	} else {
		return nil, "", errors.New("supported image is missing")
	}

	if fileID == "" {
		return nil, "", errors.New("image file id is missing")
	}

	fileURL, err := s.bot.GetFileDirectURL(fileID)
	if err != nil {
		return nil, "", fmt.Errorf("get telegram file url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, "", err
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("telegram file download failed: status %d", resp.StatusCode)
	}

	if resp.ContentLength > int64(s.config.MaxImageBytes) && resp.ContentLength > 0 {
		return nil, "", fmt.Errorf("image is too large: %d bytes (limit %d)", resp.ContentLength, s.config.MaxImageBytes)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(s.config.MaxImageBytes)+1))
	if err != nil {
		return nil, "", err
	}

	if len(data) > s.config.MaxImageBytes {
		return nil, "", fmt.Errorf("image is too large: %d bytes (limit %d)", len(data), s.config.MaxImageBytes)
	}

	mimeType = normalizeImageMIMEType(mimeType, resp.Header.Get("Content-Type"), message, data)

	return data, mimeType, nil
}

func largestPhoto(photos []tgbotapi.PhotoSize) tgbotapi.PhotoSize {
	best := photos[0]
	bestArea := best.Width * best.Height
	for _, photo := range photos[1:] {
		area := photo.Width * photo.Height
		if area > bestArea || (area == bestArea && photo.FileSize > best.FileSize) {
			best = photo
			bestArea = area
		}
	}
	return best
}

func (s *BotService) resolveImageSource(message *tgbotapi.Message) (*tgbotapi.Message, bool) {
	if hasSupportedImage(message) {
		return message, true
	}

	if message != nil && message.ReplyToMessage != nil && hasSupportedImage(message.ReplyToMessage) {
		return message.ReplyToMessage, true
	}

	return nil, false
}

func hasSupportedImage(message *tgbotapi.Message) bool {
	if message == nil {
		return false
	}

	if len(message.Photo) > 0 {
		return true
	}

	return isImageDocument(message.Document)
}

func isImageDocument(document *tgbotapi.Document) bool {
	if document == nil {
		return false
	}

	mimeType := normalizeSimpleMIMEType(document.MimeType)
	if mimeType == "image/jpeg" || mimeType == "image/png" || mimeType == "image/webp" {
		return true
	}

	if document.FileName != "" {
		byExt := mime.TypeByExtension(strings.ToLower(fileExtension(document.FileName)))
		byExt = normalizeSimpleMIMEType(byExt)
		return byExt == "image/jpeg" || byExt == "image/png" || byExt == "image/webp"
	}

	return false
}

func normalizeImageMIMEType(current string, header string, message *tgbotapi.Message, data []byte) string {
	for _, candidate := range []string{current, header} {
		normalized := normalizeSimpleMIMEType(candidate)
		if isSupportedGeminiImageMIMEType(normalized) {
			return normalized
		}
	}

	if message != nil && message.Document != nil {
		if normalized := normalizeSimpleMIMEType(message.Document.MimeType); isSupportedGeminiImageMIMEType(normalized) {
			return normalized
		}

		if ext := fileExtension(message.Document.FileName); ext != "" {
			if normalized := normalizeSimpleMIMEType(mime.TypeByExtension(ext)); isSupportedGeminiImageMIMEType(normalized) {
				return normalized
			}
		}
	}

	if len(data) > 0 {
		detected := normalizeSimpleMIMEType(http.DetectContentType(data))
		if isSupportedGeminiImageMIMEType(detected) {
			return detected
		}
	}

	return "image/jpeg"
}

func normalizeSimpleMIMEType(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}

	if parsed, _, err := mime.ParseMediaType(value); err == nil {
		return strings.TrimSpace(strings.ToLower(parsed))
	}

	if index := strings.Index(value, ";"); index >= 0 {
		return strings.TrimSpace(value[:index])
	}

	return value
}

func isSupportedGeminiImageMIMEType(value string) bool {
	switch value {
	case "image/jpeg", "image/png", "image/webp", "image/heic", "image/heif":
		return true
	default:
		return false
	}
}

func fileExtension(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	index := strings.LastIndex(name, ".")
	if index < 0 || index == len(name)-1 {
		return ""
	}

	return strings.ToLower(name[index:])
}

func normalizeMenuDate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nowInSeoul().Format("2006-01-02")
	}

	if _, err := time.Parse("2006-01-02", value); err == nil {
		return value
	}

	return nowInSeoul().Format("2006-01-02")
}

func normalizeMealType(value string) string {
	value = strings.TrimSpace(strings.ToUpper(value))
	switch value {
	case "BREAKFAST", "LUNCH", "DINNER":
		return value
	default:
		return ""
	}
}

func (s *BotService) toolDeclarations() []llm.Tool {
	tools := []llm.Tool{cafeteriaMenuToolDeclaration()}
	if s.searchEnabled() {
		tools = append(tools, webSearchToolDeclaration())
	}
	return tools
}

func (s *BotService) completeToolFlow(ctx context.Context, baseContents []llm.Content, initial llm.Response, tools []llm.Tool) (string, bool, error) {
	if len(initial.FunctionCalls) == 0 {
		return "", false, nil
	}

	contents := append([]llm.Content{}, baseContents...)
	contents = append(contents, initial.ConversationDelta...)

	handledAny := false
	for _, functionCall := range initial.FunctionCalls {
		toolResponse, handled, err := s.executeTool(ctx, functionCall)
		if err != nil {
			return "", false, err
		}
		if !handled {
			continue
		}

		handledAny = true
		contents = append(contents, llm.Content{
			Role: "user",
			Parts: []llm.Part{{
				FunctionResponse: &llm.FunctionResponse{
					ID:       functionCall.ID,
					Name:     functionCall.Name,
					Response: toolResponse,
				},
			}},
		})
	}

	if !handledAny {
		return "", false, nil
	}

	finalResult, err := s.llm.Generate(ctx, llm.Request{
		Model:    s.config.GeminiModel,
		Contents: contents,
		Tools:    tools,
	})
	if err != nil {
		return "", false, err
	}

	text := strings.TrimSpace(finalResult.Text)
	if text == "" {
		return "", false, errors.New("empty response from model after tool call")
	}

	return text, true, nil
}

func (s *BotService) executeTool(ctx context.Context, functionCall llm.FunctionCall) (map[string]any, bool, error) {
	switch functionCall.Name {
	case "web_search":
		return s.executeWebSearchTool(ctx, functionCall)
	case "get_cafeteria_menu":
		return s.executeCafeteriaMenuTool(ctx, functionCall)
	default:
		return nil, false, nil
	}
}

func (s *BotService) executeWebSearchTool(ctx context.Context, functionCall llm.FunctionCall) (map[string]any, bool, error) {
	if !s.searchEnabled() {
		return map[string]any{"ok": false, "error": "web search is disabled"}, true, nil
	}

	query, _ := functionCall.Args["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return map[string]any{
			"ok":    false,
			"error": "empty query",
		}, true, nil
	}

	result, err := s.tavilySearch(ctx, query)
	if err != nil {
		return map[string]any{
			"ok":    false,
			"error": err.Error(),
			"query": query,
		}, true, nil
	}

	response := map[string]any{
		"ok":      true,
		"query":   query,
		"answer":  result.Answer,
		"results": make([]map[string]any, 0, len(result.Results)),
	}

	results := response["results"].([]map[string]any)
	for _, item := range result.Results {
		results = append(results, map[string]any{
			"title":   item.Title,
			"url":     item.URL,
			"content": truncateRunes(strings.TrimSpace(item.Content), 600),
		})
	}
	response["results"] = results

	return response, true, nil
}

func (s *BotService) executeCafeteriaMenuTool(ctx context.Context, functionCall llm.FunctionCall) (map[string]any, bool, error) {
	dateValue, _ := functionCall.Args["date"].(string)
	mealTypeValue, _ := functionCall.Args["meal_type"].(string)

	targetDate := normalizeMenuDate(dateValue)
	mealType := normalizeMealType(mealTypeValue)
	if mealType == "" {
		mealType = "LUNCH"
	}

	plan, err := s.fetchCafeteriaWeeklyPlan(ctx, targetDate)
	if err != nil {
		return map[string]any{
			"ok":        false,
			"date":      targetDate,
			"meal_type": mealType,
			"error":     err.Error(),
		}, true, nil
	}

	menus := plan.MenusByMealType[mealType]
	items := make([]map[string]any, 0, len(menus))
	for _, item := range menus {
		name := strings.TrimSpace(item.MenuName)
		if name == "" || name == "*" {
			continue
		}
		items = append(items, map[string]any{
			"course":        strings.TrimSpace(item.Course),
			"menu_name":     name,
			"average_score": item.AverageScore,
			"review_count":  item.ReviewCount,
		})
	}

	log.Printf("cafeteria tool result: date=%s meal_type=%s items=%d", plan.Date, mealType, len(items))

	return map[string]any{
		"ok":        true,
		"date":      plan.Date,
		"meal_type": mealType,
		"items":     items,
		"source":    fmt.Sprintf("https://api.arambyeol.com/plans/weekly/%s", targetDate),
	}, true, nil
}

func (s *BotService) fetchCafeteriaWeeklyPlan(ctx context.Context, date string) (*CafeteriaWeeklyPlan, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://api.arambyeol.com/plans/weekly/%s", date), nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	log.Printf("cafeteria api raw response: date=%s body=%s", date, truncateRunes(strings.TrimSpace(string(body)), 4000))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cafeteria api failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var plans []CafeteriaWeeklyPlan
	if err := json.Unmarshal(body, &plans); err != nil {
		return nil, err
	}

	for _, plan := range plans {
		if plan.Date == date {
			return &plan, nil
		}
	}

	return nil, fmt.Errorf("no cafeteria menu found for %s", date)
}

func (s *BotService) tavilySearch(ctx context.Context, query string) (*TavilySearchResponse, error) {
	requestBody := TavilySearchRequest{
		Query:         query,
		SearchDepth:   "basic",
		IncludeAnswer: true,
		MaxResults:    s.config.SearchMaxResults,
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.config.TavilyAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tavily search failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	var searchResponse TavilySearchResponse
	if err := json.Unmarshal(responseBody, &searchResponse); err != nil {
		return nil, err
	}

	return &searchResponse, nil
}

func (s *BotService) searchEnabled() bool {
	return strings.TrimSpace(s.config.TavilyAPIKey) != ""
}

func webSearchToolDeclaration() llm.Tool {
	return llm.Tool{
		Name:        "web_search",
		Description: "Search the web for fresh or external information when the answer depends on current events, recent facts, online sources, or data outside the model's memory.",
		Parameters: &llm.Schema{
			Type: "OBJECT",
			Properties: map[string]*llm.Schema{
				"query": {
					Type:        "STRING",
					Description: "The search query in the user's language. Make it specific enough to retrieve relevant current information.",
				},
			},
			Required: []string{"query"},
		},
	}
}

func cafeteriaMenuToolDeclaration() llm.Tool {
	return llm.Tool{
		Name:        "get_cafeteria_menu",
		Description: "Get today's or a specific day's cafeteria menu from Arambyeol cafeteria API. Use this when the user asks what is served in the cafeteria for breakfast, lunch, or dinner.",
		Parameters: &llm.Schema{
			Type: "OBJECT",
			Properties: map[string]*llm.Schema{
				"date": {
					Type:        "STRING",
					Description: "Optional date in YYYY-MM-DD format. If omitted, use today's date.",
				},
				"meal_type": {
					Type:        "STRING",
					Description: "Optional meal type: BREAKFAST, LUNCH, or DINNER. If omitted, default to LUNCH.",
				},
			},
		},
	}
}

func buildUserInput(message *tgbotapi.Message, userText string) string {
	userText = strings.TrimSpace(userText)
	if message == nil {
		return userText
	}

	if message.ReplyToMessage == nil {
		return userText
	}

	repliedText := extractMessageText(message.ReplyToMessage)
	if repliedText == "" {
		return userText
	}

	quotedAuthor := displayName(message.ReplyToMessage.From)
	if quotedAuthor == "" {
		quotedAuthor = "unknown"
	}

	if userText == "" {
		return fmt.Sprintf("Контекст реплая:\n%s: %s", quotedAuthor, repliedText)
	}

	return fmt.Sprintf("Контекст реплая:\n%s: %s\n\nЗапрос пользователя:\n%s", quotedAuthor, repliedText, userText)
}

func buildHistoryEntry(message *tgbotapi.Message, userText string) string {
	author := displayName(nil)
	if message != nil {
		author = displayName(message.From)
	}

	userText = strings.TrimSpace(userText)
	if userText == "" {
		userText = "[empty message]"
	}

	if message == nil || message.ReplyToMessage == nil {
		return fmt.Sprintf("%s: %s", author, userText)
	}

	repliedText := extractMessageText(message.ReplyToMessage)
	if repliedText == "" {
		return fmt.Sprintf("%s: %s", author, userText)
	}

	repliedAuthor := displayName(message.ReplyToMessage.From)
	return fmt.Sprintf("%s replied to %s (%s): %s", author, repliedAuthor, repliedText, userText)
}

func extractMessageText(message *tgbotapi.Message) string {
	if message == nil {
		return ""
	}

	for _, candidate := range []string{message.Text, message.Caption} {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" {
			return candidate
		}
	}

	return ""
}

func displayName(user *tgbotapi.User) string {
	if user == nil {
		return "user"
	}

	if user.UserName != "" {
		return "@" + user.UserName
	}

	fullName := strings.TrimSpace(strings.Join([]string{user.FirstName, user.LastName}, " "))
	if fullName != "" {
		return fullName
	}

	return "user"
}

func userFacingLLMError(err error) string {
	message := err.Error()
	if strings.Contains(message, "image is too large") {
		return "Картинка слишком большая для обработки. Отправь более легкое изображение или сожми файл."
	}

	if strings.Contains(strings.ToLower(message), "openai-compatible adapter does not support image") {
		return "Для этого backend изображения пока не поддерживаются. Переключись на Gemini backend или отправь текстовый запрос."
	}

	if isQuotaError(err) {
		if retryDelay := extractRetryDelay(err); retryDelay > 0 {
			return fmt.Sprintf("Лимит модели сейчас исчерпан. Попробуй снова примерно через %s.", retryDelay.Round(time.Second))
		}

		return "Лимит модели для этого проекта сейчас исчерпан или недоступен. Попробуй позже или смени модель/ключ доступа."
	}

	if isModelNotFoundError(err) {
		return "У текущих credentials недоступна выбранная модель. Я попробовал подобрать доступную, но не смог. Проверь имя модели и доступы."
	}

	return "Не смог получить ответ от модели. Попробуй ещё раз через пару секунд."
}

func isQuotaError(err error) bool {
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		return false
	}

	if apiErr.Code == 429 {
		return true
	}

	return apiErr.Status == "RESOURCE_EXHAUSTED"
}

func isModelNotFoundError(err error) bool {
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		return false
	}

	if apiErr.Code == 404 || apiErr.Status == "NOT_FOUND" {
		return true
	}

	return strings.Contains(strings.ToLower(apiErr.Message), "model") && strings.Contains(strings.ToLower(apiErr.Message), "not found")
}

func extractRetryDelay(err error) time.Duration {
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		return 0
	}

	for _, detail := range apiErr.Details {
		typeName, _ := detail["@type"].(string)
		if !strings.Contains(typeName, "RetryInfo") {
			continue
		}

		rawDelay, _ := detail["retryDelay"].(string)
		if rawDelay == "" {
			continue
		}

		parsed, parseErr := time.ParseDuration(rawDelay)
		if parseErr == nil {
			return parsed
		}
	}

	return 0
}

func (s *BotService) reply(chatID int64, text string) error {
	message := tgbotapi.NewMessage(chatID, text)
	message.ParseMode = ""
	_, err := s.bot.Send(message)
	return err
}

func NewSQLiteStore(path string, maxItems int) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	store := &SQLiteStore{db: db, maxItems: maxItems}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *SQLiteStore) init() error {
	statements := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA busy_timeout = 5000;`,
		`CREATE TABLE IF NOT EXISTS chat_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id INTEGER NOT NULL,
			role TEXT NOT NULL,
			text TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_chat_messages_chat_id_id ON chat_messages(chat_id, id);`,
	}

	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}

	return nil
}

func (s *SQLiteStore) Get(chatID int64) []ChatMessage {
	limit := s.maxItems
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.db.Query(`
		SELECT role, text FROM (
			SELECT role, text, id
			FROM chat_messages
			WHERE chat_id = ?
			ORDER BY id DESC
			LIMIT ?
		)
		ORDER BY id ASC
	`, chatID, limit)
	if err != nil {
		log.Printf("sqlite get history: %v", err)
		return nil
	}
	defer rows.Close()

	history := make([]ChatMessage, 0, limit)
	for rows.Next() {
		var message ChatMessage
		if err := rows.Scan(&message.Role, &message.Text); err != nil {
			log.Printf("sqlite scan history: %v", err)
			return history
		}
		history = append(history, message)
	}

	return history
}

func (s *SQLiteStore) Append(chatID int64, message ChatMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec(`INSERT INTO chat_messages(chat_id, role, text) VALUES(?, ?, ?)`, chatID, message.Role, message.Text); err != nil {
		return err
	}

	if s.maxItems > 0 {
		_, err := s.db.Exec(`
			DELETE FROM chat_messages
			WHERE chat_id = ? AND id NOT IN (
				SELECT id FROM chat_messages WHERE chat_id = ? ORDER BY id DESC LIMIT ?
			)
		`, chatID, chatID, s.maxItems)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *SQLiteStore) Clear(chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM chat_messages WHERE chat_id = ?`, chatID)
	return err
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func buildContents(systemPrompt string, history []ChatMessage, userText string) []llm.Content {
	contents := make([]llm.Content, 0, len(history)+2)

	if systemPrompt != "" {
		contents = append(contents, llm.Content{Role: "user", Parts: []llm.Part{{Text: systemPrompt}}})
	}

	contents = append(contents, llm.Content{Role: "user", Parts: []llm.Part{{Text: currentDateContext()}}})

	for _, message := range history {
		role := "user"
		if message.Role == "assistant" {
			role = "assistant"
		}

		contents = append(contents, llm.Content{Role: role, Parts: []llm.Part{{Text: message.Text}}})
	}

	if strings.TrimSpace(userText) != "" {
		contents = append(contents, llm.Content{Role: "user", Parts: []llm.Part{{Text: userText}}})
	}

	return contents
}

func nowInSeoul() time.Time {
	return time.Now().In(seoulLocation)
}

func currentDateContext() string {
	now := nowInSeoul()
	return fmt.Sprintf("Текущая дата и время для всех относительных запросов: %s (Asia/Seoul). День недели: %s.", now.Format("2006-01-02 15:04:05"), weekdayRu(now.Weekday()))
}

func weekdayRu(day time.Weekday) string {
	switch day {
	case time.Monday:
		return "понедельник"
	case time.Tuesday:
		return "вторник"
	case time.Wednesday:
		return "среда"
	case time.Thursday:
		return "четверг"
	case time.Friday:
		return "пятница"
	case time.Saturday:
		return "суббота"
	case time.Sunday:
		return "воскресенье"
	default:
		return "неизвестно"
	}
}

func mustLoadLocation(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone(name, 9*60*60)
	}
	return location
}

func buildContentsWithPhoto(systemPrompt string, history []ChatMessage, userText string, imageBytes []byte, mimeType string) []llm.Content {
	contents := buildContents(systemPrompt, history, "")
	parts := make([]llm.Part, 0, 2)

	if strings.TrimSpace(userText) != "" {
		parts = append(parts, llm.Part{Text: userText})
	} else {
		parts = append(parts, llm.Part{Text: "Опиши, что на изображении, и ответь по существу."})
	}

	parts = append(parts, llm.Part{Data: imageBytes, MimeType: mimeType})
	contents = append(contents, llm.Content{Role: "user", Parts: parts})
	return contents
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return value
	}

	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}

	return string(runes[:limit-1]) + "…"
}

func getEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getEnvAsInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}

	return parsed
}

func getEnvAsBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func normalizeTriggerAlias(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "@") {
		value = "@" + value
	}
	return value
}

func replaceCaseInsensitive(source, oldValue, newValue string) string {
	if oldValue == "" {
		return source
	}

	lowerSource := strings.ToLower(source)
	lowerOld := strings.ToLower(oldValue)

	var builder strings.Builder
	start := 0
	for {
		index := strings.Index(lowerSource[start:], lowerOld)
		if index < 0 {
			builder.WriteString(source[start:])
			break
		}

		index += start
		builder.WriteString(source[start:index])
		builder.WriteString(newValue)
		start = index + len(oldValue)
	}

	return builder.String()
}

func llmConfig(cfg Config) llm.Config {
	return llm.Config{
		Backend:  cfg.AIBackend,
		Model:    cfg.GeminiModel,
		APIKey:   cfg.GeminiAPIKey,
		BaseURL:  cfg.OpenAIBaseURL,
		Token:    cfg.OpenAIAPIKey,
		Project:  cfg.GoogleCloudProject,
		Location: cfg.GoogleCloudLocation,
		Proxy:    cfg.GeminiProxy,
	}
}

func normalizeAIBackend(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "gemini":
		return "gemini"
	case "vertex", "vertex-gemini":
		return "vertex-gemini"
	case "openai-compat", "vertex-grok":
		return value
	default:
		return value
	}
}

func loadEnvFile() error {
	envFile := strings.TrimSpace(os.Getenv("ENV_FILE"))
	if envFile == "" {
		envFile = ".env"
	}

	if envFile == ".env" {
		if err := godotenv.Load(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: %w", envFile, err)
		}
		return nil
	}

	if err := godotenv.Load(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(".env: %w", err)
	}

	if err := godotenv.Overload(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", envFile, err)
	}

	return nil
}

func defaultVertexOpenAIBaseURL(project, location string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		return ""
	}

	location = strings.TrimSpace(location)
	if location == "" {
		location = "global"
	}

	return fmt.Sprintf("https://aiplatform.googleapis.com/v1/projects/%s/locations/%s/endpoints/openapi", project, location)
}
