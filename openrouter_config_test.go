package main

import (
	"testing"
)

func TestOpenRouterConfigAndLegacyNames(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "legacy-openrouter-key")
	t.Setenv("AI_BACKEND", "openai-compat")
	t.Setenv("OPENAI_BASE_URL", "https://openrouter.ai/api/v1")
	t.Setenv("GEMINI_MODEL", "x-ai/grok-4.3")
	t.Setenv("CAFETERIA_MODEL", "gemini-3.8-flash")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AIBackend != "openrouter" || cfg.OpenAIAPIKey != "legacy-openrouter-key" || cfg.GeminiModel != "x-ai/grok-4.3" || cfg.CafeteriaModel != "google/gemini-3.8-flash" {
		t.Fatalf("unexpected config: backend=%s model=%s cafeteria=%s", cfg.AIBackend, cfg.GeminiModel, cfg.CafeteriaModel)
	}
	t.Setenv("OPENAI_BASE_URL", "https://example.com/v1")
	if _, err := loadConfig(); err == nil {
		t.Fatal("non-OpenRouter endpoint was accepted")
	}
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("AI_BACKEND", "gemini")
	if _, err := loadConfig(); err == nil {
		t.Fatal("direct Gemini backend was accepted")
	}
}
