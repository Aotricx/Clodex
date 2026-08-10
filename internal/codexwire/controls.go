package codexwire

import "encoding/json"

// Verbosity is the proven Responses text verbosity value.
type Verbosity string

const (
	VerbosityLow    Verbosity = "low"
	VerbosityMedium Verbosity = "medium"
	VerbosityHigh   Verbosity = "high"
)

// TextControls configures proven Responses text output controls.
type TextControls struct {
	Verbosity Verbosity         `json:"verbosity,omitempty"`
	Format    *JSONSchemaFormat `json:"format,omitempty"`
}

// JSONSchemaFormat requests output matching a JSON schema.
type JSONSchemaFormat struct {
	Strict bool
	Schema json.RawMessage
}

func (f JSONSchemaFormat) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type   string          `json:"type"`
		Strict bool            `json:"strict"`
		Schema json.RawMessage `json:"schema"`
		Name   string          `json:"name"`
	}{
		Type:   "json_schema",
		Strict: f.Strict,
		Schema: f.Schema,
		Name:   "codex_output_schema",
	})
}
