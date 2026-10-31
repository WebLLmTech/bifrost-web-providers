package modelscope

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

type taskResult struct {
	TaskID       string   `json:"task_id"`
	TaskStatus   string   `json:"task_status"`
	OutputImages []string `json:"output_images"`
	Message      string   `json:"message"`
	Errors       struct {
		Message string `json:"message"`
	} `json:"errors"`
	Outputs struct {
		OutputImages []string `json:"output_images"`
	} `json:"outputs"`
}

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,512}$`)

func terminal(status string) bool {
	switch status {
	case "SUCCEED", "SUCCEEDED", "FAILED", "CANCELED", "CANCELLED", "UNKNOWN":
		return true
	}
	return false
}
func (p *Provider) callTask(ctx context.Context, rc core.RequestContext, method, path string, body []byte, out *taskResult) error {
	req, err := p.request(ctx, rc, method, path, body)
	if err != nil {
		return err
	}
	if method == http.MethodPost {
		req.Header.Set("X-ModelScope-Async-Mode", "true")
	} else {
		req.Header.Set("X-ModelScope-Task-Type", "image_generation")
	}
	resp, err := p.http.Do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := core.ReadBounded(resp.Body, 2<<20)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("ModelScope returned HTTP %d", resp.StatusCode)
		var details struct {
			Errors struct {
				Message string `json:"message"`
			} `json:"errors"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &details) == nil {
			detail := details.Errors.Message
			if detail == "" {
				detail = details.Error.Message
			}
			if detail = safeDetail(detail, rc.Credential.Value); detail != "" {
				message += ": " + detail
			}
		}
		return core.Upstream(resp.StatusCode, "upstream_http_error", message)
	}
	if json.Unmarshal(data, out) != nil {
		return core.Upstream(502, "invalid_task", "ModelScope returned invalid task JSON")
	}
	if len(out.OutputImages) == 0 {
		out.OutputImages = out.Outputs.OutputImages
	}
	return err
}
func (p *Provider) Images(parent context.Context, in core.ImageRequest, rc core.RequestContext) (result core.ImageResponse, err error) {
	ctx, cancel := p.lifetime(parent)
	defer cancel()
	// Normalize a copy so callers can reuse the original request.
	body := make(map[string]any, len(in.Parameters))
	for k, v := range in.Parameters {
		body[k] = v
	}
	format, err := normalizeImage(body, in.Editing)
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return result, core.Invalid("Invalid image parameters")
	}
	var submission taskResult
	err = p.callTask(ctx, rc, http.MethodPost, "/v1/images/generations", raw, &submission)
	taskID := submission.TaskID
	if taskID != "" && !taskIDPattern.MatchString(taskID) {
		return result, core.Upstream(502, "invalid_task_id", "ModelScope returned an invalid task ID")
	}
	if taskID != "" {
		result.Headers = http.Header{"X-Modelscope-Task-Id": []string{taskID}}
		defer func() {
			if err != nil {
				e := core.PublicError(err, rc.Credential.Value)
				e.Message += " (task_id=" + taskID + "; the submitted task may still exist upstream)"
				err = e
			}
		}()
	}
	if err != nil {
		return result, err
	}
	current := submission
	if taskID != "" {
		current = taskResult{}
	} // Submission SUCCEED does not complete a task.
	if taskID == "" && len(current.OutputImages) == 0 {
		return result, core.Upstream(502, "invalid_task", "ModelScope did not return a task ID")
	}
	for !terminal(current.TaskStatus) && len(current.OutputImages) == 0 {
		if err = wait(ctx, time.Duration(p.opts.PollIntervalMS)*time.Millisecond); err != nil {
			return result, err
		}
		var next taskResult
		err = p.callTask(ctx, rc, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID), nil, &next)
		if err != nil {
			var apiErr *core.APIError
			if errors.As(err, &apiErr) && apiErr.Code == "upstream_http_error" && (apiErr.Status == 429 || apiErr.Status == 502 || apiErr.Status == 503 || apiErr.Status == 504) {
				continue
			}
			return result, err
		}
		if next.TaskStatus == "" && len(next.OutputImages) == 0 {
			return result, core.Upstream(502, "invalid_task", "ModelScope returned no task status")
		}
		current = next
	}
	if terminal(current.TaskStatus) && current.TaskStatus != "SUCCEED" && current.TaskStatus != "SUCCEEDED" {
		message := "ModelScope task " + current.TaskStatus
		detail := current.Message
		if detail == "" {
			detail = current.Errors.Message
		}
		if detail = safeDetail(detail, rc.Credential.Value); detail != "" {
			message += ": " + detail
		}
		return result, core.Upstream(502, "task_failed", message)
	}
	if len(current.OutputImages) == 0 {
		return result, core.Upstream(502, "empty_result", "ModelScope completed without output images")
	}
	result.Created = time.Now().Unix()
	result.Data = make([]core.ImageData, 0, len(current.OutputImages))
	remaining := p.opts.MaxResultMB << 20
	for _, imageURL := range current.OutputImages {
		u, parseErr := url.Parse(imageURL)
		if parseErr != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return result, core.Upstream(502, "invalid_image_url", "ModelScope returned an invalid image URL")
		}
		if format == "url" {
			result.Data = append(result.Data, core.ImageData{URL: imageURL})
			continue
		}
		// Shared resolver enforces public addresses, redirect checks and size limits.
		// It never receives the provider's Authorization header or cookies.
		if remaining <= 0 {
			return result, core.Upstream(502, "image_too_large", "ModelScope output exceeds the result limit")
		}
		resolver := *p.results
		resolver.MaxBytes = remaining
		file, downloadErr := resolver.Resolve(ctx, core.FileRef{Source: imageURL})
		if downloadErr != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, core.Upstream(502, "image_download_failed", "Could not download the ModelScope output image")
		}
		if !strings.HasPrefix(http.DetectContentType(file.Bytes), "image/") {
			return result, core.Upstream(502, "invalid_image", "ModelScope output is not an image")
		}
		remaining -= int64(len(file.Bytes))
		result.Data = append(result.Data, core.ImageData{B64JSON: base64.StdEncoding.EncodeToString(file.Bytes)})
	}
	return result, nil
}
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func safeDetail(message, token string) string {
	if token != "" {
		message = strings.ReplaceAll(message, token, "[redacted]")
	}
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)
	runes := []rune(message)
	if len(runes) > 512 {
		message = string(runes[:512])
	}
	return strings.TrimSpace(message)
}
