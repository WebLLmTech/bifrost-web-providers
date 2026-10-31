package core

import (
	"encoding/json"
	"fmt"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"sort"
	"strings"
)

// MergeMessages preserves system instructions, flattens history into a user
// envelope and resolves tool results against the most recent matching call.
func MergeMessages(messages []Message, cfg ChatConfig) []Message {
	var out []Message
	var history strings.Builder
	names := map[string]string{}
	for _, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			out = append(out, Message{Role: "system", Content: m.Content})
			continue
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, call := range m.ToolCalls {
				names[call.ID] = call.Function.Name
			}
			if m.Content.Text() == "" {
				continue
			}
		}
		text := m.Content.Text()
		if m.Role == "tool" {
			name := names[m.ToolCallID]
			if name == "" {
				name = m.ToolCallID
			}
			fmt.Fprintf(&history, "<message>user\n[ToolResults]\n[Result:%s]\n[ToolResult]\n%s\n[/ToolResult]\n[/Result]\n[/ToolResults]</message>\n", name, text)
		} else {
			fmt.Fprintf(&history, "<message>%s\n%s</message>\n", m.Role, text)
		}
	}
	var prompt string
	if cfg.ToolProtocol != nil {
		prompt = cfg.ToolProtocol.Prompt(cfg)
	}
	if cfg.ResponseFormat.Type != "" && cfg.ResponseFormat.Type != "text" {
		prompt += "\nReturn only a valid JSON object. Do not wrap it in Markdown.\n"
		if len(cfg.ResponseFormat.JSONSchema) > 0 {
			prompt += "JSON schema specification: " + string(cfg.ResponseFormat.JSONSchema) + "\n"
		}
	}
	if strings.TrimSpace(prompt) != "" {
		if len(out) == 0 {
			out = append(out, Message{Role: "system", Content: TextContent(prompt)})
		} else {
			out[0].Content = TextContent(out[0].Content.Text() + "\n\n" + prompt)
		}
	}
	out = append(out, Message{Role: "user", Content: TextContent(history.String())})
	return out
}
func HistoryText(messages []Message) string {
	last := -1
	for i, m := range messages {
		if m.Role == "user" {
			last = i
		}
	}
	var b strings.Builder
	for i, m := range messages {
		if i == last || m.Content.Text() == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s]\n%s\n", m.Role, m.Content.Text())
	}
	return b.String()
}
func NormalizeJSON(text string, format ResponseFormat) (string, error) {
	if format.Type == "" || format.Type == "text" {
		return text, nil
	}
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 && strings.HasSuffix(s, "```") {
			s = strings.TrimSpace(strings.TrimSuffix(s[i+1:], "```"))
		}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &obj) != nil || obj == nil {
		return "", Upstream(502, "invalid_json_output", "Provider did not return a valid JSON object")
	}
	if format.Type == "json_schema" {
		schema, err := responseSchema(format)
		if err != nil {
			return "", err
		}
		value, err := jsonschema.UnmarshalJSON(strings.NewReader(s))
		if err != nil || schema.Validate(value) != nil {
			return "", Upstream(502, "json_schema_mismatch", "Provider output does not match the requested JSON schema")
		}
	}
	b, _ := json.Marshal(obj)
	return string(b), nil
}

// PromptText renders the shared history envelope, including assistant function
// calls. Both web providers use the same format; only the tool protocol differs.
func PromptText(messages []Message, cfg ChatConfig) (string, error) {
	prepared := append([]Message(nil), messages...)
	if cfg.ToolProtocol != nil {
		p := cfg.ToolProtocol
		m := p.Markers
		for i, msg := range prepared {
			if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
				continue
			}
			var b strings.Builder
			b.WriteString(msg.Content.Text())
			for _, call := range msg.ToolCalls {
				var args map[string]json.RawMessage
				if call.Function.Name == "" || json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
					return "", Invalid("assistant tool arguments must be a JSON object")
				}
				b.WriteString("\n" + m.Start + "\n")
				if p.Mode == "delimited" {
					b.WriteString(m.NameStart + call.Function.Name + m.NameEnd + "\n" + m.ArgsStart + call.Function.Arguments + m.ArgsEnd + "\n")
				} else {
					b.WriteString("[Call:" + call.Function.Name + "]\n")
					keys := make([]string, 0, len(args))
					for key := range args {
						keys = append(keys, key)
					}
					sort.Strings(keys)
					for _, key := range keys {
						b.WriteString("[CallParameter:" + key + "]\n" + string(args[key]) + "\n[/CallParameter]\n")
					}
					b.WriteString("[/Call]\n")
				}
				b.WriteString(m.End + "\n")
			}
			prepared[i].Content = TextContent(b.String())
		}
	}
	var b strings.Builder
	for _, msg := range MergeMessages(prepared, cfg) {
		if msg.Role == "system" {
			fmt.Fprintf(&b, "<message>system\n%s</message>\n", msg.Content.Text())
		} else {
			b.WriteString(msg.Content.Text())
		}
	}
	return b.String(), nil
}
