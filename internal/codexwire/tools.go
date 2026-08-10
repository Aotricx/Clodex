package codexwire

import "encoding/json"

// FunctionTool is one Responses function definition.
type FunctionTool struct {
	Name        string
	Description string
	Strict      bool
	Parameters  json.RawMessage
}

func (FunctionTool) tool() {}

func (f FunctionTool) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Strict      bool            `json:"strict"`
		Parameters  json.RawMessage `json:"parameters"`
	}{
		Type:        "function",
		Name:        f.Name,
		Description: f.Description,
		Strict:      f.Strict,
		Parameters:  f.Parameters,
	})
}

// WebSearchTool is the backend-hosted web search tool. Unsupported Anthropic
// server-tool controls are warning-accounted before this wire type is built.
type WebSearchTool struct {
	ExternalWebAccess  *bool
	AllowedDomains     []string
	SearchContentTypes []string
}

func (WebSearchTool) tool() {}

func (tool WebSearchTool) MarshalJSON() ([]byte, error) {
	type filters struct {
		AllowedDomains []string `json:"allowed_domains,omitempty"`
	}
	var toolFilters *filters
	if len(tool.AllowedDomains) > 0 {
		toolFilters = &filters{AllowedDomains: tool.AllowedDomains}
	}
	return json.Marshal(struct {
		Type               string   `json:"type"`
		ExternalWebAccess  *bool    `json:"external_web_access,omitempty"`
		Filters            *filters `json:"filters,omitempty"`
		SearchContentTypes []string `json:"search_content_types,omitempty"`
	}{
		Type:               "web_search",
		ExternalWebAccess:  tool.ExternalWebAccess,
		Filters:            toolFilters,
		SearchContentTypes: tool.SearchContentTypes,
	})
}
