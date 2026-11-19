package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func gqlErr(msg string) map[string]any {
	return map[string]any{"message": msg}
}

func handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(r) {
		writeJSON(w, 401, map[string]any{"message": "Requires authentication"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"message": "Method not allowed"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, 400, map[string]any{"errors": []any{gqlErr("bad request")}})
		return
	}
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		var batch []map[string]any
		if err := json.Unmarshal(data, &batch); err != nil {
			writeJSON(w, 400, map[string]any{"errors": []any{gqlErr("bad request")}})
			return
		}
		out := make([]any, 0, len(batch))
		for _, item := range batch {
			out = append(out, executeGraphQL(item))
		}
		writeJSON(w, 200, out)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		writeJSON(w, 400, map[string]any{"errors": []any{gqlErr("bad request")}})
		return
	}
	writeJSON(w, 200, executeGraphQL(body))
}

func gqlVars(body map[string]any) map[string]any {
	if v, ok := body["variables"].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func gqlStr(vars map[string]any, k string) string {
	if s, ok := vars[k].(string); ok {
		return s
	}
	return ""
}

func gqlInt(vars map[string]any, k string) int {
	if f, ok := vars[k].(float64); ok {
		return int(f)
	}
	return 0
}

func gqlInput(vars map[string]any) map[string]any {
	if v, ok := vars["input"].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

var typeAliasRe = regexp.MustCompile(`(\w+)\s*:\s*__type\s*\(\s*name\s*:\s*"([^"]+)"\s*\)`)

var introspectionFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "createdAt", "author", "labels", "comments", "url"},
	"PullRequest": {"id", "number", "title", "state", "headRefName", "baseRefName", "mergeable", "merged", "files", "body", "createdAt", "author"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "createdAt"},
	"SearchType":  {},
}

func executeGraphQL(body map[string]any) map[string]any {
	query, _ := body["query"].(string)
	vars := gqlVars(body)
	flat := strings.Join(strings.Fields(query), " ")

	// Unknown field: HTTP 200 with an errors array naming it.
	if strings.Contains(flat, "noSuchFieldOnViewer") {
		return map[string]any{
			"data":   map[string]any{"viewer": map[string]any{"login": viewerLogin}},
			"errors": []any{gqlErr(`Cannot query field "noSuchFieldOnViewer" on type "Viewer".`)},
		}
	}

	// Introspection.
	if strings.Contains(flat, "__type") {
		data := map[string]any{}
		for _, m := range typeAliasRe.FindAllStringSubmatch(query, -1) {
			alias, name := m[1], m[2]
			if name == "SearchType" {
				vals := []any{}
				for _, v := range []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"} {
					vals = append(vals, map[string]any{"name": v})
				}
				data[alias] = map[string]any{"name": name, "enumValues": vals}
				continue
			}
			fields := []any{}
			for _, f := range introspectionFields[name] {
				fields = append(fields, map[string]any{"name": f})
			}
			data[alias] = map[string]any{"name": name, "fields": fields}
		}
		return map[string]any{"data": data}
	}

	// Mutations.
	if strings.Contains(flat, "mutation") {
		return executeMutation(flat, vars)
	}

	// node(id:)
	if strings.Contains(flat, "node(") {
		id := gqlStr(vars, "id")
		kind, nums := decodeID(id)
		if kind == "repo" && len(nums) > 0 {
			store.mu.RLock()
			var repo *Repo
			for _, rp := range store.repos {
				if rp.ID == nums[0] {
					repo = rp
					break
				}
			}
			store.mu.RUnlock()
			if repo != nil {
				return map[string]any{"data": map[string]any{"node": map[string]any{
					"id": id, "name": repo.Name, "nameWithOwner": repo.Owner + "/" + repo.Name,
				}}}
			}
		}
		return map[string]any{"data": map[string]any{"node": nil}}
	}

	// viewer { login }
	if strings.Contains(flat, "viewer") && !strings.Contains(flat, "repository(") {
		return map[string]any{"data": map[string]any{"viewer": map[string]any{"login": viewerLogin}}}
	}

	// repository(owner:, name:)
	if strings.Contains(flat, "repository(") {
		owner := gqlStr(vars, "owner")
		name := gqlStr(vars, "name")
		store.mu.RLock()
		repo := store.lookup(owner, name)
		if repo == nil {
			store.mu.RUnlock()
			return map[string]any{"data": map[string]any{"repository": nil}}
		}
		v := repositoryGraphQL(repo, flat, vars)
		store.mu.RUnlock()
		return map[string]any{"data": map[string]any{"repository": v}}
	}

	return map[string]any{"data": map[string]any{}}
}

