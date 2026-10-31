package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// ImageProvider is optional. Existing chat providers need not implement it.
type ImageProvider interface {
	Images(context.Context, ImageRequest, RequestContext) (ImageResponse, error)
}
type ImageRequest struct {
	Editing    bool
	Parameters map[string]any
}
type ImageData struct {
	URL     string `json:"url,omitempty"`
	B64JSON string `json:"b64_json,omitempty"`
}
type ImageResponse struct {
	Created int64       `json:"created"`
	Data    []ImageData `json:"data"`
	Headers http.Header `json:"-"`
}

// ReadImageRequest accepts standard JSON and multipart image requests. The
// caller must cap the total request body and close it when context expires.
func ReadImageRequest(r *http.Request, editing bool) (ImageRequest, error) {
	result := ImageRequest{Editing: editing}
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return result, Invalid("A valid Content-Type is required")
	}
	if ct == "application/json" {
		d := json.NewDecoder(r.Body)
		d.UseNumber()
		if err = d.Decode(&result.Parameters); err != nil {
			return result, err
		}
		if result.Parameters == nil {
			return result, Invalid("Image request must be a JSON object")
		}
		if d.Decode(new(any)) != io.EOF {
			return result, Invalid("Expected one JSON object")
		}
		return result, nil
	}
	if ct != "multipart/form-data" {
		return result, Invalid("Use application/json or multipart/form-data")
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return result, Invalid("Invalid multipart request")
	}
	body := map[string]any{}
	var images []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, err
		}
		name := part.FormName()
		image := name == "image" || name == "image[]" || name == "images" || name == "image_url"
		if part.FileName() != "" {
			if name == "mask" {
				part.Close()
				return result, Unsupported("Mask editing is not supported")
			}
			if !image {
				part.Close()
				return result, Invalid("Unknown image file field")
			}
			data, err := io.ReadAll(part)
			part.Close()
			if err != nil {
				return result, err
			}
			contentType := http.DetectContentType(data)
			if !strings.HasPrefix(contentType, "image/") {
				return result, Invalid("Unsupported input image type")
			}
			images = append(images, "data:"+contentType+";base64,"+base64.StdEncoding.EncodeToString(data))
			continue
		}
		data, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			return result, err
		}
		value := string(data)
		if image {
			images = append(images, value)
			continue
		}
		if name == "" {
			return result, Invalid("Multipart fields must have a name")
		}
		if _, exists := body[name]; exists {
			return result, Invalid("Duplicate image request field")
		}
		switch name {
		case "n", "seed", "steps", "num_inference_steps", "guidance", "guidance_scale", "output_compression":
			var number json.Number
			if json.Unmarshal(data, &number) != nil || !json.Valid(data) || strings.HasPrefix(value, "\"") {
				return result, Invalid("Invalid numeric image parameter")
			}
			body[name] = number
		case "stream":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return result, Invalid("Invalid stream flag")
			}
			body[name] = b
		case "loras":
			var v any
			if json.Unmarshal(data, &v) == nil {
				body[name] = v
			} else {
				body[name] = value
			}
		default:
			body[name] = value
		}
	}
	if len(images) > 0 {
		body["image_url"] = images
	}
	result.Parameters = body
	return result, nil
}
