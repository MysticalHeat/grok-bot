package llm

type Config struct {
	APIKey string
}

type Request struct {
	Model          string
	MaxTokens      int
	Contents       []Content
	Tools          []Tool
	ResponseSchema *Schema
}

type Response struct {
	Text              string
	Usage             *Usage
	FunctionCalls     []FunctionCall
	ConversationDelta []Content
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
	Cost             *float64
}

type Content struct {
	Role  string
	Parts []Part
}

type Part struct {
	Text             string
	Data             []byte
	MimeType         string
	FunctionCall     *FunctionCall
	FunctionResponse *FunctionResponse
}

type Tool struct {
	Name        string
	Description string
	Parameters  *Schema
}

type Schema struct {
	Type        string
	Description string
	Properties  map[string]*Schema
	Required    []string
	Items       *Schema
}

type FunctionCall struct {
	ID   string
	Name string
	Args map[string]any
}

type FunctionResponse struct {
	ID       string
	Name     string
	Response map[string]any
}
