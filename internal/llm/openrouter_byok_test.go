package llm

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

func TestOpenRouterBYOKRouting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		strict bool
	}{
		{name: "strict", strict: true},
		{name: "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := generateBYOKFixture(t, tc.strict, `{"is_byok":true}`, func(r *http.Request) {
				var payload map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				raw, hasProvider := payload["provider"]
				if !tc.strict {
					if hasProvider {
						t.Errorf("default client unexpectedly set provider routing: %s", raw)
					}
					return
				}
				if !hasProvider {
					t.Fatal("strict client omitted provider routing")
				}
				var provider struct {
					Only              []string `json:"only"`
					AllowFallbacks    *bool    `json:"allow_fallbacks"`
					RequireParameters *bool    `json:"require_parameters"`
				}
				if err := json.Unmarshal(raw, &provider); err != nil {
					t.Fatal(err)
				}
				if len(provider.Only) != 1 || provider.Only[0] != "openai" {
					t.Errorf("provider.only = %v, want [openai]", provider.Only)
				}
				if provider.AllowFallbacks == nil || *provider.AllowFallbacks {
					t.Errorf("provider.allow_fallbacks must be explicitly false: %s", raw)
				}
				if provider.RequireParameters == nil || !*provider.RequireParameters {
					t.Errorf("provider.require_parameters must be explicitly true: %s", raw)
				}
			})
			if err != nil || response.Text != "ok" {
				t.Fatalf("Generate() = %+v, %v; want accepted response", response, err)
			}
		})
	}
}

func TestOpenRouterBYOKRejectsUnconfirmedUsage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
	}{
		{name: "missing usage"},
		{name: "missing is_byok", usage: `{"cost":0.003}`},
		{name: "null is_byok", usage: `{"is_byok":null,"cost":0.003}`},
		{name: "false is_byok", usage: `{"is_byok":false,"cost":0.003}`},
		{name: "string is_byok", usage: `{"is_byok":"true","cost":0.003}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := generateBYOKFixture(t, true, tc.usage, nil)
			if err == nil {
				t.Error("strict client accepted a response without BYOK confirmation")
			}
			if response.Text != "" || response.Usage != nil || len(response.ConversationDelta) != 0 {
				t.Errorf("strict client exposed an unverified response: %+v", response)
			}
		})
	}
}

func TestOpenRouterBYOKUsageCost(t *testing.T) {
	for _, tc := range []struct {
		name      string
		strict    bool
		fields    string
		knownCost bool
		cost      float64
	}{
		{name: "sum fee and upstream", strict: true, fields: `"is_byok":true,"cost":0.002,"cost_details":{"upstream_inference_cost":0.003}`, knownCost: true, cost: 0.005},
		{name: "sum without strict mode", fields: `"is_byok":true,"cost":0.002,"cost_details":{"upstream_inference_cost":0.003}`, knownCost: true, cost: 0.005},
		{name: "zero fee with upstream charge", strict: true, fields: `"is_byok":true,"cost":0,"cost_details":{"upstream_inference_cost":0.003}`, knownCost: true, cost: 0.003},
		{name: "zero charges are known", strict: true, fields: `"is_byok":true,"cost":0,"cost_details":{"upstream_inference_cost":0}`, knownCost: true, cost: 0},
		{name: "missing fee", strict: true, fields: `"is_byok":true,"cost_details":{"upstream_inference_cost":0.003}`},
		{name: "null fee", strict: true, fields: `"is_byok":true,"cost":null,"cost_details":{"upstream_inference_cost":0.003}`},
		{name: "missing cost details", strict: true, fields: `"is_byok":true,"cost":0.002`},
		{name: "missing upstream amount", strict: true, fields: `"is_byok":true,"cost":0.002,"cost_details":{}`},
		{name: "null upstream amount", strict: true, fields: `"is_byok":true,"cost":0.002,"cost_details":{"upstream_inference_cost":null}`},
		{name: "negative fee", strict: true, fields: `"is_byok":true,"cost":-0.002,"cost_details":{"upstream_inference_cost":0.003}`},
		{name: "negative upstream", strict: true, fields: `"is_byok":true,"cost":0.002,"cost_details":{"upstream_inference_cost":-0.001}`},
		{name: "ordinary fee ignores upstream", fields: `"is_byok":false,"cost":0.002,"cost_details":{"upstream_inference_cost":0.003}`, knownCost: true, cost: 0.002},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := `{"prompt_tokens":12,"completion_tokens":3,` + tc.fields + `}`
			response, err := generateBYOKFixture(t, tc.strict, usage, nil)
			if err != nil || response.Text != "ok" || response.Usage == nil {
				t.Fatalf("Generate() = %+v, %v; want accepted response with usage", response, err)
			}
			if response.Usage.PromptTokens != 12 || response.Usage.CompletionTokens != 3 {
				t.Errorf("usage token counts changed: %+v", response.Usage)
			}
			cost := response.Usage.Cost
			if !tc.knownCost {
				if cost != nil {
					t.Errorf("Usage.Cost = %g, want nil", *cost)
				}
				return
			}
			if cost == nil {
				t.Fatalf("Usage.Cost = nil, want %g", tc.cost)
			}
			if math.Abs(*cost-tc.cost) > 1e-12 {
				t.Errorf("Usage.Cost = %g, want %g", *cost, tc.cost)
			}
		})
	}
}

func generateBYOKFixture(t *testing.T, strict bool, usage string, inspect func(*http.Request)) (Response, error) {
	t.Helper()
	ctx := context.Background()
	client, err := NewClient(ctx, Config{APIKey: "test-key", RequireOpenAIBYOK: strict})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"choices":[{"message":{"role":"assistant","content":"ok"}}]`
	if usage != "" {
		body += `,"usage":` + usage
	}
	body += `}`
	c := client.(*openAICompatClient)
	c.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if inspect != nil {
			inspect(r)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	return c.Generate(ctx, Request{Model: "openai/test-model", Contents: []Content{{Role: "user", Parts: []Part{{Text: "hello"}}}}})
}
