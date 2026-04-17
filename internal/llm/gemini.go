package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/genai"
)

type GeminiClient struct{ client *genai.Client }

func newGeminiHTTPClient(proxy string) (*http.Client, error) {
	transport := &http.Transport{}
	if strings.TrimSpace(proxy) != "" {
		proxyURL, err := url.Parse(strings.TrimSpace(proxy))
		if err != nil {
			return nil, fmt.Errorf("parse proxy: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: transport}, nil
}

func newGeminiClient(ctx context.Context, cfg Config, backend genai.Backend) (*GeminiClient, error) {
	opts := &genai.ClientConfig{Backend: backend}
	if backend == genai.BackendGeminiAPI {
		httpClient, err := newGeminiHTTPClient(cfg.Proxy)
		if err != nil {
			return nil, err
		}
		opts.APIKey = cfg.APIKey
		opts.HTTPClient = httpClient
	} else {
		opts.Project = cfg.Project
		opts.Location = cfg.Location
	}
	client, err := genai.NewClient(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &GeminiClient{client: client}, nil
}

func (g *GeminiClient) Generate(ctx context.Context, req Request) (Response, error) {
	contents, err := toGenAIContents(req.Contents)
	if err != nil {
		return Response{}, err
	}

	result, err := g.client.Models.GenerateContent(ctx, req.Model, contents, generationConfig(req.Tools))
	if err != nil {
		return Response{}, err
	}

	response := Response{
		Text: strings.TrimSpace(result.Text()),
	}

	for _, functionCall := range result.FunctionCalls() {
		if functionCall == nil {
			continue
		}
		response.FunctionCalls = append(response.FunctionCalls, FunctionCall{
			ID:   functionCall.ID,
			Name: functionCall.Name,
			Args: functionCall.Args,
		})
	}

	if candidateContent := firstCandidateContent(result); candidateContent != nil {
		converted, err := fromGenAIContent(candidateContent)
		if err != nil {
			return Response{}, err
		}
		response.ConversationDelta = []Content{converted}
	}

	return response, nil
}

func ResolveModelName(ctx context.Context, cfg Config, requested string, preferred []string) (string, error) {
	backend := normalizeBackend(cfg.Backend)
	if backend != "gemini" && backend != "vertex-gemini" {
		requested = strings.TrimSpace(firstNonEmptyCompat(requested, cfg.Model))
		if requested == "" {
			return "", errors.New("model name is required for configured backend")
		}
		return requested, nil
	}

	client, err := NewClient(ctx, cfg)
	if err != nil {
		return "", err
	}

	geminiClient, ok := client.(*GeminiClient)
	if !ok {
		return "", fmt.Errorf("model resolution is unsupported for backend %q", cfg.Backend)
	}

	availableModels, err := listGenerateContentModels(ctx, geminiClient.client)
	if err != nil {
		return "", err
	}

	if len(availableModels) == 0 {
		return "", errors.New("no generateContent models available for configured backend")
	}

	if requested != "" {
		if resolved := findMatchingModel(requested, availableModels); resolved != "" {
			return resolved, nil
		}
	}

	for _, candidate := range preferred {
		if resolved := findMatchingModel(candidate, availableModels); resolved != "" {
			return resolved, nil
		}
	}

	return availableModels[0], nil
}

func generationConfig(tools []Tool) *genai.GenerateContentConfig {
	config := &genai.GenerateContentConfig{}
	if len(tools) == 0 {
		return config
	}

	toolDeclarations := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, tool := range tools {
		toolDeclarations = append(toolDeclarations, &genai.FunctionDeclaration{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  toGenAISchema(tool.Parameters),
		})
	}

	config.Tools = []*genai.Tool{{FunctionDeclarations: toolDeclarations}}
	config.ToolConfig = &genai.ToolConfig{
		FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto},
	}

	return config
}

func toGenAIContents(contents []Content) ([]*genai.Content, error) {
	result := make([]*genai.Content, 0, len(contents))
	for _, content := range contents {
		var role genai.Role = genai.RoleUser
		switch strings.ToLower(strings.TrimSpace(content.Role)) {
		case "assistant", "model":
			role = genai.RoleModel
		}

		parts := make([]*genai.Part, 0, len(content.Parts))
		for _, part := range content.Parts {
			switch {
			case part.FunctionCall != nil:
				parts = append(parts, genai.NewPartFromFunctionCall(part.FunctionCall.Name, part.FunctionCall.Args))
			case part.FunctionResponse != nil:
				parts = append(parts, genai.NewPartFromFunctionResponse(part.FunctionResponse.Name, part.FunctionResponse.Response))
			case len(part.Data) > 0:
				parts = append(parts, genai.NewPartFromBytes(part.Data, part.MimeType))
			default:
				parts = append(parts, genai.NewPartFromText(part.Text))
			}
		}

		result = append(result, genai.NewContentFromParts(parts, role))
	}

	return result, nil
}

func fromGenAIContent(content *genai.Content) (Content, error) {
	if content == nil {
		return Content{}, errors.New("nil content")
	}

	result := Content{Role: string(content.Role)}
	for _, part := range content.Parts {
		if part == nil {
			continue
		}

		switch {
		case part.Text != "":
			result.Parts = append(result.Parts, Part{Text: part.Text})
		case part.FunctionCall != nil:
			result.Parts = append(result.Parts, Part{FunctionCall: &FunctionCall{ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Args: part.FunctionCall.Args}})
		case part.FunctionResponse != nil:
			result.Parts = append(result.Parts, Part{FunctionResponse: &FunctionResponse{ID: part.FunctionResponse.ID, Name: part.FunctionResponse.Name, Response: part.FunctionResponse.Response}})
		case part.InlineData != nil:
			result.Parts = append(result.Parts, Part{Data: part.InlineData.Data, MimeType: part.InlineData.MIMEType})
		}
	}

	return result, nil
}

func toGenAISchema(schema *Schema) *genai.Schema {
	if schema == nil {
		return nil
	}

	result := &genai.Schema{
		Description: schema.Description,
		Required:    schema.Required,
	}

	switch strings.ToUpper(strings.TrimSpace(schema.Type)) {
	case "OBJECT":
		result.Type = genai.TypeObject
	case "STRING":
		result.Type = genai.TypeString
	case "NUMBER":
		result.Type = genai.TypeNumber
	case "INTEGER":
		result.Type = genai.TypeInteger
	case "BOOLEAN":
		result.Type = genai.TypeBoolean
	case "ARRAY":
		result.Type = genai.TypeArray
	}

	if len(schema.Properties) > 0 {
		result.Properties = make(map[string]*genai.Schema, len(schema.Properties))
		for key, value := range schema.Properties {
			result.Properties[key] = toGenAISchema(value)
		}
	}

	return result
}

func firstCandidateContent(response *genai.GenerateContentResponse) *genai.Content {
	if response == nil || len(response.Candidates) == 0 {
		return nil
	}

	return response.Candidates[0].Content
}

func listGenerateContentModels(ctx context.Context, client *genai.Client) ([]string, error) {
	modelSet := make(map[string]struct{})

	for model, err := range client.Models.All(ctx) {
		if err != nil {
			return nil, err
		}

		if model == nil || model.Name == "" {
			continue
		}

		if !supportsGenerateContent(model.SupportedActions) {
			continue
		}

		name := strings.TrimSpace(strings.TrimPrefix(model.Name, "models/"))
		if name == "" {
			continue
		}

		modelSet[name] = struct{}{}
	}

	models := make([]string, 0, len(modelSet))
	for name := range modelSet {
		models = append(models, name)
	}

	return models, nil
}

func supportsGenerateContent(actions []string) bool {
	for _, action := range actions {
		if strings.EqualFold(action, "generateContent") {
			return true
		}
	}

	return false
}

func findMatchingModel(requested string, available []string) string {
	requested = strings.TrimSpace(strings.TrimPrefix(requested, "models/"))
	if requested == "" {
		return ""
	}

	for _, name := range available {
		if name == requested {
			return name
		}
	}

	for _, name := range available {
		if strings.HasPrefix(name, requested) {
			return name
		}
	}

	for _, name := range available {
		if strings.Contains(name, requested) {
			return name
		}
	}

	return ""
}
