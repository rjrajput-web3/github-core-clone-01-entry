// GitForge MCP server: JSON-RPC 2.0 over Streamable HTTP (POST /mcp), acting
// on the same in-memory store as the REST and GraphQL surfaces. Faithful port
// of the Node mcp.ts. The tools list is a precomputed constant so tools/list
// is byte-identical across calls.
package main

import (
	"encoding/json"
	"strings"
)

// toolsJSON is the exact JSON of the TOOLS array (field order as produced by
// the original implementation: name, description, inputSchema{type,
// properties, required}).
const toolsJSON = `[{"name":"create_repo","description":"Create a new repository for the authenticated user.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"},"description":{"type":"string","description":"Repository description"},"private":{"type":"boolean","description":"Whether the repository is private"}},"required":["name"]}},{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"}},"required":["owner","repo"]}},{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Issue title"},"body":{"type":"string","description":"Issue body"}},"required":["owner","repo","title"]}},{"name":"list_issues","description":"List issues in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Issue state filter: open, closed, or all"}},"required":["owner","repo"]}},{"name":"create_pull_request","description":"Create a pull request in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Pull request title"},"body":{"type":"string","description":"Pull request body"},"head":{"type":"string","description":"Head branch name"},"base":{"type":"string","description":"Base branch name"}},"required":["owner","repo","title","head","base"]}},{"name":"merge_pull_request","description":"Merge a pull request in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"pull_number":{"type":"integer","description":"Pull request number"}},"required":["owner","repo","pull_number"]}}]`

var toolRequired = map[string][]string{
	"create_repo":         {"name"},
	"get_repo":            {"owner", "repo"},
	"create_issue":        {"owner", "repo", "title"},
	"list_issues":         {"owner", "repo"},
	"create_pull_request": {"owner", "repo", "title", "head", "base"},
	"merge_pull_request":  {"owner", "repo", "pull_number"},
}

type mcpResult struct {
	Status int
	Body   string
}

func rpcResult(id interface{}, result string) mcpResult {
	return mcpResult{200, `{"jsonrpc":"2.0","id":` + string(marshalJSON(id)) + `,"result":` + result + `}`}
}

func rpcError(id interface{}, code int, message string, status int) mcpResult {
	return mcpResult{status, `{"jsonrpc":"2.0","id":` + string(marshalJSON(id)) + `,"error":{"code":` + itoa(code) + `,"message":` + qjson(message) + `}}`}
}

func toolResult(result interface{}) string {
	return `{"content":[{"type":"text","text":` + qjson(string(marshalJSON(result))) + `}],"structuredContent":` + string(marshalJSON(result)) + `,"isError":false}`
}

func toolError(message string) string {
	return `{"content":[{"type":"text","text":` + qjson(message) + `}],"structuredContent":null,"isError":true}`
}

func handleMcp(s *Store, rawBody []byte) mcpResult {
	var body interface{}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rpcError(nil, -32700, "Parse error", 400)
	}
	req, ok := body.(map[string]interface{})
	if !ok {
		return rpcError(nil, -32600, "Invalid Request", 400)
	}
	var id interface{}
	if v, ok := req["id"]; ok {
		id = v
	}
	method, _ := req["method"].(string)

	if method == "notifications/initialized" {
		return mcpResult{202, ""}
	}
	if method == "" {
		return rpcError(id, -32600, "Invalid Request", 200)
	}

	if method == "server/discover" {
		return rpcResult(id, `{"supportedVersions":["2026-07-28","2025-06-18","2025-03-26"],"capabilities":{"tools":{}},"serverInfo":{"name":"`+ForgeName+`","version":"`+ForgeVersion+`"}}`)
	}
	if method == "initialize" {
		params, _ := req["params"].(map[string]interface{})
		proto := "2026-07-28"
		if v, ok := params["protocolVersion"].(string); ok {
			proto = v
		}
		return rpcResult(id, `{"protocolVersion":`+qjson(proto)+`,"capabilities":{"tools":{}},"serverInfo":{"name":"`+ForgeName+`","version":"`+ForgeVersion+`"}}`)
	}
	if method == "ping" {
		return rpcResult(id, `{}`)
	}
	if method == "tools/list" {
		return rpcResult(id, `{"tools":`+toolsJSON+`}`)
	}
	if method == "tools/call" {
		params, _ := req["params"].(map[string]interface{})
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]interface{})
		if args == nil {
			args = map[string]interface{}{}
		}
		required, known := toolRequired[name]
		if !known {
			return rpcError(id, -32601, "Unknown tool: "+name, 200)
		}
		for _, reqArg := range required {
			v := args[reqArg]
			if v == nil || v == "" {
				return rpcError(id, -32602, "Missing required argument: "+reqArg, 200)
			}
		}
		result, errMsg := callTool(s, name, args)
		if errMsg != "" {
			return rpcResult(id, toolError(errMsg))
		}
		return rpcResult(id, result)
	}
	return rpcError(id, -32601, "Method not found: "+method, 200)
}

