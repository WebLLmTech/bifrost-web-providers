package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

func NewID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cryptographic random source unavailable")
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

type ToolMarkers struct{ Start, End, NameStart, NameEnd, ArgsStart, ArgsEnd string }
type ToolProtocol struct {
	Mode    string
	Markers ToolMarkers
}

func NewToolProtocol(mode string) (*ToolProtocol, error) {
	switch mode {
	case "bracket":
		return &ToolProtocol{Mode: mode, Markers: ToolMarkers{Start: "[ToolCalls]", End: "[/ToolCalls]"}}, nil
	case "delimited":
		id := NewID("tc")
		return &ToolProtocol{Mode: mode, Markers: ToolMarkers{Start: "<" + id + ">", End: "</" + id + ">", NameStart: "<name>", NameEnd: "</name>", ArgsStart: "<arguments>", ArgsEnd: "</arguments>"}}, nil
	default:
		return nil, Unsupported("Unknown tool prompt protocol")
	}
}
func (p *ToolProtocol) Prompt(c ChatConfig) string {
	if len(c.Tools) == 0 || string(c.ToolChoice) == "\"none\"" {
		return ""
	}
	var b strings.Builder
	b.WriteString("Use the following tool interface only when a tool is needed. Never invent a tool result.\n")
	for _, t := range c.Tools {
		fmt.Fprintf(&b, "Tool %s: %s\nArguments JSON schema: %s\n", t.Function.Name, t.Function.Description, t.Function.Parameters)
	}
	if !c.ParallelToolCalls {
		b.WriteString("Call at most one tool per response.\n")
	}
	if string(c.ToolChoice) == "\"required\"" {
		b.WriteString("You must call at least one tool.\n")
	}
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(c.ToolChoice, &named) == nil && named.Function.Name != "" {
		fmt.Fprintf(&b, "Call only %s.\n", named.Function.Name)
	}
	if p.Mode == "delimited" {
		m := p.Markers
		fmt.Fprintf(&b, "Output one or more blocks, then stop:\n%s%sfunction_name%s%s{\"param\":\"value\"}%s%s\n", m.Start, m.NameStart, m.NameEnd, m.ArgsStart, m.ArgsEnd, m.End)
	} else {
		b.WriteString("[ToolCalls]\n[Call:function_name]\n[CallParameter:param]\nvalue\n[/CallParameter]\n[/Call]\n[/ToolCalls]\n")
	}
	return b.String()
}
func between(s, start, end string) (string, string, bool) {
	i := strings.Index(s, start)
	if i < 0 {
		return "", s, false
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return "", s, false
	}
	return rest[:j], rest[j+len(end):], true
}
func (p *ToolProtocol) Parse(block string) ([]ToolCall, error) {
	var calls []ToolCall
	if p.Mode == "delimited" {
		name, _, ok := between(block, p.Markers.NameStart, p.Markers.NameEnd)
		if !ok {
			return nil, Upstream(502, "invalid_tool_call", "Tool name marker missing")
		}
		args, _, ok := between(block, p.Markers.ArgsStart, p.Markers.ArgsEnd)
		if !ok {
			return nil, Upstream(502, "invalid_tool_call", "Tool arguments marker missing")
		}
		calls = append(calls, ToolCall{ID: NewID("call"), Type: "function", Function: ToolFunction{strings.TrimSpace(name), strings.TrimSpace(args)}})
	} else {
		for {
			start := strings.Index(block, "[Call:")
			if start < 0 {
				break
			}
			rest := block[start+6:]
			name, rest, ok := strings.Cut(rest, "]")
			if !ok {
				return nil, Upstream(502, "invalid_tool_call", "Invalid bracket tool name")
			}
			content, after, ok := strings.Cut(rest, "[/Call]")
			if !ok {
				return nil, Upstream(502, "invalid_tool_call", "Unclosed bracket tool call")
			}
			args := map[string]any{}
			for {
				at := strings.Index(content, "[CallParameter:")
				if at < 0 {
					break
				}
				k, remaining, ok := strings.Cut(content[at+15:], "]")
				if !ok {
					return nil, Upstream(502, "invalid_tool_call", "Invalid tool parameter")
				}
				v, next, ok := strings.Cut(remaining, "[/CallParameter]")
				if !ok {
					return nil, Upstream(502, "invalid_tool_call", "Unclosed tool parameter")
				}
				v = strings.TrimSpace(v)
				if strings.HasPrefix(v, "```") {
					if i := strings.Index(v, "\n"); i >= 0 {
						v = strings.TrimSpace(strings.TrimSuffix(v[i+1:], "```"))
					}
				}
				var parsed any
				if json.Unmarshal([]byte(v), &parsed) == nil {
					args[k] = parsed
				} else {
					args[k] = v
				}
				content = next
			}
			b, _ := json.Marshal(args)
			calls = append(calls, ToolCall{ID: NewID("call"), Type: "function", Function: ToolFunction{strings.TrimSpace(name), string(b)}})
			block = after
		}
	}
	if len(calls) == 0 {
		return nil, Upstream(502, "invalid_tool_call", "Empty tool block")
	}
	for _, call := range calls {
		var args map[string]json.RawMessage
		if call.Function.Name == "" || json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
			return nil, Upstream(502, "invalid_tool_arguments", "Tool arguments must be a JSON object")
		}
	}
	return calls, nil
}

