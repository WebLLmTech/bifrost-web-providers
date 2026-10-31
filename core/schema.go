package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type noExternalSchema struct{}

func (noExternalSchema) Load(string) (any, error) {
	return nil, errors.New("external schema references are disabled")
}
func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, Invalid("Invalid JSON schema")
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noExternalSchema{})
	if c.AddResource("https://schema.invalid/request.json", doc) != nil {
		return nil, Invalid("Invalid JSON schema resource")
	}
	schema, err := c.Compile("https://schema.invalid/request.json")
	if err != nil {
		return nil, Invalid("Invalid or unsupported JSON schema")
	}
	return schema, nil
}
func responseSchema(format ResponseFormat) (*jsonschema.Schema, error) {
	var wrapper struct {
		Schema json.RawMessage `json:"schema"`
	}
	if json.Unmarshal(format.JSONSchema, &wrapper) != nil || len(wrapper.Schema) == 0 {
		return nil, Invalid("json_schema must contain a schema")
	}
	return compileSchema(wrapper.Schema)
}
func ValidateConfig(c ChatConfig) error {
	if c.ResponseFormat.Type == "json_schema" {
		if _, err := responseSchema(c.ResponseFormat); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, t := range c.Tools {
		if t.Type != "function" || t.Function.Name == "" || seen[t.Function.Name] {
			return Invalid("Tools must be uniquely named functions")
		}
		seen[t.Function.Name] = true
		if len(t.Function.Parameters) > 0 {
			if _, err := compileSchema(t.Function.Parameters); err != nil {
				return err
			}
		}
	}
	if len(c.ToolChoice) > 0 {
		var choice string
		if json.Unmarshal(c.ToolChoice, &choice) == nil {
			switch choice {
			case "auto", "none":
			case "required":
				if len(c.Tools) == 0 {
					return Invalid("tool_choice required needs tools")
				}
			default:
				return Invalid("Invalid tool_choice")
			}
		} else {
			var named struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(c.ToolChoice, &named) != nil || named.Type != "function" || !seen[named.Function.Name] {
				return Invalid("tool_choice must name a declared function")
			}
		}
	}
	return nil
}
