// GitForge entrypoint: the lifecycle the MakeFun env drives.
//   1. MAKEFUN_ENV_URL names the env's origin. Without it, exit 1.
//   2. Bind an ephemeral port on 127.0.0.1 (never 8080 — that belongs to the env).
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. First register wins; a 409
//      means another entry already registered — keep waiting like any other.
//   4. Poll GET $MAKEFUN_ENV_URL/done until {"done": true}, with short
//      exponential backoff; never exit before done.
//   5. Close the server and exit 0.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[entry]", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[entry] listening on port %d\n", port)
	srv := &http.Server{Handler: http.HandlerFunc(handler)}
	go srv.Serve(ln)

	client := &http.Client{Timeout: 30 * time.Second}
	retryDelay := func(attempt int) time.Duration {
		d := 250 * time.Millisecond
		for i := 0; i < attempt && i < 8; i++ {
			d *= 2
		}
		if d > 2*time.Second {
			d = 2 * time.Second
		}
		return d
	}
	// register
	for attempt := 0; ; attempt++ {
		reqBody := bytes.NewBufferString(`{"port":` + strconv.Itoa(port) + `}`)
		resp, err := client.Post(envURL+"/register", "application/json", reqBody)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				break
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				fmt.Fprintf(os.Stderr, "[entry] register answered HTTP %d\n", resp.StatusCode)
				os.Exit(1)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
	fmt.Println("[entry] registered; waiting for the env to say done")
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Done {
				resp.Body.Close()
				break
			}
			resp.Body.Close()
		}
		time.Sleep(retryDelay(attempt))
	}
	srv.Close()
	fmt.Println("[entry] done")
}
