package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type openAICompatClient struct {
	httpClient        *http.Client
	baseURL           string
	bearer            string
	requireOpenAIBYOK bool
}

const OpenRouterBaseURL = "https://openrouter.ai/api/v1"

func newOpenAICompatClient(cfg Config) (Client, error) {
	bearer := strings.TrimSpace(cfg.APIKey)
	if bearer == "" {
		return nil, fmt.Errorf("OpenRouter API key is required")
	}

	return &openAICompatClient{
		httpClient:        &http.Client{},
		baseURL:           OpenRouterBaseURL,
		bearer:            bearer,
		requireOpenAIBYOK: cfg.RequireOpenAIBYOK,
	}, nil
}

func (c *openAICompatClient) Generate(ctx context.Context, req Request) (Response, error) {
	payload, err := toOpenAIChatRequest(req)
	if err != nil {
		return Response{}, err
	}
	if c.requireOpenAIBYOK {
		payload.Provider = &openAIProviderRouting{
			Only:              []string{"openai"},
			AllowFallbacks:    false,
			RequireParameters: true,
		}
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
	httpReq.Header.Set("Authorization", "Bearer "+c.bearer)

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
		retrySeconds, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return Response{}, &APIError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(respBody)), RetryAfter: time.Duration(retrySeconds) * time.Second}
	}

	var decoded openAIChatResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return Response{}, err
	}
	if c.requireOpenAIBYOK && (decoded.Usage == nil || !decoded.Usage.IsBYOK) {
		return Response{}, fmt.Errorf("OpenRouter response did not confirm BYOK usage")
	}
	return decoded.toResponse(), nil
}

type APIError struct {
	StatusCode int
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("OpenRouter request failed: status %d: %s", e.StatusCode, e.Message)
}

type openAIChatRequest struct {
	Model          string                 `json:"model"`
	MaxTokens      int                    `json:"max_tokens,omitempty"`
	Messages       []openAIMessage        `json:"messages"`
	Tools          []openAITool           `json:"tools,omitempty"`
	ToolChoice     string                 `json:"tool_choice,omitempty"`
	Temperature    *float64               `json:"temperature,omitempty"`
	ResponseFormat any                    `json:"response_format,omitempty"`
	Provider       *openAIProviderRouting `json:"provider,omitempty"`
}

type openAIProviderRouting struct {
	Only              []string `json:"only"`
	AllowFallbacks    bool     `json:"allow_fallbacks"`
	RequireParameters bool     `json:"require_parameters"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

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
	Usage *struct {
		PromptTokens     int      `json:"prompt_tokens"`
		CompletionTokens int      `json:"completion_tokens"`
		Cost             *float64 `json:"cost"`
		IsBYOK           bool     `json:"is_byok"`
		CostDetails      struct {
			UpstreamInferenceCost *float64 `json:"upstream_inference_cost"`
		} `json:"cost_details"`
	} `json:"usage"`
	Choices []struct {
		Message      openAIResponseMessage `json:"message"`
		FinishReason string                `json:"finish_reason"`
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
	payload := openAIChatRequest{Model: req.Model, Messages: messages, MaxTokens: req.MaxTokens}
	if req.ResponseSchema != nil {
		payload.ResponseFormat = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "structured_response", "strict": true, "schema": strictSchema(req.ResponseSchema)}}
	}
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
			if !strings.HasPrefix(part.MimeType, "image/") {
				return openAIMessage{}, fmt.Errorf("unsupported binary MIME type %q", part.MimeType)
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + part.MimeType + ";base64," + base64.StdEncoding.EncodeToString(part.Data)}})
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
	text, rawText := extractOpenAIText(m.Content)
	resp := Response{Text: text, RawText: rawText, FinishReason: r.Choices[0].FinishReason}
	if r.Usage != nil {
		cost := r.Usage.Cost
		if r.Usage.IsBYOK {
			fee, upstream := cost, r.Usage.CostDetails.UpstreamInferenceCost
			cost = nil
			if fee != nil && upstream != nil && *fee >= 0 && *upstream >= 0 {
				total := *fee + *upstream
				cost = &total
			}
		}
		resp.Usage = &Usage{PromptTokens: r.Usage.PromptTokens, CompletionTokens: r.Usage.CompletionTokens, Cost: cost}
	}
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

func extractOpenAIText(raw json.RawMessage) (text, rawText string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", ""
	}

	if err := json.Unmarshal(raw, &rawText); err == nil {
		return strings.TrimSpace(rawText), rawText
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", ""
	}

	chunks := make([]string, 0, len(parts))
	var original strings.Builder
	for _, part := range parts {
		if part.Type != "text" {
			continue
		}
		original.WriteString(part.Text)
		if trimmed := strings.TrimSpace(part.Text); trimmed != "" {
			chunks = append(chunks, trimmed)
		}
	}
	return strings.Join(chunks, "\n"), original.String()
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
	if schema.Items != nil {
		m["items"] = schemaToMap(schema.Items)
	}
	return m
}

func strictSchema(schema *Schema) any {
	value, ok := schemaToMap(schema).(map[string]any)
	if !ok {
		return value
	}
	if strings.EqualFold(schema.Type, "object") {
		value["additionalProperties"] = false
		props := make(map[string]any, len(schema.Properties))
		for key, child := range schema.Properties {
			props[key] = strictSchema(child)
		}
		value["properties"] = props
	}
	if schema.Items != nil {
		value["items"] = strictSchema(schema.Items)
	}
	return value
}

func firstNonEmptyCompat(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
