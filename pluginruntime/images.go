package pluginruntime

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

func (r *Runtime) images(ctx context.Context, w http.ResponseWriter, request *http.Request, p core.Provider, rc core.RequestContext, editing bool) {
	provider, ok := p.(core.ImageProvider)
	if !ok {
		sendError(w, core.Unsupported("This provider does not support images"), rc.Credential)
		return
	}
	if request.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		sendError(w, core.Upstream(405, "method_not_allowed", "Use POST for images"), rc.Credential)
		return
	}
	request.Body = http.MaxBytesReader(w, request.Body, r.cfg.MaxRequestMB<<20)
	stop := context.AfterFunc(ctx, func() { request.Body.Close() })
	defer stop()
	input, err := core.ReadImageRequest(request, editing)
	if err != nil {
		var large *http.MaxBytesError
		var typed *core.APIError
		switch {
		case errors.As(err, &large):
			err = core.Upstream(413, "request_too_large", "Image request exceeds configured limit")
		case ctx.Err() != nil:
			err = ctx.Err()
		case !errors.As(err, &typed):
			err = core.Invalid("Invalid image request")
		}
		sendError(w, err, rc.Credential)
		return
	}
	result, err := provider.Images(ctx, input, rc)
	for key, values := range result.Headers {
		if strings.EqualFold(key, "X-ModelScope-Task-ID") {
			for _, value := range values {
				if !strings.ContainsAny(value, "\r\n\x00") {
					w.Header().Add(key, value)
				}
			}
		}
	}
	if err != nil {
		sendError(w, err, rc.Credential)
		return
	}
	jsonResponse(w, http.StatusOK, result)
}