func callTool(s *Store, name string, args map[string]interface{}) (string, string) {
	owner, _ := args["owner"].(string)
	repoName, _ := args["repo"].(string)
	switch name {
	case "create_repo":
		repoNameArg, _ := args["name"].(string)
		desc, _ := args["description"].(string)
		priv, _ := args["private"].(bool)
		repo := s.createRepo(ViewerLogin, repoNameArg, desc, priv)
		if repo == nil {
			return "", "Repository already exists: " + ViewerLogin + "/" + repoNameArg
		}
		return toolResult(map[string]interface{}{
			"id": repo.ID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name,
			"owner": map[string]interface{}{"login": repo.Owner}, "private": repo.Private,
			"description": repo.Description, "default_branch": repo.DefaultBranch,
		}), ""
	case "get_repo":
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			return "", "Repository not found: " + owner + "/" + repoName
		}
		return toolResult(map[string]interface{}{
			"id": repo.ID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name,
			"owner": map[string]interface{}{"login": repo.Owner}, "private": repo.Private,
			"description": repo.Description, "default_branch": repo.DefaultBranch,
		}), ""
	case "create_issue":
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			return "", "Repository not found: " + owner + "/" + repoName
		}
		title, _ := args["title"].(string)
		body, _ := args["body"].(string)
		made, errStr := s.createIssue(repo, title, body, []string{})
		if errStr != "" {
			return "", errStr
		}
		return toolResult(mcpIssueJSON(repo, made)), ""
	case "list_issues":
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			return "", "Repository not found: " + owner + "/" + repoName
		}
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		var list []map[string]interface{}
		for _, i := range repo.Issues {
			if state == "all" || i.State == state {
				list = append(list, mcpIssueJSON(repo, i))
			}
		}
		// sort by number ascending
		for i := 0; i < len(list); i++ {
			for j := i + 1; j < len(list); j++ {
				if list[j]["number"].(int) < list[i]["number"].(int) {
					list[i], list[j] = list[j], list[i]
				}
			}
		}
		if list == nil {
			list = []map[string]interface{}{}
		}
		return toolResult(list), ""
	case "create_pull_request":
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			return "", "Repository not found: " + owner + "/" + repoName
		}
		title, _ := args["title"].(string)
		body, _ := args["body"].(string)
		head, _ := args["head"].(string)
		base, _ := args["base"].(string)
		made, errStr := s.createPull(repo, title, body, head, base)
		if errStr != "" {
			return "", errStr
		}
		return toolResult(mcpPRJSON(repo, made)), ""
	case "merge_pull_request":
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			return "", "Repository not found: " + owner + "/" + repoName
		}
		pr := repo.Pulls[int(toFloat(args["pull_number"]))]
		if pr == nil {
			return "", "Pull request not found: " + strings.TrimSpace(jsonString(args["pull_number"]))
		}
		ok, sha, reason := s.mergePull(repo, pr)
		if !ok {
			return "", reason
		}
		return toolResult(map[string]interface{}{
			"merged": true, "sha": sha, "message": "Merge pull request #" + itoa(pr.Number) + " from " + pr.HeadRef,
		}), ""
	}
	return "", "Unknown tool: " + name
}

func jsonString(v interface{}) string {
	if v == nil {
		return ""
	}
	return strings.Trim(string(marshalJSON(v)), `"`)
}

func mcpIssueJSON(repo *Repo, issue *Issue) map[string]interface{} {
	var labels []map[string]interface{}
	for _, name := range issue.Labels {
		labels = append(labels, map[string]interface{}{"name": name})
	}
	if labels == nil {
		labels = []map[string]interface{}{}
	}
	return map[string]interface{}{
		"id": issue.ID, "number": issue.Number, "title": issue.Title, "body": issue.Body,
		"state": issue.State, "labels": labels, "user": map[string]interface{}{"login": issue.User},
		"created_at": issue.CreatedAt, "updated_at": issue.UpdatedAt,
		"html_url": "/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
	}
}

func mcpPRJSON(repo *Repo, pr *Pull) map[string]interface{} {
	headSha := pr.HeadSha
	if v, ok := repo.Refs["refs/heads/"+pr.HeadRef]; ok {
		headSha = v
	}
	baseSha := pr.BaseSha
	if v, ok := repo.Refs["refs/heads/"+pr.BaseRef]; ok {
		baseSha = v
	}
	return map[string]interface{}{
		"id": pr.ID, "number": pr.Number, "title": pr.Title, "body": pr.Body,
		"state": pr.State, "merged": pr.Merged,
		"head": map[string]interface{}{"ref": pr.HeadRef, "sha": headSha},
		"base": map[string]interface{}{"ref": pr.BaseRef, "sha": baseSha},
		"merged_at":      nullableStr(pr.MergedAt),
		"merge_commit_sha": nullableStr(pr.MergeCommitSha),
		"html_url":        "/" + repo.Owner + "/" + repo.Name + "/pull/" + itoa(pr.Number),
	}
}

func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
