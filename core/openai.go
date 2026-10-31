package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// OpenAIChatBody preserves native tools, multimodal messages and vendor options.
// Compatible providers do not need the prompt-based web tool protocols.
func OpenAIChatBody(r ChatRequest) ([]byte, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for key, value := range r.Extra {
		if _, known := fields[key]; !known {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

type openAIChunk struct {
	Error     json.RawMessage   `json:"error"`
	Usage     *Usage            `json:"usage"`
	Citations []json.RawMessage `json:"citations"`
	Choices   []struct {
		Index        int           `json:"index"`
		Message      openAIMessage `json:"message"`
		Delta        openAIMessage `json:"delta"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
}
type openAIMessage struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`
	ToolCalls        []struct {
		Index    int          `json:"index"`
		ID       string       `json:"id"`
		Type     string       `json:"type"`
		Function ToolFunction `json:"function"`
	} `json:"tool_calls"`
}

func (c *openAIChunk) events(stream bool) ([]Event, bool, error) {
	if len(c.Error) > 0 && string(c.Error) != "null" {
		return nil, false, Upstream(502, "upstream_error", "OpenAI-compatible provider reported an error")
	}
	var out []Event
	finished := false
	for _, choice := range c.Choices {
		if choice.Index != 0 {
			return nil, false, Unsupported("Multiple completion choices are not supported")
		}
		m := choice.Message
		if stream {
			m = choice.Delta
		}
		reason := m.ReasoningContent
		if reason == "" {
			reason = m.Reasoning
		}
		if reason != "" {
			out = append(out, Event{Kind: EventReasoning, Text: reason})
		}
		if m.Content != "" {
			out = append(out, Event{Kind: EventContent, Text: m.Content})
		}
		for i, call := range m.ToolCalls {
			index := call.Index
			if !stream {
				index = i
			}
			out = append(out, Event{Kind: EventToolCall, ToolCall: &ToolCallDelta{Index: index, ID: call.ID, Type: call.Type, Function: call.Function}})
		}
		if choice.FinishReason != "" {
			finished = true
			out = append(out, Event{Kind: EventFinish, FinishReason: choice.FinishReason})
		}
	}
	if len(c.Citations) > 0 {
		out = append(out, Event{Kind: EventCitations, Citations: c.Citations})
	}
	if c.Usage != nil {
		out = append(out, Event{Kind: EventUsage, Usage: c.Usage})
	}
	return out, finished, nil
}
func OpenAIChatStream(ctx context.Context, client *HTTPClient, req *http.Request, stream bool) (EventStream, error) {
	s := &openAIStream{}
	if stream {
		var err error
		s.sse, err = client.SSE(ctx, req)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	var c openAIChunk
	if err := client.JSON(ctx, req, &c); err != nil {
		return nil, err
	}
	events, finished, err := c.events(false)
	if err != nil {
		return nil, err
	}
	if !finished || len(c.Choices) == 0 {
		return nil, Upstream(502, "invalid_completion", "Provider returned no completed choice")
	}
	s.queue = events
	s.done = true
	return s, nil
}

type openAIStream struct {
	sse      *SSEReader
	queue    []Event
	finished bool
	done     bool
}

func (s *openAIStream) Close() error {
	if s.sse != nil {
		return s.sse.Close()
	}
	return nil
}
func (s *openAIStream) Next(ctx context.Context) (Event, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}
		if len(s.queue) > 0 {
			e := s.queue[0]
			s.queue = s.queue[1:]
			return e, nil
		}
		if s.done {
			return Event{}, io.EOF
		}
		frame, err := s.sse.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Event{}, Upstream(502, "truncated_stream", "OpenAI-compatible stream ended without a done marker")
			}
			return Event{}, err
		}
		if frame.Event == "error" {
			return Event{}, Upstream(502, "upstream_error", "OpenAI-compatible stream reported an error")
		}
		if frame.Data == "" {
			continue
		}
		if frame.Data == "[DONE]" {
			if !s.finished {
				return Event{}, Upstream(502, "truncated_stream", "OpenAI-compatible stream returned no finish reason")
			}
			s.done = true
			continue
		}
		var c openAIChunk
		if json.Unmarshal([]byte(frame.Data), &c) != nil {
			return Event{}, Upstream(502, "invalid_stream", "OpenAI-compatible provider returned invalid JSON")
		}
		events, finished, err := c.events(true)
		if err != nil {
			return Event{}, err
		}
		s.queue = events
		s.finished = s.finished || finished
	}
}
