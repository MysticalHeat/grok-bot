package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
)

type openAICompatClient struct {
	httpClient *http.Client
	baseURL    string
	bearer     string
	creds      *auth.Credentials
}

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

func newOpenAICompatClient(cfg Config) (Client, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is required for backend %q", cfg.Backend)
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q", baseURL)
	}

	if strings.TrimSpace(parsed.Path) == "" || parsed.Path == "/" {
		parsed.Path = "/v1"
	}

	bearer := strings.TrimSpace(firstNonEmptyCompat(cfg.Token, cfg.APIKey))
	var creds *auth.Credentials
	if bearer == "" {
		if normalizeBackend(cfg.Backend) != "vertex-grok" && !strings.Contains(parsed.Host, "aiplatform.googleapis.com") {
			return nil, fmt.Errorf("token is required for backend %q", cfg.Backend)
		}

		var err error
		creds, err = credentials.DetectDefault(&credentials.DetectOptions{
			Scopes: []string{cloudPlatformScope},
		})
		if err != nil {
			return nil, fmt.Errorf("detect application default credentials: %w", err)
		}
		creds.TokenProvider = auth.NewCachedTokenProvider(creds.TokenProvider, nil)
	}

	return &openAICompatClient{
		httpClient: &http.Client{},
		baseURL:    strings.TrimRight(parsed.String(), "/"),
		bearer:     bearer,
		creds:      creds,
	}, nil
}

func (c *openAICompatClient) Generate(ctx context.Context, req Request) (Response, error) {
	payload, err := toOpenAIChatRequest(req)
	if err != nil {
		return Response{}, err
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	bearer, err := c.authorizationToken(ctx)
	if err != nil {
		return Response{}, err
	}
	if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("openai-compatible request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var decoded openAIChatResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return Response{}, err
	}
	return decoded.toResponse(), nil
}

func (c *openAICompatClient) authorizationToken(ctx context.Context) (string, error) {
	if c.bearer != "" {
		return c.bearer, nil
	}
	if c.creds == nil {
		return "", nil
	}

	token, err := c.creds.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("obtain access token from adc: %w", err)
	}
	if token == nil || strings.TrimSpace(token.Value) == "" {
		return "", fmt.Errorf("adc returned empty access token")
	}

	return strings.TrimSpace(token.Value), nil
}

type openAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Tools       []openAITool    `json:"tools,omitempty"`
	ToolChoice  string          `json:"tool_choice,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

type openAITextPart struct{ Type, Text string }
type openAIImagePart struct{ Type, URL string }
type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}
type openAIToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type openAIChatResponse struct {
	Choices []struct {
		Message openAIResponseMessage `json:"message"`
	} `json:"choices"`
}
type openAIResponseMessage struct {
	Role      string           `json:"role"`
	Content   json.RawMessage  `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls"`
}

func toOpenAIChatRequest(req Request) (openAIChatRequest, error) {
	messages := make([]openAIMessage, 0, len(req.Contents))
	for _, content := range req.Contents {
		msg, err := toOpenAIMessage(content)
		if err != nil {
			return openAIChatRequest{}, err
		}
		messages = append(messages, msg)
	}
	payload := openAIChatRequest{Model: req.Model, Messages: messages}
	if len(req.Tools) > 0 {
		payload.Tools = make([]openAITool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			payload.Tools = append(payload.Tools, openAITool{Type: "function", Function: openAIToolFunction{Name: tool.Name, Description: tool.Description, Parameters: schemaToMap(tool.Parameters)}})
		}
	}
	return payload, nil
}

func toOpenAIMessage(content Content) (openAIMessage, error) {
	role := strings.ToLower(strings.TrimSpace(content.Role))
	if role == "" {
		role = "user"
	}
	msg := openAIMessage{Role: role}
	if len(content.Parts) == 0 {
		return msg, nil
	}
	parts := make([]map[string]any, 0, len(content.Parts))
	textParts := 0
	for _, part := range content.Parts {
		switch {
		case part.FunctionResponse != nil:
			payload, err := json.Marshal(part.FunctionResponse.Response)
			if err != nil {
				return openAIMessage{}, err
			}
			msg.Role = "tool"
			msg.Name = part.FunctionResponse.Name
			msg.ToolCallID = strings.TrimSpace(part.FunctionResponse.ID)
			msg.Content = string(payload)
		case part.FunctionCall != nil:
			callID := strings.TrimSpace(part.FunctionCall.ID)
			if callID == "" {
				callID = fmt.Sprintf("call_%d", len(msg.ToolCalls))
			}
			call := openAIToolCall{ID: callID, Type: "function"}
			call.Function.Name = part.FunctionCall.Name
			payload, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				return openAIMessage{}, err
			}
			call.Function.Arguments = string(payload)
			msg.ToolCalls = append(msg.ToolCalls, call)
		case part.Text != "":
			textParts++
			parts = append(parts, map[string]any{"type": "text", "text": part.Text})
		case len(part.Data) > 0:
			return openAIMessage{}, fmt.Errorf("openai-compatible adapter does not support image/binary content yet")
		}
	}

	if msg.Role == "tool" {
		return msg, nil
	}

	if textParts == 1 && len(parts) == 1 && len(msg.ToolCalls) == 0 {
		msg.Content = parts[0]["text"]
	} else if len(parts) > 0 {
		msg.Content = parts
	}

	return msg, nil
}

func (r openAIChatResponse) toResponse() Response {
	if len(r.Choices) == 0 {
		return Response{}
	}
	m := r.Choices[0].Message
	text := extractOpenAIText(m.Content)
	resp := Response{Text: text}
	delta := Content{Role: strings.TrimSpace(firstNonEmptyCompat(m.Role, "assistant"))}
	if text != "" {
		delta.Parts = append(delta.Parts, Part{Text: text})
	}
	for _, tc := range m.ToolCalls {
		var args map[string]any
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		call := FunctionCall{ID: tc.ID, Name: tc.Function.Name, Args: args}
		resp.FunctionCalls = append(resp.FunctionCalls, call)
		delta.Parts = append(delta.Parts, Part{FunctionCall: &call})
	}
	if len(delta.Parts) > 0 {
		resp.ConversationDelta = []Content{delta}
	}
	return resp
}

func extractOpenAIText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		chunks := make([]string, 0, len(parts))
		for _, part := range parts {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				chunks = append(chunks, strings.TrimSpace(part.Text))
			}
		}
		return strings.TrimSpace(strings.Join(chunks, "\n"))
	}

	return ""
}

func schemaToMap(schema *Schema) any {
	if schema == nil {
		return nil
	}
	m := map[string]any{}
	if schema.Type != "" {
		m["type"] = strings.ToLower(schema.Type)
	}
	if schema.Description != "" {
		m["description"] = schema.Description
	}
	if len(schema.Required) > 0 {
		m["required"] = schema.Required
	}
	if len(schema.Properties) > 0 {
		props := map[string]any{}
		for k, v := range schema.Properties {
			props[k] = schemaToMap(v)
		}
		m["properties"] = props
	}
	return m
}

func firstNonEmptyCompat(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
