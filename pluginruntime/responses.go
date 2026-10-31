package pluginruntime

import (
	"encoding/json"
	"github.com/WebLLmTech/bifrost-web-providers/core"
	"io"
	"sort"
)

// Responses is deliberately stateless. It shares the exact Provider contract
// and lifecycle with Chat, rather than invoking a provider a second time.
func readResponses(reader io.Reader) (core.ChatRequest, error) {
	var input struct {
		Model        string          `json:"model"`
		Input        json.RawMessage `json:"input"`
		Instructions string          `json:"instructions"`
		Stream       bool            `json:"stream"`
		Store        bool            `json:"store"`
		Previous     string          `json:"previous_response_id"`
		Conversation json.RawMessage `json:"conversation"`
		Background   bool            `json:"background"`
		Tools        []struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      bool            `json:"strict"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
		Parallel   *bool           `json:"parallel_tool_calls"`
		Text       struct {
			Format json.RawMessage `json:"format"`
		} `json:"text"`
	}
	d := json.NewDecoder(reader)
	if err := d.Decode(&input); err != nil {
		return core.ChatRequest{}, err
	}
	var tail any
	if d.Decode(&tail) != io.EOF {
		return core.ChatRequest{}, core.Invalid("Expected one JSON object")
	}
	if input.Store || input.Previous != "" || input.Background || (len(input.Conversation) > 0 && string(input.Conversation) != "null") {
		return core.ChatRequest{}, core.Unsupported("Stored/background Responses and previous_response_id are not supported; send full input history")
	}
	r := core.ChatRequest{Model: input.Model, Stream: input.Stream, ToolChoice: input.ToolChoice, ParallelToolCalls: input.Parallel}
	if input.Instructions != "" {
		r.Messages = append(r.Messages, core.Message{Role: "system", Content: core.TextContent(input.Instructions)})
	}
	var text string
	if json.Unmarshal(input.Input, &text) == nil {
		r.Messages = append(r.Messages, core.Message{Role: "user", Content: core.TextContent(text)})
	} else {
		var items []struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		if json.Unmarshal(input.Input, &items) != nil {
			return r, core.Invalid("Responses input must be text or an item array")
		}
		for _, item := range items {
			switch item.Type {
			case "function_call":
				r.Messages = append(r.Messages, core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: item.CallID, Type: "function", Function: core.ToolFunction{Name: item.Name, Arguments: item.Arguments}}}})
			case "function_call_output":
				var output string
				if json.Unmarshal(item.Output, &output) != nil {
					output = string(item.Output)
				}
				r.Messages = append(r.Messages, core.Message{Role: "tool", ToolCallID: item.CallID, Content: core.TextContent(output)})
			case "message", "":
				role := item.Role
				if role == "" {
					role = "user"
				}
				content := item.Content
				var parts []map[string]json.RawMessage
				if json.Unmarshal(content, &parts) == nil {
					for _, p := range parts {
						var kind string
						_ = json.Unmarshal(p["type"], &kind)
						switch kind {
						case "input_text", "output_text":
							p["type"] = json.RawMessage(`"text"`)
						case "input_image":
							p["type"] = json.RawMessage(`"image_url"`)
							var imageURL string
							if json.Unmarshal(p["image_url"], &imageURL) == nil {
								p["image_url"], _ = json.Marshal(map[string]string{"url": imageURL})
							}
						case "input_file":
							p["type"] = json.RawMessage(`"file"`)
							p["file"], _ = json.Marshal(map[string]json.RawMessage{"filename": p["filename"], "file_data": p["file_data"], "file_id": p["file_id"]})
							delete(p, "filename")
							delete(p, "file_data")
							delete(p, "file_id")
						}
					}
					content, _ = json.Marshal(parts)
				}
				r.Messages = append(r.Messages, core.Message{Role: role, Content: core.Content(content)})
			default:
				return r, core.Unsupported("Unsupported Responses input item")
			}
		}
	}
	for _, t := range input.Tools {
		if t.Type != "function" {
			return r, core.Unsupported("Only function tools are supported by web providers")
		}
		r.Tools = append(r.Tools, core.Tool{Type: "function", Function: core.Function{Name: t.Name, Description: t.Description, Parameters: t.Parameters, Strict: t.Strict}})
	}
	var named struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(r.ToolChoice, &named) == nil && named.Type == "function" {
		r.ToolChoice, _ = json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": named.Name}})
	}
	if len(input.Text.Format) > 0 {
		var format struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(input.Text.Format, &format) != nil {
			return r, core.Invalid("Invalid text.format")
		}
		r.ResponseFormat = &core.ResponseFormat{Type: format.Type}
		if format.Type == "json_schema" {
			r.ResponseFormat.JSONSchema = input.Text.Format
		}
	}
	return r, nil
}
func responseObject(id, model string, created int64, c core.Completion) map[string]any {
	var output []any
	if c.Reasoning != "" {
		output = append(output, map[string]any{"id": core.NewID("rs"), "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": c.Reasoning}}})
	}
	if c.Content != "" || len(c.ToolCalls) == 0 {
		output = append(output, map[string]any{"id": core.NewID("msg"), "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": c.Content, "annotations": []any{}}}})
	}
	for _, call := range c.ToolCalls {
		output = append(output, map[string]any{"id": core.NewID("fc"), "type": "function_call", "status": "completed", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
	}
	return responseWithOutput(id, model, created, c, output)
}
func responseWithOutput(id, model string, created int64, c core.Completion, output []any) map[string]any {
	response := map[string]any{"id": id, "object": "response", "created_at": created, "status": "completed", "model": model, "output": output, "error": nil, "incomplete_details": nil, "parallel_tool_calls": true}
	if c.Usage != nil {
		response["usage"] = map[string]int{"input_tokens": c.Usage.PromptTokens, "output_tokens": c.Usage.CompletionTokens, "total_tokens": c.Usage.TotalTokens}
	}
	if c.FinishReason == "length" {
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
	}
	if len(c.Citations) > 0 {
		response["citations"] = core.CitationURLs(c.Citations)
	}
	return response
}

type responseStream struct {
	id, model              string
	created                int64
	sequence               int
	write                  func(string, any) error
	output                 []any
	textIndex, reasonIndex int
	tools                  map[int]int
	parallel               bool
}

func newResponseStream(id, model string, created int64, write func(string, any) error) *responseStream {
	return &responseStream{id: id, model: model, created: created, write: write, textIndex: -1, reasonIndex: -1, tools: map[int]int{}}
}
func (s *responseStream) emit(kind string, data map[string]any) error {
	data["type"] = kind
	data["sequence_number"] = s.sequence
	s.sequence++
	return s.write(kind, data)
}
func (s *responseStream) start() error {
	r := map[string]any{"id": s.id, "object": "response", "status": "in_progress", "model": s.model, "created_at": s.created, "output": []any{}}
	if err := s.emit("response.created", map[string]any{"response": r}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{"response": r})
}
func (s *responseStream) item(kind string, index *int) error {
	if *index >= 0 {
		return nil
	}
	*index = len(s.output)
	id := core.NewID("msg")
	item := map[string]any{"id": id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
	if kind == "reasoning" {
		item = map[string]any{"id": core.NewID("rs"), "type": "reasoning", "summary": []any{}}
	}
	s.output = append(s.output, item)
	if err := s.emit("response.output_item.added", map[string]any{"output_index": *index, "item": item}); err != nil {
		return err
	}
	if kind == "reasoning" {
		return s.emit("response.reasoning_summary_part.added", map[string]any{"output_index": *index, "item_id": item["id"], "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
	}
	return s.emit("response.content_part.added", map[string]any{"output_index": *index, "item_id": id, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
}
func (s *responseStream) event(e core.Event, format string) error {
	switch e.Kind {
	case core.EventContent:
		if format != "" && format != "text" {
			return nil
		}
		return s.text(e.Text)
	case core.EventReasoning:
		if err := s.item("reasoning", &s.reasonIndex); err != nil {
			return err
		}
		item := s.output[s.reasonIndex].(map[string]any)
		return s.emit("response.reasoning_summary_text.delta", map[string]any{"output_index": s.reasonIndex, "item_id": item["id"], "summary_index": 0, "delta": e.Text})
	case core.EventToolCall:
		d := e.ToolCall
		at, ok := s.tools[d.Index]
		if !ok {
			at = len(s.output)
			s.tools[d.Index] = at
			item := map[string]any{"id": core.NewID("fc"), "type": "function_call", "call_id": d.ID, "name": d.Function.Name, "arguments": "", "status": "in_progress"}
			s.output = append(s.output, item)
			if err := s.emit("response.output_item.added", map[string]any{"output_index": at, "item": item}); err != nil {
				return err
			}
		}
		item := s.output[at].(map[string]any)
		return s.emit("response.function_call_arguments.delta", map[string]any{"output_index": at, "item_id": item["id"], "delta": d.Function.Arguments})
	}
	return nil
}
func (s *responseStream) text(text string) error {
	if err := s.item("text", &s.textIndex); err != nil {
		return err
	}
	item := s.output[s.textIndex].(map[string]any)
	return s.emit("response.output_text.delta", map[string]any{"output_index": s.textIndex, "item_id": item["id"], "content_index": 0, "delta": text})
}
func (s *responseStream) finish(c core.Completion) error {
	if s.textIndex < 0 && (c.Content != "" || len(c.ToolCalls) == 0) {
		if err := s.text(c.Content); err != nil {
			return err
		}
	}
	if s.textIndex >= 0 {
		item := s.output[s.textIndex].(map[string]any)
		part := map[string]any{"type": "output_text", "text": c.Content, "annotations": []any{}}
		item["content"] = []any{part}
		item["status"] = "completed"
		if err := s.emit("response.output_text.done", map[string]any{"output_index": s.textIndex, "item_id": item["id"], "content_index": 0, "text": c.Content}); err != nil {
			return err
		}
		if err := s.emit("response.content_part.done", map[string]any{"output_index": s.textIndex, "item_id": item["id"], "content_index": 0, "part": part}); err != nil {
			return err
		}
	}
	if s.reasonIndex >= 0 {
		item := s.output[s.reasonIndex].(map[string]any)
		part := map[string]any{"type": "summary_text", "text": c.Reasoning}
		item["summary"] = []any{part}
		if err := s.emit("response.reasoning_summary_text.done", map[string]any{"output_index": s.reasonIndex, "item_id": item["id"], "summary_index": 0, "text": c.Reasoning}); err != nil {
			return err
		}
		if err := s.emit("response.reasoning_summary_part.done", map[string]any{"output_index": s.reasonIndex, "item_id": item["id"], "summary_index": 0, "part": part}); err != nil {
			return err
		}
	}
	indices := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for i, index := range indices {
		item := s.output[s.tools[index]].(map[string]any)
		call := c.ToolCalls[i]
		item["call_id"], item["name"], item["arguments"], item["status"] = call.ID, call.Function.Name, call.Function.Arguments, "completed"
		if err := s.emit("response.function_call_arguments.done", map[string]any{"output_index": s.tools[index], "item_id": item["id"], "arguments": call.Function.Arguments}); err != nil {
			return err
		}
	}
	for index, item := range s.output {
		if err := s.emit("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
			return err
		}
	}
	response := responseWithOutput(s.id, s.model, s.created, c, s.output)
	response["parallel_tool_calls"] = s.parallel
	kind := "response.completed"
	if c.FinishReason == "length" {
		kind = "response.incomplete"
	}
	return s.emit(kind, map[string]any{"response": response})
}