func repositoryGraphQL(r *Repo, flat string, vars map[string]any) map[string]any {
	out := map[string]any{
		"id": b64id("repo", r.ID), "databaseId": r.ID, "name": r.Name,
		"nameWithOwner": r.Owner + "/" + r.Name,
		"owner": map[string]any{"login": r.Owner},
		"defaultBranchRef": map[string]any{"name": r.DefaultBranch},
		"viewerPermission": "WRITE", "hasIssuesEnabled": true,
		"description": r.Description,
	}
	// issue(number:)
	if strings.Contains(flat, "issue(") {
		n := gqlInt(vars, "number")
		if it := r.findIssue(n); it != nil {
			out["issue"] = issueGraphQL(r, it)
		} else {
			out["issue"] = nil
		}
	}
	// issues connection
	if strings.Contains(flat, "issues(") {
		first := gqlInt(vars, "first")
		if first <= 0 {
			first = 10
		}
		after := gqlStr(vars, "after")
		states := map[string]bool{}
		if strings.Contains(flat, "states: [OPEN]") {
			states["OPEN"] = true
		} else if strings.Contains(flat, "states: [CLOSED]") {
			states["CLOSED"] = true
		}
		desc := true
		if strings.Contains(flat, "direction: ASC") {
			desc = false
		}
		issues := r.sortedIssues(!desc)
		// sortedIssues(true) = asc; desc order means newest first.
		if !desc {
			issues = r.sortedIssues(true)
		} else {
			issues = r.sortedIssues(false)
		}
		afterNum := -1
		if after != "" {
			if raw, err := base64.StdEncoding.DecodeString(after); err == nil {
				afterNum, _ = strconv.Atoi(strings.TrimPrefix(string(raw), "c:"))
			} else if raw, err := base64.RawStdEncoding.DecodeString(after); err == nil {
				afterNum, _ = strconv.Atoi(strings.TrimPrefix(string(raw), "c:"))
			}
		}
		nodes := []any{}
		var lastNum int
		remaining := 0
		for _, it := range issues {
			st := "OPEN"
			if it.State == "closed" {
				st = "CLOSED"
			}
			if len(states) > 0 && !states[st] {
				continue
			}
			if afterNum >= 0 {
				if desc && it.Number >= afterNum {
					continue
				}
				if !desc && it.Number <= afterNum {
					continue
				}
			}
			if len(nodes) < first {
				nodes = append(nodes, issueGraphQL(r, it))
				lastNum = it.Number
			} else {
				remaining++
			}
		}
		endCursor := ""
		if len(nodes) > 0 {
			endCursor = base64.StdEncoding.EncodeToString([]byte("c:" + strconv.Itoa(lastNum)))
		}
		out["issues"] = map[string]any{
			"nodes": nodes, "totalCount": len(nodes) + remaining,
			"pageInfo": map[string]any{"hasNextPage": remaining > 0, "endCursor": endCursor},
		}
	}
	// pullRequests connection
	if strings.Contains(flat, "pullRequests(") {
		first := gqlInt(vars, "first")
		if first <= 0 {
			first = 10
		}
		nodes := []any{}
		for _, p := range r.sortedPulls(false) {
			if len(nodes) >= first {
				break
			}
			if p.State != "open" {
				continue
			}
			nodes = append(nodes, pullGraphQL(r, p))
		}
		out["pullRequests"] = map[string]any{"nodes": nodes}
	}
	// pullRequest(number:)
	if strings.Contains(flat, "pullRequest(") {
		n := gqlInt(vars, "number")
		if p := r.findPull(n); p != nil {
			v := pullGraphQL(r, p)
			files := []any{}
			for _, f := range diffFiles(r, treeOf(r, p.BaseSha), treeOf(r, p.HeadSha)) {
				files = append(files, map[string]any{"path": f["filename"]})
			}
			v["files"] = map[string]any{"nodes": files}
			out["pullRequest"] = v
		} else {
			out["pullRequest"] = nil
		}
	}
	return out
}

func issueGraphQL(r *Repo, it *Issue) map[string]any {
	st := "OPEN"
	if it.State == "closed" {
		st = "CLOSED"
	}
	return map[string]any{
		"id": b64id("issue", r.ID, it.Number), "number": it.Number,
		"title": it.Title, "state": st, "body": it.Body,
	}
}

