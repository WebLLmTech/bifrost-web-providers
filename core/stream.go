package core

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
)

type Completion struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	Citations    []json.RawMessage
	Usage        *Usage
	FinishReason string
}
type Accumulator struct {
	Result    Completion
	tools     map[int]*ToolCall
	size      int
	MaxBytes  int
	content   strings.Builder
	reasoning strings.Builder
	arguments map[int]*strings.Builder
}

func (a *Accumulator) Add(e Event) error {
	if a.MaxBytes < 1 {
		a.MaxBytes = 16 << 20
	}
	a.size += len(e.Text)
	for _, citation := range e.Citations {
		a.size += len(citation)
	}
	if e.ToolCall != nil {
		a.size += len(e.ToolCall.Function.Arguments)
	}
	if a.size > a.MaxBytes {
		return Upstream(502, "completion_too_large", "Provider completion exceeds configured limit")
	}
	switch e.Kind {
	case EventContent:
		a.content.WriteString(e.Text)
	case EventReasoning:
		a.reasoning.WriteString(e.Text)
	case EventToolCall:
		if e.ToolCall == nil {
			return Upstream(502, "invalid_tool_delta", "Empty tool delta")
		}
		d := e.ToolCall
		if d.Index < 0 || d.Index > 1023 {
			return Upstream(502, "invalid_tool_index", "Tool delta index is out of range")
		}
		if a.tools == nil {
			a.tools = map[int]*ToolCall{}
		}
		t := a.tools[d.Index]
		if t == nil {
			t = &ToolCall{Type: "function"}
			a.tools[d.Index] = t
		}
		if d.ID != "" {
			t.ID = d.ID
		}
		if d.Type != "" {
			t.Type = d.Type
		}
		t.Function.Name += d.Function.Name
		if a.arguments == nil {
			a.arguments = map[int]*strings.Builder{}
		}
		if a.arguments[d.Index] == nil {
			a.arguments[d.Index] = &strings.Builder{}
		}
		a.arguments[d.Index].WriteString(d.Function.Arguments)
	case EventCitations:
		for _, citation := range e.Citations {
			a.Result.Citations = append(a.Result.Citations, append(json.RawMessage(nil), citation...))
		}
	case EventUsage:
		if e.Usage != nil {
			copy := *e.Usage
			a.Result.Usage = &copy
		}
	case EventFinish:
		a.Result.FinishReason = e.FinishReason
	default:
		return Upstream(502, "unknown_event", "Provider returned an unknown stream event")
	}
	return nil
}
func (a *Accumulator) Finish(c ChatConfig) (Completion, error) {
	a.Result.Content = a.content.String()
	a.Result.Reasoning = a.reasoning.String()
	indices := make([]int, 0, len(a.tools))
	for i := range a.tools {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	a.Result.ToolCalls = nil
	for _, i := range indices {
		t := *a.tools[i]
		if b := a.arguments[i]; b != nil {
			t.Function.Arguments = b.String()
		}
		if t.ID == "" {
			t.ID = NewID("call")
		}
		a.Result.ToolCalls = append(a.Result.ToolCalls, t)
	}
	if err := ValidateToolCalls(a.Result.ToolCalls, c); err != nil {
		return Completion{}, err
	}
	if len(a.Result.ToolCalls) > 0 {
		a.Result.FinishReason = "tool_calls"
	} else {
		content, err := NormalizeJSON(a.Result.Content, c.ResponseFormat)
		if err != nil {
			return Completion{}, err
		}
		a.Result.Content = content
	}
	if a.Result.FinishReason == "" {
		a.Result.FinishReason = "stop"
	}
	return a.Result, nil
}

// NormalizeStream performs prompt-tool parsing for both streaming and collected
// output, without forcing providers to duplicate protocol assembly.
type normalStream struct {
	source  EventStream
	buffer  *ToolBuffer
	pending []Event
	done    bool
	finish  string
}

func NormalizeStream(s EventStream, c ChatConfig) EventStream {
	if c.ToolProtocol == nil {
		return s
	}
	return &normalStream{source: s, buffer: NewToolBuffer(c.ToolProtocol, 4<<20)}
}
func (s *normalStream) Close() error { return s.source.Close() }
func (s *normalStream) Next(ctx context.Context) (Event, error) {
	for {
		if len(s.pending) > 0 {
			e := s.pending[0]
			s.pending = s.pending[1:]
			return e, nil
		}
		if s.done {
			return Event{}, io.EOF
		}
		e, err := s.source.Next(ctx)
		if err != nil {
			if err != io.EOF {
				return Event{}, err
			}
			events, err := s.buffer.Flush()
			if err != nil {
				return Event{}, err
			}
			s.pending = events
			s.done = true
			if s.finish != "" {
				s.pending = append(s.pending, Event{Kind: EventFinish, FinishReason: s.finish})
			}
			continue
		}
		if e.Kind == EventContent {
			events, err := s.buffer.Push(e.Text)
			if err != nil {
				return Event{}, err
			}
			s.pending = events
			continue
		}
		if e.Kind == EventFinish {
			s.finish = e.FinishReason
			continue
		}
		return e, nil
	}
}
func Collect(ctx context.Context, s EventStream, c ChatConfig, maxBytes int) (Completion, error) {
	defer s.Close()
	s = NormalizeStream(s, c)
	a := Accumulator{MaxBytes: maxBytes}
	for {
		e, err := s.Next(ctx)
		if err == io.EOF {
			return a.Finish(c)
		}
		if err != nil {
			return Completion{}, err
		}
		if err = a.Add(e); err != nil {
			return Completion{}, err
		}
	}
}
func CompletionMessage(c Completion) map[string]any {
	m := map[string]any{"role": "assistant", "content": c.Content}
	if c.Reasoning != "" {
		m["reasoning_content"] = c.Reasoning
	}
	if len(c.ToolCalls) > 0 {
		m["tool_calls"] = c.ToolCalls
		if strings.TrimSpace(c.Content) == "" {
			m["content"] = nil
		}
	}
	return m
}

// Bifrost's OpenAI wire schema carries root-level citation URL strings. Keep
// richer source JSON in Completion, while normalizing the transport field.
func CitationURLs(citations []json.RawMessage) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range citations {
		var url string
		if json.Unmarshal(raw, &url) != nil {
			var item struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			url = item.URL
		}
		if url != "" && !seen[url] {
			seen[url] = true
			out = append(out, url)
		}
	}
	return out
}
