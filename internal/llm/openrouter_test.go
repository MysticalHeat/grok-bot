package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOpenRouterStructuredRequestAndImage(t *testing.T) {
	client, err := NewClient(context.Background(), Config{APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	c := client.(*openAICompatClient)
	c.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != OpenRouterBaseURL+"/chat/completions" {
			t.Fatalf("unexpected endpoint %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatal("missing bearer")
		}
		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Content []struct {
					Type     string `json:"type"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
			ResponseFormat struct {
				Type       string `json:"type"`
				JSONSchema struct {
					Strict bool `json:"strict"`
					Schema struct {
						Properties map[string]struct {
							Items struct {
								Type string `json:"type"`
							} `json:"items"`
						} `json:"properties"`
						AdditionalProperties *bool `json:"additionalProperties"`
					} `json:"schema"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "google/gemini-3.8-flash" {
			t.Fatalf("model %q", payload.Model)
		}
		if payload.Messages[0].Content[1].ImageURL.URL != "data:image/png;base64,AQI=" {
			t.Fatal("image not encoded")
		}
		format := payload.ResponseFormat
		if format.Type != "json_schema" || !format.JSONSchema.Strict || format.JSONSchema.Schema.Properties["items"].Items.Type != "string" || format.JSONSchema.Schema.AdditionalProperties == nil || *format.JSONSchema.Schema.AdditionalProperties {
			t.Fatalf("invalid schema: %+v", format)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"{\"items\":[\"x\"]}"}}]}`)), Request: r}, nil
	})}
	resp, err := c.Generate(context.Background(), Request{Model: "google/gemini-3.8-flash", Contents: []Content{{Role: "user", Parts: []Part{{Text: "Describe"}, {Data: []byte{1, 2}, MimeType: "image/png"}}}}, ResponseSchema: &Schema{Type: "object", Required: []string{"items"}, Properties: map[string]*Schema{"items": {Type: "array", Items: &Schema{Type: "string"}}}}})
	if err != nil || resp.Text != `{"items":["x"]}` {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
}

func TestOpenRouterQuotaError(t *testing.T) {
	c := &openAICompatClient{baseURL: OpenRouterBaseURL, bearer: "key", httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"7"}}, Body: io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)), Request: r}, nil
	})}}
	_, err := c.Generate(context.Background(), Request{Model: "test"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 || apiErr.RetryAfter != 7*time.Second {
		t.Fatalf("error=%v", err)
	}
}

func TestRejectNonImageBinary(t *testing.T) {
	_, err := toOpenAIMessage(Content{Parts: []Part{{Data: []byte("abc"), MimeType: "application/pdf"}}})
	if err == nil {
		t.Fatal("expected error for non-image binary")
	}
}

func TestOpenRouterUsageAndMaxTokens(t *testing.T) {
	c := &openAICompatClient{baseURL: OpenRouterBaseURL, bearer: "key", httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.MaxTokens != 512 {
			t.Fatalf("payload=%+v error=%v", payload, err)
		}
		body := `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1200,"completion_tokens":100,"cost":0.003}}`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}}
	response, err := c.Generate(context.Background(), Request{Model: "test", MaxTokens: 512, Contents: []Content{{Role: "user", Parts: []Part{{Text: "hello"}}}}})
	if err != nil || response.Usage == nil || response.Usage.Cost == nil || *response.Usage.Cost != 0.003 || response.Usage.PromptTokens != 1200 {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}
