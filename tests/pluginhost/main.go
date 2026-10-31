// A real dynamically linked native Go plugin loader, exercising the same three
// lifecycle symbols used by Bifrost 2.2.6. It is not a mock of plugin.Open.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"plugin"
	"strings"
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	for i, path := range os.Args[1:] {
		p, err := plugin.Open(path)
		check(err)
		symbol, err := p.Lookup("Init")
		check(err)
		init := symbol.(func(any) error)
		symbol, err = p.Lookup("Cleanup")
		check(err)
		cleanup := symbol.(func() error)
		symbol, err = p.Lookup("GetName")
		check(err)
		name := symbol.(func() string)()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		check(err)
		addr := ln.Addr().String()
		ln.Close()
		config := map[string]any{"listen_addr": addr, "providers": map[string]any{}}
		if i == 1 {
			config["providers"] = map[string]any{"fixture": map[string]any{"type": "fixture"}}
		}
		check(init(config))
		res, err := http.Get("http://" + addr + "/health")
		check(err)
		if res.StatusCode != 200 {
			panic("health")
		}
		res.Body.Close()
		if i == 1 {
			for _, endpoint := range []string{"chat/completions", "responses"} {
				body := `{"model":"fixture","messages":[{"role":"user","content":"hello"}],"stream":true}`
				if endpoint == "responses" {
					body = `{"model":"fixture","input":"hello","stream":true}`
				}
				req, err := http.NewRequest("POST", "http://"+addr+"/providers/fixture/v1/"+endpoint, strings.NewReader(body))
				check(err)
				req.Header.Set("Authorization", "Bearer test-only")
				response, err := http.DefaultClient.Do(req)
				check(err)
				b, err := io.ReadAll(response.Body)
				response.Body.Close()
				check(err)
				if response.StatusCode != 200 || !strings.Contains(string(b), "hello plugin") {
					panic(string(b))
				}
			}
			req, err := http.NewRequest("POST", "http://"+addr+"/providers/fixture/v1/chat/completions", strings.NewReader(`{"model":"fixture","messages":[{"role":"user","content":"hello"}]}`))
			check(err)
			req.Header.Set("Authorization", "Bearer test-only")
			response, err := http.DefaultClient.Do(req)
			check(err)
			var data map[string]any
			check(json.NewDecoder(response.Body).Decode(&data))
			response.Body.Close()
			if data["object"] != "chat.completion" {
				panic("completion format")
			}
		}
		check(cleanup())
		check(cleanup())
		if response, err := http.Get("http://" + addr + "/health"); err == nil {
			response.Body.Close()
			panic("listener remained after Cleanup")
		}
		check(init(config))
		check(cleanup())
		fmt.Println("PASS native lifecycle and HTTP contract:", name)
	}
}
