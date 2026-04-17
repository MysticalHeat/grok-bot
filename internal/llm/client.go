package llm

import "context"

type Client interface {
	Generate(ctx context.Context, req Request) (Response, error)
}