// ToolBuffer holds only incomplete marker prefixes and actual tool blocks;
// ordinary text is emitted immediately, including multibyte/fragmented markers.
type ToolBuffer struct {
	Protocol *ToolProtocol
	pending  string
	inside   bool
	max      int
	next     int
}

func NewToolBuffer(p *ToolProtocol, max int) *ToolBuffer {
	if max < 1 {
		max = 4 << 20
	}
	return &ToolBuffer{Protocol: p, max: max}
}
func (b *ToolBuffer) Push(text string) ([]Event, error) {
	b.pending += text
	if len(b.pending) > b.max {
		return nil, Upstream(502, "tool_buffer_limit", "Tool block exceeds configured limit")
	}
	var out []Event
	m := b.Protocol.Markers
	for {
		if b.inside {
			at := strings.Index(b.pending, m.End)
			if at < 0 {
				break
			}
			calls, err := b.Protocol.Parse(b.pending[:at])
			if err != nil {
				return nil, err
			}
			b.pending = b.pending[at+len(m.End):]
			b.inside = false
			for _, call := range calls {
				out = append(out, Event{Kind: EventToolCall, ToolCall: &ToolCallDelta{Index: b.next, ID: call.ID, Type: call.Type, Function: call.Function}})
				b.next++
			}
			continue
		}
		at := strings.Index(b.pending, m.Start)
		if at >= 0 {
			if at > 0 {
				out = append(out, Event{Kind: EventContent, Text: b.pending[:at]})
			}
			b.pending = b.pending[at+len(m.Start):]
			b.inside = true
			continue
		}
		keep := 0
		for n := min(len(m.Start)-1, len(b.pending)); n > 0; n-- {
			if strings.HasSuffix(b.pending, m.Start[:n]) {
				keep = n
				break
			}
		}
		safe := len(b.pending) - keep
		if safe > 0 {
			out = append(out, Event{Kind: EventContent, Text: b.pending[:safe]})
			b.pending = b.pending[safe:]
		}
		break
	}
	return out, nil
}
func (b *ToolBuffer) Flush() ([]Event, error) {
	if b.inside {
		return nil, Upstream(502, "truncated_tool_call", "Tool block ended before its closing marker")
	}
	if b.pending == "" {
		return nil, nil
	}
	out := []Event{{Kind: EventContent, Text: b.pending}}
	b.pending = ""
	return out, nil
}
func ValidateToolCalls(calls []ToolCall, c ChatConfig) error {
	if string(c.ToolChoice) == "\"none\"" && len(calls) > 0 {
		return Upstream(502, "unexpected_tool_call", "Tool call returned when tool_choice is none")
	}
	if !c.ParallelToolCalls && len(calls) > 1 {
		return Upstream(502, "parallel_tools_disabled", "Provider returned multiple calls when parallel_tool_calls is false")
	}
	if string(c.ToolChoice) == "\"required\"" && len(calls) == 0 {
		return Upstream(502, "required_tool_missing", "Provider did not return a required tool call")
	}
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	_ = json.Unmarshal(c.ToolChoice, &named)
	if named.Function.Name != "" && len(calls) == 0 {
		return Upstream(502, "required_tool_missing", "Provider did not return the selected tool call")
	}
	for _, call := range calls {
		allowed := false
		for _, t := range c.Tools {
			if t.Function.Name == call.Function.Name {
				allowed = true
				break
			}
		}
		if !allowed {
			return Upstream(502, "unknown_tool", "Provider returned an undeclared tool")
		}
		for _, tool := range c.Tools {
			if tool.Function.Name == call.Function.Name && len(tool.Function.Parameters) > 0 {
				schema, err := compileSchema(tool.Function.Parameters)
				if err != nil {
					return err
				}
				var value any
				if json.Unmarshal([]byte(call.Function.Arguments), &value) != nil || schema.Validate(value) != nil {
					return Upstream(502, "tool_schema_mismatch", "Tool arguments do not match the declared JSON schema")
				}
			}
		}
		if named.Function.Name != "" && named.Function.Name != call.Function.Name {
			return Upstream(502, "wrong_tool", "Provider returned a tool different from tool_choice")
		}
		var args map[string]any
		if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
			return Upstream(502, "invalid_tool_arguments", "Provider returned invalid tool arguments")
		}
	}
	return nil
}
