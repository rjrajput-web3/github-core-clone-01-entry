// Forge: a standalone GitHub-compatible git forge in one process.
//
// The binary binds an ephemeral loopback port, prints FORGE_PORT=<n> on stdout,
// and serves four surfaces from one in-memory store: git smart HTTP, the REST
// v3 API, GraphQL v4, and MCP over streamable HTTP. It keeps no disk state:
// a trial is one process lifetime, and memory is the fastest store for it.
//
// Auth: every API request must carry Authorization: Bearer <MAKEFUN_SHOP_TOKEN>.
// The token owner's login is the fixed scenario viewer, "adaiversen".
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

var reqCounter uint64

// nextReqID hands out short deterministic request ids for X-GitHub-Request-Id.
func nextReqID() string {
	return "req-" + strconv.FormatUint(atomic.AddUint64(&reqCounter, 1), 10)
}

// jsonOut writes one JSON reply with the headers every REST reply carries.
func jsonOut(w http.ResponseWriter, status int, body any, extra map[string]string) {
	data, err := json.Marshal(body)
	if err != nil {
		data = []byte(`{"message":"internal error"}`)
		status = 500
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-GitHub-Request-Id", nextReqID())
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}

// errJSON is the {"message": ...} body GitHub returns for errors.
func errJSON(w http.ResponseWriter, status int, msg string) {
	jsonOut(w, status, map[string]string{"message": msg}, nil)
}

// readJSONBody decodes a request body into v. An empty body decodes as an
// empty object; a malformed body reports false so the caller can answer 400.
func readJSONBody(r *http.Request, v any) bool {
	if r.Body == nil {
		return true
	}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		// io.EOF means an empty body, which is fine; anything else is malformed.
		if err.Error() == "EOF" {
			return true
		}
		return false
	}
	return true
}

// bodyMap reads the request body as a generic object. It reports ok=false
// when the body is present but not valid JSON.
func bodyMap(r *http.Request) (map[string]any, bool) {
	out := map[string]any{}
	if r.Body == nil {
		return out, true
	}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		if err.Error() == "EOF" {
			return out, true
		}
		return nil, false
	}
	return out, true
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolVal(m map[string]any, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func numVal(m map[string]any, key string) (int64, bool) {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i, true
			}
		case float64:
			return int64(n), true
		}
	}
	return 0, false
}

// server bundles the store with the HTTP handler.
type server struct {
	store *Store
}

func (s *server) authed(r *http.Request) bool {
	want := s.store.token
	got := r.Header.Get("Authorization")
	if want == "" {
		// No token configured (local dev): any non-empty bearer passes.
		return strings.HasPrefix(got, "Bearer ") && len(got) > 7
	}
	return got == "Bearer "+want
}

// ServeHTTP routes every surface the forge speaks.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if r.Method == "GET" && path == "/healthz" {
		jsonOut(w, 200, map[string]bool{"ok": true}, nil)
		return
	}

	// Git smart HTTP: /{owner}/{repo}.git/...
	if strings.HasSuffix(path, ".git/info/refs") || strings.HasSuffix(path, ".git/git-upload-pack") || strings.HasSuffix(path, ".git/git-receive-pack") {
		s.serveGit(w, r)
		return
	}

	if path == "/api/graphql" || strings.HasPrefix(path, "/api/graphql/") {
		if r.Method != "POST" {
			errJSON(w, 404, "Not Found")
			return
		}
		if !s.authed(r) {
			errJSON(w, 401, "Requires authentication")
			return
		}
		s.serveGraphQL(w, r)
		return
	}

	if path == "/mcp" || strings.HasPrefix(path, "/mcp/") {
		s.serveMCP(w, r)
		return
	}

	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		if !s.authed(r) {
			errJSON(w, 401, "Requires authentication")
			return
		}
		s.serveRest(w, r)
		return
	}

	errJSON(w, 404, "Not Found")
}

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	viewer := os.Getenv("FORGE_VIEWER")
	if viewer == "" {
		viewer = "adaiversen"
	}
	store := newStore(token, viewer)
	srv := &server{store: store}

	port := 0
	if p := os.Getenv("FORGE_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	fmt.Printf("FORGE_PORT=%d\n", addr.Port)
	_ = os.Stdout.Sync()

	httpServer := &http.Server{Handler: srv}
	if err := httpServer.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
		os.Exit(1)
	}
}
