package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// MCP over Streamable HTTP: JSON-RPC 2.0. Supports the 2026-07-28 revision
// (server/discover, no handshake) and the legacy handshake era (initialize +
// notifications/initialized) through the same endpoint.

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func strSchema(props map[string]any, required []string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func mcpTools() []mcpTool {
	s := func(props map[string]any, req []string) map[string]any { return strSchema(props, req) }
	str := map[string]any{"type": "string"}
	return []mcpTool{
		{"create_repo", "Create a repository owned by the viewer.", s(map[string]any{"name": str, "description": str, "private": map[string]any{"type": "boolean"}, "owner": str, "full_name": str}, []string{"name"})},
		{"get_repo", "Get a repository by owner and name.", s(map[string]any{"owner": str, "repo": str, "name": str, "full_name": str}, []string{"owner", "repo"})},
		{"create_issue", "Create an issue in a repository.", s(map[string]any{"owner": str, "repo": str, "name": str, "full_name": str, "title": str, "body": str}, []string{"owner", "repo", "title"})},
		{"list_issues", "List issues in a repository, optionally filtered by state.", s(map[string]any{"owner": str, "repo": str, "name": str, "full_name": str, "state": str}, []string{"owner", "repo"})},
		{"create_pull_request", "Open a pull request from a head branch onto a base branch.", s(map[string]any{"owner": str, "repo": str, "name": str, "full_name": str, "title": str, "body": str, "head": str, "base": str}, []string{"owner", "repo", "title", "head", "base"})},
		{"merge_pull_request", "Merge a pull request by number.", s(map[string]any{"owner": str, "repo": str, "name": str, "full_name": str, "pull_number": map[string]any{"type": "integer"}, "number": map[string]any{"type": "integer"}}, []string{"owner", "repo", "pull_number"})},
		{"list_pull_requests", "List pull requests in a repository.", s(map[string]any{"owner": str, "repo": str, "name": str, "full_name": str, "state": str}, []string{"owner", "repo"})},
		{"get_issue", "Get one issue by number.", s(map[string]any{"owner": str, "repo": str, "name": str, "full_name": str, "number": map[string]any{"type": "integer"}}, []string{"owner", "repo", "number"})},
	}
}

func mcpResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpError(id any, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

// argOf reads an argument by any of its documented spellings.
func argOf(args map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := args[k]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

func argStr(args map[string]any, keys ...string) string {
	if v, ok := argOf(args, keys...); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argInt(args map[string]any, keys ...string) (int, bool) {
	if v, ok := argOf(args, keys...); ok {
		switch n := v.(type) {
		case float64:
			return int(n), true
		case string:
			if i, err := strconv.Atoi(n); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}

// repoFromArgs resolves owner/repo from the argument spellings the suite
// uses, including a full_name of "owner/repo".
func repoFromArgs(args map[string]any) (owner, name string, ok bool) {
	owner = argStr(args, "owner")
	name = argStr(args, "repo", "name", "repository", "repository_name", "repo_name")
	if full := argStr(args, "full_name", "repo_full_name", "repository_full_name", "fullName"); full != "" {
		if parts := strings.SplitN(full, "/", 2); len(parts) == 2 {
			if owner == "" {
				owner = parts[0]
			}
			if name == "" {
				name = parts[1]
			}
		}
	}
	if owner == "" {
		owner = viewerLogin
	}
	return owner, name, name != ""
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"message": "Method not allowed"})
		return
	}
	if !checkAuth(r) {
		writeJSON(w, 401, map[string]any{"message": "Requires authentication"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, 400, mcpError(nil, -32700, "Parse error"))
		return
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		writeJSON(w, 400, mcpError(nil, -32700, "Parse error"))
		return
	}
	method, _ := body["method"].(string)
	id := body["id"]
	params, _ := body["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	// Notifications carry no id and expect no result body.
	if id == nil {
		prepRest(w)
		w.WriteHeader(202)
		return
	}

	switch method {
	case "server/discover":
		w.Header().Set("Mcp-Session-Id", "forge-1")
		writeJSON(w, 200, mcpResult(id, map[string]any{
			"protocolVersion": "2026-07-28",
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"},
			"serverInfo": map[string]any{"name": "forge", "version": "1.0.0"},
			"capabilities": map[string]any{"tools": map[string]any{}},
			"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28"},
		}))
	case "initialize":
		version := "2025-11-25"
		if v, ok := params["protocolVersion"].(string); ok && v != "" {
			version = v
		}
		w.Header().Set("Mcp-Session-Id", "forge-1")
		writeJSON(w, 200, mcpResult(id, map[string]any{
			"protocolVersion": version,
			"capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "forge", "version": "1.0.0"},
		}))
	case "tools/list":
		writeJSON(w, 200, mcpResult(id, map[string]any{"tools": mcpTools()}))
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result, code, msg := callTool(name, args)
		if code != 0 {
			writeJSON(w, 200, mcpError(id, code, msg))
			return
		}
		text, _ := json.Marshal(result)
		writeJSON(w, 200, mcpResult(id, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": string(text)}},
			"structuredContent": result,
			"isError": false,
		}))
	case "ping":
		writeJSON(w, 200, mcpResult(id, map[string]any{}))
	default:
		writeJSON(w, 200, mcpError(id, -32601, "Method not found: "+method))
	}
}

// callTool dispatches one tool call. Returns (result, 0, "") on success or
// (nil, jsonrpcCode, message) on failure.
func callTool(name string, args map[string]any) (any, int, string) {
	// Match by suffix, case-insensitively: "github__create_issue" works.
	base := strings.ToLower(name)
	if i := strings.LastIndex(base, "__"); i >= 0 {
		base = base[i+2:]
	}
	missing := func(field string) (any, int, string) {
		return nil, -32602, "missing required argument: " + field
	}
	switch base {
	case "create_repo":
		repoName := argStr(args, "name", "repo", "repository", "repository_name")
		if repoName == "" {
			return missing("name")
		}
		store.mu.Lock()
		repo, ok := store.createRepo(repoName, argStr(args, "private") == "true", argStr(args, "description"))
		if !ok {
			repo = store.repos[repoKey(viewerLogin, repoName)]
		}
		v := repoView(repo)
		store.mu.Unlock()
		return v, 0, ""
	case "get_repo":
		owner, repoName, ok := repoFromArgs(args)
		if !ok {
			return missing("repo")
		}
		store.mu.RLock()
		repo := store.lookup(owner, repoName)
		var v any
		if repo != nil {
			v = repoView(repo)
		}
		store.mu.RUnlock()
		if v == nil {
			return nil, -32602, "repository not found"
		}
		return v, 0, ""
	case "create_issue":
		owner, repoName, ok := repoFromArgs(args)
		if !ok {
			return missing("repo")
		}
		title := argStr(args, "title", "issue_title", "subject")
		if title == "" {
			return missing("title")
		}
		store.mu.Lock()
		repo := store.lookup(owner, repoName)
		if repo == nil {
			store.mu.Unlock()
			return nil, -32602, "repository not found"
		}
		it := store.newIssue(repo, title, argStr(args, "body"), nil)
		v := issueView(repo, it)
		store.mu.Unlock()
		return v, 0, ""
	case "list_issues":
		owner, repoName, ok := repoFromArgs(args)
		if !ok {
			return missing("repo")
		}
		state := strings.ToLower(argStr(args, "state"))
		if state == "" {
			state = "open"
		}
		store.mu.RLock()
		repo := store.lookup(owner, repoName)
		out := []any{}
		if repo != nil {
			for _, it := range repo.sortedIssues(false) {
				if state == "all" || it.State == state {
					out = append(out, issueView(repo, it))
				}
			}
		}
		store.mu.RUnlock()
		if repo == nil {
			return nil, -32602, "repository not found"
		}
		return map[string]any{"issues": out}, 0, ""
	case "get_issue":
		owner, repoName, ok := repoFromArgs(args)
		if !ok {
			return missing("repo")
		}
		n, hasNum := argInt(args, "number", "issue_number")
		if !hasNum {
			return missing("number")
		}
		store.mu.RLock()
		repo := store.lookup(owner, repoName)
		var v any
		if repo != nil {
			if it := repo.findIssue(n); it != nil {
				v = issueView(repo, it)
			}
		}
		store.mu.RUnlock()
		if v == nil {
			return nil, -32602, "issue not found"
		}
		return v, 0, ""
	case "create_pull_request":
		owner, repoName, ok := repoFromArgs(args)
		if !ok {
			return missing("repo")
		}
		title := argStr(args, "title", "pr_title", "pull_request_title", "subject")
		if title == "" {
			return missing("title")
		}
		head := argStr(args, "head")
		baseRef := argStr(args, "base")
		if head == "" {
			return missing("head")
		}
		if baseRef == "" {
			return missing("base")
		}
		store.mu.Lock()
		repo := store.lookup(owner, repoName)
		if repo == nil {
			store.mu.Unlock()
			return nil, -32602, "repository not found"
		}
		headSha := repo.tip(head)
		baseSha := repo.tip(baseRef)
		if headSha == "" || baseSha == "" {
			store.mu.Unlock()
			return nil, -32602, "head or base branch not found"
		}
		p := store.newPull(repo, title, argStr(args, "body"), head, baseRef, headSha, baseSha)
		v := pullView(repo, p)
		store.mu.Unlock()
		return v, 0, ""
	case "merge_pull_request":
		owner, repoName, ok := repoFromArgs(args)
		if !ok {
			return missing("repo")
		}
		n, hasNum := argInt(args, "pull_number", "pullNumber", "number", "pr_number")
		if !hasNum {
			return missing("pull_number")
		}
		store.mu.Lock()
		repo := store.lookup(owner, repoName)
		if repo == nil {
			store.mu.Unlock()
			return nil, -32602, "repository not found"
		}
		p := repo.findPull(n)
		if p == nil {
			store.mu.Unlock()
			return nil, -32602, "pull request not found"
		}
		sha, merged := squashMerge(repo, p)
		if !merged {
			store.mu.Unlock()
			return nil, -32602, "merge conflict"
		}
		v := map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged", "number": p.Number}
		store.mu.Unlock()
		return v, 0, ""
	case "list_pull_requests":
		owner, repoName, ok := repoFromArgs(args)
		if !ok {
			return missing("repo")
		}
		state := strings.ToLower(argStr(args, "state"))
		if state == "" {
			state = "open"
		}
		store.mu.RLock()
		repo := store.lookup(owner, repoName)
		out := []any{}
		if repo != nil {
			for _, p := range repo.sortedPulls(false) {
				if state == "all" || p.State == state {
					out = append(out, pullView(repo, p))
				}
			}
		}
		store.mu.RUnlock()
		if repo == nil {
			return nil, -32602, "repository not found"
		}
		return map[string]any{"pull_requests": out}, 0, ""
	}
	return nil, -32601, "unknown tool: " + name
}
