// MCP over Streamable HTTP: JSON-RPC 2.0 at POST /mcp. Modern era
// (2026-07-28) answers server/discover; the legacy era answers initialize.
// The same six tools the suite exercises are offered, and every tool call
// reads and writes the same store the REST API uses.
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

var mcpVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var mcpLegacyVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// mcpTool is one entry of the tools/list answer.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func strProp() map[string]any { return map[string]any{"type": "string"} }
func boolProp() map[string]any { return map[string]any{"type": "boolean"} }
func intProp() map[string]any  { return map[string]any{"type": "integer"} }

func schema(props map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

func mcpTools() []mcpTool {
	return []mcpTool{
		{
			Name:        "create_repo",
			Description: "Create a repository owned by the authenticated user.",
			InputSchema: schema(map[string]any{
				"name":        strProp(),
				"description": strProp(),
				"private":     boolProp(),
			}, []string{"name"}),
		},
		{
			Name:        "get_repo",
			Description: "Get a repository by owner and name.",
			InputSchema: schema(map[string]any{
				"owner": strProp(),
				"repo":  strProp(),
			}, []string{"owner", "repo"}),
		},
		{
			Name:        "create_issue",
			Description: "Create an issue in a repository.",
			InputSchema: schema(map[string]any{
				"owner": strProp(),
				"repo":  strProp(),
				"title": strProp(),
				"body":  strProp(),
			}, []string{"owner", "repo", "title"}),
		},
		{
			Name:        "list_issues",
			Description: "List issues in a repository.",
			InputSchema: schema(map[string]any{
				"owner": strProp(),
				"repo":  strProp(),
				"state": strProp(),
			}, []string{"owner", "repo"}),
		},
		{
			Name:        "create_pull_request",
			Description: "Create a pull request from a head branch onto a base branch.",
			InputSchema: schema(map[string]any{
				"owner": strProp(),
				"repo":  strProp(),
				"title": strProp(),
				"head":  strProp(),
				"base":  strProp(),
			}, []string{"owner", "repo", "title", "head", "base"}),
		},
		{
			Name:        "merge_pull_request",
			Description: "Squash-merge a pull request by number.",
			InputSchema: schema(map[string]any{
				"owner":       strProp(),
				"repo":        strProp(),
				"pull_number": intProp(),
			}, []string{"owner", "repo", "pull_number"}),
		},
	}
}

func mcpServerInfo() map[string]any {
	return map[string]any{"name": "forge", "version": "1.0.0"}
}

func (s *server) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		errJSON(w, 404, "Not Found")
		return
	}
	// Auth is decided before any parsing: a call without a token is 401.
	if !s.authed(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Requires authentication"}}` + "\n"))
		return
	}
	var raw json.RawMessage
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}` + "\n"))
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}` + "\n"))
		return
	}
	// A notification carries no id: answer 202 with an empty body.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(202)
		return
	}
	var id any
	if err := json.Unmarshal(req.ID, &id); err != nil {
		id = nil
	}
	// Normalise json.Number ids to int64/float64 for a faithful echo.
	if n, ok := id.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			id = i
		} else if f, err := n.Float64(); err == nil {
			id = f
		}
	}
	respond := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  result,
		})
	}
	respondErr := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": code, "message": msg},
		})
	}

	switch req.Method {
	case "server/discover":
		respond(map[string]any{
			"supportedVersions": mcpVersions,
			"protocolVersion":   "2026-07-28",
			"serverInfo":        mcpServerInfo(),
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta": map[string]any{
				"serverInfo":      mcpServerInfo(),
				"protocolVersion": "2026-07-28",
				"capabilities":    map[string]any{"tools": map[string]any{}},
			},
		})
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if !mcpLegacyVersions[version] {
			version = "2025-11-25"
		}
		respond(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      mcpServerInfo(),
		})
	case "ping":
		respond(map[string]any{})
	case "tools/list":
		respond(map[string]any{"tools": mcpTools()})
	case "tools/call":
		s.mcpCall(w, id, req.Params, respond, respondErr)
	default:
		respondErr(-32601, "Method not found: "+req.Method)
	}
}