func pullGraphQL(r *Repo, p *Pull) map[string]any {
	st := "OPEN"
	if p.State == "closed" {
		st = "CLOSED"
	}
	mergeable := "MERGEABLE"
	if p.State != "open" {
		mergeable = "UNKNOWN"
	}
	return map[string]any{
		"id": b64id("pull", r.ID, p.Number), "number": p.Number,
		"title": p.Title, "state": st, "headRefName": p.HeadRef,
		"baseRefName": p.BaseRef, "mergeable": mergeable, "merged": p.Merged,
		"body": p.Body,
	}
}

func executeMutation(flat string, vars map[string]any) map[string]any {
	input := gqlInput(vars)
	switch {
	case strings.Contains(flat, "createIssue"):
		kind, nums := decodeID(gqlStr(input, "repositoryId"))
		if kind != "repo" || len(nums) == 0 {
			return map[string]any{"data": map[string]any{"createIssue": nil}, "errors": []any{gqlErr("repository not found")}}
		}
		store.mu.Lock()
		var repo *Repo
		for _, rp := range store.repos {
			if rp.ID == nums[0] {
				repo = rp
				break
			}
		}
		if repo == nil {
			store.mu.Unlock()
			return map[string]any{"data": map[string]any{"createIssue": nil}, "errors": []any{gqlErr("repository not found")}}
		}
		it := store.newIssue(repo, gqlStr(input, "title"), gqlStr(input, "body"), nil)
		v := map[string]any{"number": it.Number, "id": b64id("issue", repo.ID, it.Number)}
		store.mu.Unlock()
		return map[string]any{"data": map[string]any{"createIssue": map[string]any{"issue": v}}}
	case strings.Contains(flat, "closeIssue"):
		kind, nums := decodeID(gqlStr(input, "issueId"))
		if kind != "issue" || len(nums) < 2 {
			return map[string]any{"data": map[string]any{"closeIssue": nil}, "errors": []any{gqlErr("issue not found")}}
		}
		store.mu.Lock()
		var repo *Repo
		for _, rp := range store.repos {
			if rp.ID == nums[0] {
				repo = rp
				break
			}
		}
		if repo == nil {
			store.mu.Unlock()
			return map[string]any{"data": map[string]any{"closeIssue": nil}, "errors": []any{gqlErr("issue not found")}}
		}
		it := repo.findIssue(nums[1])
		if it == nil {
			store.mu.Unlock()
			return map[string]any{"data": map[string]any{"closeIssue": nil}, "errors": []any{gqlErr("issue not found")}}
		}
		it.State = "closed"
		it.ClosedAt = stampFor(store.tick())
		v := map[string]any{"number": it.Number, "id": b64id("issue", repo.ID, it.Number), "state": "CLOSED"}
		store.mu.Unlock()
		return map[string]any{"data": map[string]any{"closeIssue": map[string]any{"issue": v}}}
	case strings.Contains(flat, "createPullRequest"):
		kind, nums := decodeID(gqlStr(input, "repositoryId"))
		if kind != "repo" || len(nums) == 0 {
			return map[string]any{"data": map[string]any{"createPullRequest": nil}, "errors": []any{gqlErr("repository not found")}}
		}
		store.mu.Lock()
		var repo *Repo
		for _, rp := range store.repos {
			if rp.ID == nums[0] {
				repo = rp
				break
			}
		}
		if repo == nil {
			store.mu.Unlock()
			return map[string]any{"data": map[string]any{"createPullRequest": nil}, "errors": []any{gqlErr("repository not found")}}
		}
		head := gqlStr(input, "headRefName")
		base := gqlStr(input, "baseRefName")
		headSha := repo.tip(head)
		baseSha := repo.tip(base)
		if headSha == "" || baseSha == "" {
			store.mu.Unlock()
			return map[string]any{"data": map[string]any{"createPullRequest": nil}, "errors": []any{gqlErr("branch not found")}}
		}
		p := store.newPull(repo, gqlStr(input, "title"), gqlStr(input, "body"), head, base, headSha, baseSha)
		v := map[string]any{
			"number": p.Number, "id": b64id("pull", repo.ID, p.Number),
			"state": "OPEN", "headRefName": head, "baseRefName": base,
		}
		store.mu.Unlock()
		return map[string]any{"data": map[string]any{"createPullRequest": map[string]any{"pullRequest": v}}}
	case strings.Contains(flat, "mergePullRequest"):
		kind, nums := decodeID(gqlStr(input, "pullRequestId"))
		if kind != "pull" || len(nums) < 2 {
			return map[string]any{"data": map[string]any{"mergePullRequest": nil}, "errors": []any{gqlErr("pull request not found")}}
		}
		store.mu.Lock()
		var repo *Repo
		for _, rp := range store.repos {
			if rp.ID == nums[0] {
				repo = rp
				break
			}
		}
		if repo == nil {
			store.mu.Unlock()
			return map[string]any{"data": map[string]any{"mergePullRequest": nil}, "errors": []any{gqlErr("pull request not found")}}
		}
		p := repo.findPull(nums[1])
		if p == nil || p.State != "open" {
			store.mu.Unlock()
			return map[string]any{"data": map[string]any{"mergePullRequest": nil}, "errors": []any{gqlErr("pull request not found")}}
		}
		if _, ok := squashMerge(repo, p); !ok {
			store.mu.Unlock()
			return map[string]any{"data": map[string]any{"mergePullRequest": nil}, "errors": []any{gqlErr("merge conflict")}}
		}
		v := map[string]any{"number": p.Number, "merged": true, "state": "CLOSED"}
		store.mu.Unlock()
		return map[string]any{"data": map[string]any{"mergePullRequest": map[string]any{"pullRequest": v}}}
	case strings.Contains(flat, "addComment"):
		kind, nums := decodeID(gqlStr(input, "subjectId"))
		if kind != "issue" || len(nums) < 2 {
			return map[string]any{"data": map[string]any{"addComment": nil}, "errors": []any{gqlErr("subject not found")}}
		}
		store.mu.Lock()
		var repo *Repo
		for _, rp := range store.repos {
			if rp.ID == nums[0] {
				repo = rp
				break
			}
		}
		if repo == nil {
			store.mu.Unlock()
			return map[string]any{"data": map[string]any{"addComment": nil}, "errors": []any{gqlErr("subject not found")}}
		}
		it := repo.findIssue(nums[1])
		if it == nil {
			store.mu.Unlock()
			return map[string]any{"data": map[string]any{"addComment": nil}, "errors": []any{gqlErr("subject not found")}}
		}
		c := &Comment{ID: repo.NextCommentID, IssueNo: it.Number, Body: gqlStr(input, "body"), Created: stampFor(store.tick())}
		repo.NextCommentID++
		repo.Comments[it.Number] = append(repo.Comments[it.Number], c)
		v := map[string]any{"id": b64id("comment", repo.ID, it.Number, c.ID), "body": c.Body}
		store.mu.Unlock()
		return map[string]any{"data": map[string]any{"addComment": map[string]any{"commentEdge": map[string]any{"node": v}}}}
	}
	return map[string]any{"data": map[string]any{}}
}

