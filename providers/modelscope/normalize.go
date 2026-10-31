package modelscope

import (
	"fmt"
	"github.com/WebLLmTech/bifrost-web-providers/core"
	"net/url"
	"strings"
)

func disabled(v any) bool {
	switch x := v.(type) {
	case bool:
		return !x
	case string:
		return x == "false"
	}
	return false
}
func empty(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}
func validInputImage(s string) bool {
	if strings.HasPrefix(s, "data:image/") {
		return strings.Contains(s, ";base64,")
	}
	u, err := url.Parse(s)
	return err == nil && u.Host != "" && u.User == nil && (u.Scheme == "https" || u.Scheme == "http")
}
func normalizeImage(body map[string]any, editing bool) (string, error) {
	model, _ := body["model"].(string)
	prompt, _ := body["prompt"].(string)
	if model == "" || strings.TrimSpace(prompt) == "" {
		return "", core.Invalid("model and prompt are required")
	}
	for from, to := range map[string]string{"num_inference_steps": "steps", "guidance_scale": "guidance"} {
		if v, ok := body[from]; ok {
			if _, exists := body[to]; exists {
				return "", core.Invalid("Specify only one of " + from + " and " + to)
			}
			body[to] = v
			delete(body, from)
		}
	}
	if value, ok := body["size"].(string); ok && value == "auto" {
		delete(body, "size")
	}
	if v, ok := body["stream"]; ok && !disabled(v) {
		return "", core.Invalid("ModelScope adapter supports synchronous image responses; set stream=false")
	}
	if v, ok := body["mask"]; ok && !empty(v) {
		return "", core.Invalid("ModelScope mask editing is not supported by this adapter")
	}
	if n, ok := body["n"]; ok && fmt.Sprint(n) != "1" {
		return "", core.Invalid("ModelScope adapter currently supports n=1")
	}
	format, _ := body["response_format"].(string)
	if v, present := body["response_format"]; present && v != nil {
		if _, ok := v.(string); !ok {
			return "", core.Invalid("response_format must be url or b64_json")
		}
	}
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return "", core.Invalid("response_format must be url or b64_json")
	}
	// JSON clients can use image, images or image_url; multipart clients (including
	// Bifrost's OpenAI provider) arrive as files and are converted to data URLs.
	for _, alias := range []string{"image", "images"} {
		if v, exists := body[alias]; exists {
			if _, set := body["image_url"]; set {
				return "", core.Invalid("Specify only one of image, images and image_url")
			}
			body["image_url"] = v
			delete(body, alias)
		}
	}
	if image, ok := body["image_url"]; ok {
		var values []string
		switch v := image.(type) {
		case string:
			values = []string{v}
		case []string:
			values = v
		case []any:
			for _, x := range v {
				s, ok := x.(string)
				if !ok {
					return "", core.Invalid("image_url must contain URL or data URL strings")
				}
				values = append(values, s)
			}
		default:
			return "", core.Invalid("image_url must be a string or array of strings")
		}
		if len(values) == 0 {
			return "", core.Invalid("image_url cannot be empty")
		}
		for _, s := range values {
			if !validInputImage(s) {
				return "", core.Invalid("Input images must use HTTP(S) URLs or image data URLs")
			}
		}
		body["image_url"] = values
	} else if editing {
		return "", core.Invalid("At least one input image is required for image editing")
	}
	for _, k := range []string{"response_format", "stream", "n", "user", "mask"} {
		delete(body, k)
	}
	return format, nil
}