// mcpCall runs one tools/call and writes its JSON-RPC answer.
func (s *server) mcpCall(w http.ResponseWriter, id any, paramsRaw json.RawMessage, respond func(any), respondErr func(int, string)) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		respondErr(-32602, "Invalid params")
		return
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	// Required-argument check per tool.
	required := map[string][]string{
		"create_repo":        {"name"},
		"get_repo":           {"owner", "repo"},
		"create_issue":       {"owner", "repo", "title"},
		"list_issues":        {"owner", "repo"},
		"create_pull_request": {"owner", "repo", "title", "head", "base"},
		"merge_pull_request": {"owner", "repo", "pull_number"},
	}
	names, known := required[params.Name]
	if !known {
		respondErr(-32601, "Unknown tool: "+params.Name)
		return
	}
	for _, key := range names {
		if _, ok := params.Arguments[key]; !ok {
			respond(map[string]any{
				"content": []any{map[string]any{
					"type": "text",
					"text": "missing required argument: " + key,
				}},
				"isError": true,
			})
			return
		}
	}
	argStr := func(key string) string {
		if v, ok := params.Arguments[key].(string); ok {
			return v
		}
		return ""
	}
	argBool := func(key string) bool {
		if v, ok := params.Arguments[key].(bool); ok {
			return v
		}
		return false
	}
	argInt := func(key string) int {
		switch v := params.Arguments[key].(type) {
		case float64:
			return int(v)
		case json.Number:
			if i, err := v.Int64(); err == nil {
				return int(i)
			}
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return 0
	}
	// The full_name argument, when present, selects the repository.
	repoName := argStr("repo")
	if full := argStr("full_name"); full != "" {
		if parts := strings.SplitN(full, "/", 2); len(parts) == 2 {
			repoName = parts[1]
		}
	}
	result := func(obj any) {
		text, _ := json.Marshal(obj)
		respond(map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": string(text),
			}},
			"structuredContent": obj,
			"isError":           false,
		})
	}
	fail := func(msg string) {
		respond(map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": msg,
			}},
			"isError": true,
		})
	}

	switch params.Name {
	case "create_repo":
		name := argStr("name")
		s.store.mu.Lock()
		rep, created := s.store.createRepo(name, argBool("private"), argStr("description"))
		if !created {
			s.store.mu.Unlock()
			fail("Repository " + name + " already exists")
			return
		}
		out := repoJSON(s.store, rep)
		s.store.mu.Unlock()
		result(out)
	case "get_repo":
		s.store.mu.RLock()
		rep := s.store.lookup(argStr("owner"), repoName)
		if rep == nil {
			s.store.mu.RUnlock()
			fail("Repository not found")
			return
		}
		out := repoJSON(s.store, rep)
		s.store.mu.RUnlock()
		result(out)
	case "create_issue":
		s.store.mu.Lock()
		rep := s.store.lookup(argStr("owner"), repoName)
		if rep == nil {
			s.store.mu.Unlock()
			fail("Repository not found")
			return
		}
		is := &Issue{
			ID:     s.store.nextID,
			Number: rep.NextIssue,
			Title:  argStr("title"),
			Body:   argStr("body"),
			State:  "open",
			Seq:    s.store.tick(),
		}
		s.store.nextID++
		rep.NextIssue++
		rep.Issues = append(rep.Issues, is)
		out := issueJSON(s.store, rep, is)
		s.store.mu.Unlock()
		result(out)
	case "list_issues":
		state := argStr("state")
		if state == "" {
			state = "open"
		}
		s.store.mu.RLock()
		rep := s.store.lookup(argStr("owner"), repoName)
		if rep == nil {
			s.store.mu.RUnlock()
			fail("Repository not found")
			return
		}
		items := make([]any, 0)
		for _, is := range rep.Issues {
			if state == "all" || is.State == state {
				items = append(items, issueJSON(s.store, rep, is))
			}
		}
		s.store.mu.RUnlock()
		result(map[string]any{"issues": items})
	case "create_pull_request":
		head := argStr("head")
		base := argStr("base")
		s.store.mu.Lock()
		rep := s.store.lookup(argStr("owner"), repoName)
		if rep == nil {
			s.store.mu.Unlock()
			fail("Repository not found")
			return
		}
		if existing := rep.openPullFor(head, base); existing != nil {
			out := pullJSON(s, rep, existing)
			s.store.mu.Unlock()
			result(out)
			return
		}
		headSHA := rep.tipOf(head)
		baseSHA := rep.tipOf(base)
		if headSHA == "" || baseSHA == "" {
			s.store.mu.Unlock()
			fail("head or base branch does not exist")
			return
		}
		p := &Pull{
			ID:      s.store.nextID,
			Number:  rep.NextPull,
			Title:   argStr("title"),
			Body:    "",
			State:   "open",
			Head:    head,
			Base:    base,
			HeadSHA: headSHA,
			BaseSHA: baseSHA,
			Seq:     s.store.tick(),
		}
		s.store.nextID++
		rep.NextPull++
		rep.Pulls = append(rep.Pulls, p)
		out := pullJSON(s, rep, p)
		s.store.mu.Unlock()
		result(out)
	case "merge_pull_request":
		num := argInt("pull_number")
		s.store.mu.Lock()
		rep := s.store.lookup(argStr("owner"), repoName)
		if rep == nil {
			s.store.mu.Unlock()
			fail("Repository not found")
			return
		}
		p := rep.findPull(num)
		if p == nil {
			s.store.mu.Unlock()
			fail("Pull request not found")
			return
		}
		if _, merged := s.squashMerge(rep, p); !merged {
			s.store.mu.Unlock()
			fail("merge conflict")
			return
		}
		out := pullJSON(s, rep, p)
		s.store.mu.Unlock()
		result(out)
	}
}
