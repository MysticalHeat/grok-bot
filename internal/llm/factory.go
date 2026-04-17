package llm

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

func NewClient(ctx context.Context, cfg Config) (Client, error) {
	switch normalizeBackend(cfg.Backend) {
	case "gemini":
		return newGeminiClient(ctx, cfg, genai.BackendGeminiAPI)
	case "vertex-gemini":
		return newGeminiClient(ctx, cfg, genai.BackendVertexAI)
	case "openai-compat", "vertex-grok":
		return newOpenAICompatClient(cfg)
	default:
		return nil, fmt.Errorf("unsupported backend %q", cfg.Backend)
	}
}

func normalizeBackend(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "vertex":
		return "vertex-gemini"
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}
