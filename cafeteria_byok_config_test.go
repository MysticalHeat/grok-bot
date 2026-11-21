package main

import (
	"os"
	"testing"
)

func TestCafeteriaBYOKConfig(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test")
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("AI_BACKEND", "openrouter")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("SYSTEM_PROMPT_FILE", "")
	t.Setenv("CAFETERIA_REFRESH_MINUTES", "60")
	t.Setenv("CAFETERIA_DAYS_AHEAD", "14")

	const env = "CAFETERIA_REQUIRE_OPENAI_BYOK"
	t.Setenv(env, "")
	if err := os.Unsetenv(env); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CafeteriaRequireOpenAIBYOK || cafeteriaLLMConfig(cfg).RequireOpenAIBYOK {
		t.Error("cafeteria BYOK must default to false when the variable is unset")
	}

	t.Setenv(env, "true")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CafeteriaRequireOpenAIBYOK {
		t.Error("loadConfig did not enable cafeteria BYOK")
	}

	// Exercise config mapping independently so a broken env parser cannot mask a chat leak.
	cfg.CafeteriaRequireOpenAIBYOK = true
	if got := cafeteriaLLMConfig(cfg); !got.RequireOpenAIBYOK || got.APIKey != cfg.OpenAIAPIKey {
		t.Errorf("cafeteria client config: BYOK=%v, OpenRouter key preserved=%v; want both true", got.RequireOpenAIBYOK, got.APIKey == cfg.OpenAIAPIKey)
	}
	if llmConfig(cfg).RequireOpenAIBYOK {
		t.Error("cafeteria BYOK setting must not affect chat")
	}
}
