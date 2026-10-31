// Package pluginruntime hosts multiple web providers inside one Bifrost plugin.
package pluginruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/WebLLmTech/bifrost-web-providers/core"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Factory func(context.Context, json.RawMessage, *core.Services) (core.Provider, error)
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

func NewRegistry() *Registry { return &Registry{factories: map[string]Factory{}} }
func (r *Registry) Register(kind string, f Factory) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if kind == "" || f == nil {
		return errors.New("provider kind and factory required")
	}
	if _, ok := r.factories[kind]; ok {
		return fmt.Errorf("provider factory already registered: %s", kind)
	}
	r.factories[kind] = f
	return nil
}
func (r *Registry) get(kind string) Factory {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.factories[kind]
}

type ProviderConfig struct {
	Type    string          `json:"type"`
	Enabled *bool           `json:"enabled,omitempty"`
	Options json.RawMessage `json:"options,omitempty"`
}
type Config struct {
	ListenAddr              string                    `json:"listen_addr"`
	Providers               map[string]ProviderConfig `json:"providers"`
	RequestTimeoutSeconds   int                       `json:"request_timeout_seconds"`
	MaxRequestMB            int64                     `json:"max_request_mb"`
	MaxCompletionMB         int                       `json:"max_completion_mb"`
	AttachmentMaxMB         int64                     `json:"attachment_max_mb"`
	AllowPrivateAttachments bool                      `json:"allow_private_attachments"`
	HeartbeatSeconds        int                       `json:"heartbeat_seconds"`
}

func DefaultConfig() Config {
	return Config{ListenAddr: "127.0.0.1:8792", Providers: map[string]ProviderConfig{}, RequestTimeoutSeconds: 300, MaxRequestMB: 32, MaxCompletionMB: 16, AttachmentMaxMB: 32, HeartbeatSeconds: 15}
}
func ParseConfig(input any) (Config, error) {
	c := DefaultConfig()
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return c, err
		}
		decoder := json.NewDecoder(strings.NewReader(string(b)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c); err != nil {
			return c, err
		}
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("invalid listen_addr: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("plugin listener must bind an explicit loopback IP")
	}
	if c.RequestTimeoutSeconds < 1 || c.MaxRequestMB < 1 || c.MaxCompletionMB < 1 || c.AttachmentMaxMB < 1 || c.HeartbeatSeconds < 1 {
		return errors.New("timeouts and size limits must be positive")
	}
	valid := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	for name, p := range c.Providers {
		if !valid.MatchString(name) || p.Type == "" {
			return errors.New("provider names and types must be specified; names cannot contain paths")
		}
	}
	return nil
}

type Runtime struct {
	cfg       Config
	providers map[string]core.Provider
	services  *core.Services
	server    *http.Server
	cancel    context.CancelFunc
	mu        sync.Mutex
	closed    bool
	started   bool
}

func New(ctx context.Context, cfg Config, registry *Registry) (*Runtime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	base, cancel := context.WithCancel(ctx)
	r := &Runtime{cfg: cfg, providers: map[string]core.Provider{}, cancel: cancel, services: &core.Services{HTTP: core.NewHTTPClient(), Files: core.NewFileResolver(cfg.AttachmentMaxMB<<20, cfg.AllowPrivateAttachments)}}
	for name, pc := range cfg.Providers {
		if pc.Enabled != nil && !*pc.Enabled {
			continue
		}
		f := registry.get(pc.Type)
		if f == nil {
			_ = r.Close(context.Background())
			return nil, fmt.Errorf("provider implementation not registered: %s", pc.Type)
		}
		p, err := f(base, pc.Options, r.services)
		if err != nil {
			if p != nil {
				_ = p.Close(base)
			}
			_ = r.Close(context.Background())
			return nil, fmt.Errorf("provider %s initialization failed", name)
		}
		if p == nil {
			_ = r.Close(context.Background())
			return nil, fmt.Errorf("provider %s returned nil", name)
		}
		r.providers[name] = p
	}
	r.server = &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return base }}
	return r, nil
}
func (r *Runtime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	if r.started {
		return errors.New("runtime is already started")
	}
	ln, err := net.Listen("tcp", r.cfg.ListenAddr)
	if err != nil {
		return err
	}
	r.cfg.ListenAddr = ln.Addr().String()
	r.started = true
	go func() { _ = r.server.Serve(ln) }()
	return nil
}
func (r *Runtime) Addr() string { r.mu.Lock(); defer r.mu.Unlock(); return r.cfg.ListenAddr }
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if r.server != nil {
		if err := r.server.Shutdown(ctx); err != nil {
			_ = r.server.Close()
		}
	}
	for _, p := range r.providers {
		_ = p.Close(ctx)
	}
	r.services.HTTP.Close()
	r.services.Files.Close()
	return nil
}
