package llm

import (
	"context"
)

func NewClient(_ context.Context, cfg Config) (Client, error) {
	return newOpenAICompatClient(cfg)
}
