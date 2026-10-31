package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type FileRef struct {
	Source   string
	Filename string
}
type FileInput struct {
	Bytes       []byte
	Filename    string
	ContentType string
	Image       bool
}

func ExtractFiles(messages []Message, lastOnly bool) ([]FileRef, error) {
	if lastOnly && len(messages) > 0 {
		messages = messages[len(messages)-1:]
	}
	var out []FileRef
	for _, m := range messages {
		var parts []ContentPart
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "image_url" && p.ImageURL != nil {
				out = append(out, FileRef{Source: p.ImageURL.URL})
			}
			if p.Type == "file" {
				if p.FileURL != nil {
					out = append(out, FileRef{Source: p.FileURL.URL})
				}
				if p.File != nil {
					if p.File.FileID != "" {
						return nil, Unsupported("file_id needs a provider file-store resolver")
					}
					if p.File.FileData != "" {
						out = append(out, FileRef{Source: p.File.FileData, Filename: p.File.Filename})
					}
				}
			}
		}
	}
	return out, nil
}

type FileResolver struct {
	Client   *http.Client
	MaxBytes int64
}

// Remote attachments are public HTTP(S) by default; the dial check applies
// after DNS resolution and on every redirect, preventing DNS-rebinding bypass.
func NewFileResolver(max int64, allowPrivate bool) *FileResolver {
	if max < 1 {
		max = 32 << 20
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !allowPrivate && (ip.IP.IsPrivate() || ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() || !ip.IP.IsGlobalUnicast()) {
				return nil, Invalid("Private attachment address is not allowed")
			}
		}
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, fmt.Errorf("attachment connection failed")
	}
	return &FileResolver{Client: &http.Client{Transport: t, Timeout: 60 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return Invalid("Too many attachment redirects")
		}
		return validateFileURL(r.URL)
	}}, MaxBytes: max}
}
func validateFileURL(u *url.URL) error {
	if u.User != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return Invalid("Attachment URL must use HTTP(S) without credentials")
	}
	return nil
}
func (r *FileResolver) Resolve(ctx context.Context, ref FileRef) (FileInput, error) {
	var b []byte
	var contentType string
	var err error
	if strings.HasPrefix(ref.Source, "data:") {
		header, data, ok := strings.Cut(ref.Source, ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return FileInput{}, Invalid("Attachment data URL must be base64 encoded")
		}
		if len(data) > base64.StdEncoding.EncodedLen(int(r.MaxBytes))+4 {
			return FileInput{}, Invalid("Attachment exceeds configured size limit")
		}
		contentType = strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
		b, err = base64.StdEncoding.DecodeString(data)
		if err != nil {
			return FileInput{}, Invalid("Invalid attachment base64")
		}
	} else {
		u, err := url.Parse(ref.Source)
		if err != nil {
			return FileInput{}, Invalid("Invalid attachment URL")
		}
		if err = validateFileURL(u); err != nil {
			return FileInput{}, err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return FileInput{}, err
		}
		response, err := r.Client.Do(req)
		if err != nil {
			return FileInput{}, err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return FileInput{}, ErrorFromStatus("Attachment host", response.StatusCode)
		}
		b, err = ReadBounded(response.Body, r.MaxBytes)
		if err != nil {
			return FileInput{}, err
		}
		contentType = response.Header.Get("Content-Type")
		if ref.Filename == "" {
			ref.Filename = path.Base(u.Path)
		}
	}
	if int64(len(b)) > r.MaxBytes {
		return FileInput{}, Invalid("Attachment exceeds configured size limit")
	}
	if contentType == "" {
		contentType = http.DetectContentType(b)
	}
	ct, _, _ := mime.ParseMediaType(contentType)
	if ct != "" {
		contentType = ct
	}
	name := path.Base(ref.Filename)
	if name == "." || name == "/" || name == "" {
		exts, _ := mime.ExtensionsByType(contentType)
		name = "attachment"
		if len(exts) > 0 {
			name += exts[0]
		}
	}
	return FileInput{Bytes: b, Filename: name, ContentType: contentType, Image: strings.HasPrefix(contentType, "image/")}, nil
}
func (r *FileResolver) Close() { r.Client.CloseIdleConnections() }
