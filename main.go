package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	apiToken = os.Getenv("MAKEFUN_SHOP_TOKEN")
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")

	mux := http.NewServeMux()
	mux.HandleFunc("/", dispatch)

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[forge] listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[forge] listening on port %d\n", port)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "[forge] serve:", err)
			os.Exit(1)
		}
	}()

	if envURL == "" {
		// No env to register with (local smoke run): serve until killed.
		fmt.Println("[forge] MAKEFUN_ENV_URL is not set; serving without registering")
		select {}
	}
	register(envURL, port)
	fmt.Println("[forge] registered; waiting for done")
	waitForDone(envURL)
	_ = srv.Close()
	fmt.Println("[forge] done")
}

func postJSON(url string, body any) (int, []byte, error) {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(url, "application/json", &buf)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

func register(envURL string, port int) {
	delay := 250 * time.Millisecond
	for {
		status, _, err := postJSON(envURL+"/register", map[string]any{"port": port})
		if err == nil && (status == 200 || status == 409) {
			return
		}
		if err == nil && (status == 400 || status == 405) {
			fmt.Fprintln(os.Stderr, "[forge] register refused:", status)
			os.Exit(1)
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func waitForDone(envURL string) {
	delay := 250 * time.Millisecond
	client := &http.Client{Timeout: 30 * time.Second}
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var v struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(data, &v) == nil && v.Done {
				return
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func dispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/healthz" {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if strings.HasPrefix(path, "/api/v3") {
		rest := strings.TrimPrefix(path, "/api/v3")
		rest = strings.TrimPrefix(rest, "/")
		var segs []string
		if rest != "" {
			segs = strings.Split(rest, "/")
		}
		handleRest(w, r, segs)
		return
	}
	if path == "/api/graphql" {
		handleGraphQL(w, r)
		return
	}
	if path == "/mcp" {
		handleMCP(w, r)
		return
	}
	// Git smart HTTP: /{owner}/{repo}.git/...
	if strings.Contains(path, ".git") {
		handleGit(w, r)
		return
	}
	errJSON(w, 404, "Not Found")
}

func handleGit(w http.ResponseWriter, r *http.Request) {
	// /{owner}/{repo}.git/info/refs?service=... or /{owner}/{repo}.git/git-upload-pack
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || !strings.HasSuffix(parts[1], ".git") {
		errJSON(w, 404, "Not Found")
		return
	}
	owner := parts[0]
	name := strings.TrimSuffix(parts[1], ".git")
	if len(parts) < 3 {
		errJSON(w, 404, "Not Found")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	switch parts[2] {
	case "info":
		if len(parts) == 4 && parts[3] == "refs" && r.Method == http.MethodGet {
			service := r.URL.Query().Get("service")
			if service != "git-upload-pack" && service != "git-receive-pack" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			gitInfoRefs(w, r, owner, name, service)
			return
		}
	case "git-upload-pack":
		if r.Method == http.MethodPost {
			gitUploadPack(w, r, owner, name, body)
			return
		}
	case "git-receive-pack":
		if r.Method == http.MethodPost {
			gitReceivePack(w, r, owner, name, body)
			return
		}
	}
	errJSON(w, 404, "Not Found")
}
