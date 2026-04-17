package llm

type Config struct {
	Backend  string
	Model    string
	APIKey   string
	BaseURL  string
	Token    string
	Project  string
	Location string
	Proxy    string
}

type Request struct {
	Model    string
	Contents []Content
	Tools    []Tool
}

type Response struct {
	Text              string
	FunctionCalls     []FunctionCall
	ConversationDelta []Content
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
