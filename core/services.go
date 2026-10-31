package core

// Services are injected into provider factories. Providers may use their own
// specialized transports (e.g. CDP), but never need to duplicate HTTP/SSE/files.
type Services struct {
	HTTP  *HTTPClient
	Files *FileResolver
}
