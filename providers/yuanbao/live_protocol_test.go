package yuanbao

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/WebLLmTech/bifrost-web-providers/core"
)

// Explicit opt-in protocol diagnostic. The credential file must be private;
// no credentials or JSON content are printed. Ordinary tests never use it.
func TestLiveProtocol(t *testing.T) {
	path := os.Getenv("YUANBAO_LIVE_FILE")
	if path == "" {
		t.Skip("opt-in live diagnostic")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read private live credential file")
	}
	var cred core.Credential
	if json.Unmarshal(data, &cred) != nil {
		t.Fatal("invalid live credential file")
	}
	rc := core.RequestContext{Credential: cred}
	id, err := parseIdentity(rc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	raw, _ := json.Marshal(Options{BrowserlessWS: "ws://browserless-chromium:3000"})
	v, err := New(ctx, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := v.(*Provider)
	defer p.Close(context.Background())
	security, err := p.security(ctx, id, nil)
	if err != nil {
		t.Fatal("live signing failed")
	}
	chat, err := p.createConversation(ctx, id, security)
	if err != nil {
		t.Fatal("live session creation failed")
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.deleteConversation(clean, id, chat)
	}()
	cfg, err := p.Configure(core.ChatRequest{Model: "hunyuan_omnipotent_hy4"})
	if err != nil {
		t.Fatal(err)
	}
	security, err = p.security(ctx, id, nil)
	if err != nil {
		t.Fatal("live signing failed")
	}
	stream, err := p.completion(ctx, id, chat, "只回复 OK", cfg, security)
	if err != nil {
		t.Fatal("live completion open failed")
	}
	defer stream.Close()
	s := stream.(*completionStream)
	for i := 0; i < 100; i++ {
		frame, err := s.sse.Next(ctx)
		if err != nil {
			fmt.Printf("live protocol end: %T\n", err)
			return
		}
		var object map[string]json.RawMessage
		if json.Unmarshal([]byte(frame.Data), &object) == nil {
			var kind string
			json.Unmarshal(object["type"], &kind)
			fmt.Printf("live JSON frame type=%q\n", kind)
			continue
		}
		message := frame.Data
		for _, secret := range []string{id.Token, id.AgentID, id.User} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		if len(message) > 120 {
			message = message[:120]
		}
		fmt.Printf("live control frame event=%q data=%q\n", frame.Event, message)
	}
}