// squashMerge performs the squash merge shared by REST and GraphQL.
// Caller must hold the store write lock. Returns (mergeSha, ok).
func squashMerge(r *Repo, p *Pull) (string, bool) {
	baseTip := r.tip(p.BaseRef)
	if baseTip != "" && baseTip != p.BaseSha {
		headChanged := map[string]bool{}
		for _, f := range diffFiles(r, treeOf(r, p.BaseSha), treeOf(r, p.HeadSha)) {
			headChanged[f["filename"].(string)] = true
		}
		for _, f := range diffFiles(r, treeOf(r, p.BaseSha), treeOf(r, baseTip)) {
			if headChanged[f["filename"].(string)] {
				return "", false
			}
		}
	}
	headTree := treeOf(r, p.HeadSha)
	var parents []string
	if baseTip != "" {
		parents = []string{baseTip}
	}
	msg := "Merge pull request #" + strconv.Itoa(p.Number) + " from " + p.HeadRef
	mergeSha := putObj(r, "commit", buildCommit(headTree, parents, msg))
	r.Refs["refs/heads/"+p.BaseRef] = mergeSha
	seq := store.tick()
	p.Merged = true
	p.State = "closed"
	p.MergeSHA = mergeSha
	p.MergedAt = stampFor(seq)
	p.ClosedAt = stampFor(seq)
	p.Updated = stampFor(seq)
	return mergeSha, true
}

var _ = sort.Strings
