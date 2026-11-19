// GitForge HTTP server: smart-HTTP git, REST v3, GraphQL v4 and MCP on one
// Go http server, zero dependencies, in-memory store. Faithful port of the
// Node app.ts, including the issue-GET fast path and cached issue JSON.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var requestCounter int64

func nextRequestID() string {
	return "gf-" + itoa(int(atomic.AddInt64(&requestCounter, 1)))
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 64<<20+1))
}

var identRe = regexp.MustCompile(`^(.*) <(.*)> (\d+) ([+-]\d{4})$`)

type ident struct {
	name, email, date string
}

func parseIdent(line string) ident {
	m := identRe.FindStringSubmatch(line)
	if m == nil {
		return ident{line, "", isoNow()}
	}
	ts, _ := strconv.ParseInt(m[3], 10, 64)
	return ident{m[1], m[2], time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05.000Z")}
}

func stripTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s[:len(s)-1]
	}
	return s
}

func queryParam(query, key string) (string, bool) {
	if query == "" {
		return "", false
	}
	for _, part := range strings.Split(query, "&") {
		eq := strings.Index(part, "=")
		k := part
		if eq >= 0 {
			k = part[:eq]
		}
		if k == key {
			if eq < 0 {
				return "", true
			}
			v, err := url.QueryUnescape(strings.ReplaceAll(part[eq+1:], "+", " "))
			if err != nil {
				return part[eq+1:], true
			}
			return v, true
		}
	}
	return "", false
}

func paginate[T any](items []T, perPage, page int) ([]T, bool, int) {
	totalPages := (len(items) + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	start := (page - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], page < totalPages, totalPages
}

func marshalJSON(v interface{}) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

func qjson(s string) string {
	// Fast path: most strings need no escaping at all.
	needsEscape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' || c >= 0x80 {
			needsEscape = true
			break
		}
	}
	if !needsEscape {
		return `"` + s + `"`
	}
	return string(marshalJSON(s))
}

func repoJSON(repo *Repo) []byte {
	openIssues := 0
	for _, i := range repo.Issues {
		if i.State == "open" {
			openIssues++
		}
	}
	ownerID := 0
	if repo.Owner == ViewerLogin {
		ownerID = ViewerID
	}
	var b bytes.Buffer
	b.WriteString(`{"id":` + itoa(repo.ID))
	b.WriteString(`,"node_id":` + qjson(gid("R", repo.ID)))
	b.WriteString(`,"name":` + qjson(repo.Name))
	b.WriteString(`,"full_name":` + qjson(repo.Owner+"/"+repo.Name))
	b.WriteString(`,"owner":{"login":` + qjson(repo.Owner) + `,"id":` + itoa(ownerID) + `,"type":"User"}`)
	b.WriteString(`,"private":` + boolStr(repo.Private))
	b.WriteString(`,"description":` + qjson(repo.Description))
	b.WriteString(`,"default_branch":` + qjson(repo.DefaultBranch))
	b.WriteString(`,"created_at":` + qjson(repo.CreatedAt))
	b.WriteString(`,"updated_at":` + qjson(repo.CreatedAt))
	b.WriteString(`,"html_url":` + qjson("/"+repo.Owner+"/"+repo.Name))
	b.WriteString(`,"clone_url":` + qjson("/"+repo.Owner+"/"+repo.Name+".git"))
	b.WriteString(`,"git_url":` + qjson("/"+repo.Owner+"/"+repo.Name+".git"))
	b.WriteString(`,"ssh_url":` + qjson("git@gitforge.invalid:"+repo.Owner+"/"+repo.Name+".git"))
	b.WriteString(`,"homepage":null,"language":null,"topics":[]`)
	b.WriteString(`,"visibility":` + qjson(map[bool]string{true: "private", false: "public"}[repo.Private]))
	b.WriteString(`,"fork":false,"watchers_count":0,"stargazers_count":0,"forks_count":0`)
	b.WriteString(`,"open_issues_count":` + itoa(openIssues))
	b.WriteString(`,"has_issues":true,"has_projects":false,"has_wiki":false,"has_pages":false,"archived":false,"disabled":false}`)
	return b.Bytes()
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func issueJSON(issue *Issue, repo *Repo) []byte {
	if issue.JSONBuf != nil {
		return issue.JSONBuf
	}
	var b bytes.Buffer
	userID := 0
	if issue.User == ViewerLogin {
		userID = ViewerID
	}
	b.WriteString(`{"id":` + itoa(issue.ID))
	b.WriteString(`,"node_id":` + qjson(gid("I", repo.ID, issue.Number)))
	b.WriteString(`,"number":` + itoa(issue.Number))
	b.WriteString(`,"title":` + qjson(issue.Title))
	b.WriteString(`,"body":` + qjson(issue.Body))
	b.WriteString(`,"state":` + qjson(issue.State))
	b.WriteString(`,"labels":[`)
	for i, name := range issue.Labels {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":` + qjson(name) + `}`)
	}
	b.WriteString(`]`)
	b.WriteString(`,"user":{"login":` + qjson(issue.User) + `,"id":` + itoa(userID) + `,"type":"User"}`)
	b.WriteString(`,"created_at":` + qjson(issue.CreatedAt))
	b.WriteString(`,"updated_at":` + qjson(issue.UpdatedAt))
	b.WriteString(`,"comments":` + itoa(len(issue.Comments)))
	b.WriteString(`,"html_url":` + qjson("/"+repo.Owner+"/"+repo.Name+"/issues/"+itoa(issue.Number)))
	b.WriteString(`,"comments_url":` + qjson("/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues/"+itoa(issue.Number)+"/comments"))
	b.WriteString(`}`)
	issue.JSONBuf = b.Bytes()
	return issue.JSONBuf
}

func prJSON(repo *Repo, pr *Pull) []byte {
	headSha := pr.HeadSha
	if v, ok := repo.Refs["refs/heads/"+pr.HeadRef]; ok {
		headSha = v
	}
	baseSha := pr.BaseSha
	if v, ok := repo.Refs["refs/heads/"+pr.BaseRef]; ok {
		baseSha = v
	}
	state := pr.State
	if pr.Merged {
		state = "closed"
	}
	userID := 0
	if pr.User == ViewerLogin {
		userID = ViewerID
	}
	var b bytes.Buffer
	b.WriteString(`{"id":` + itoa(pr.ID))
	b.WriteString(`,"node_id":` + qjson(gid("P", repo.ID, pr.Number)))
	b.WriteString(`,"number":` + itoa(pr.Number))
	b.WriteString(`,"title":` + qjson(pr.Title))
	b.WriteString(`,"body":` + qjson(pr.Body))
	b.WriteString(`,"state":` + qjson(state))
	b.WriteString(`,"merged":` + boolStr(pr.Merged))
	b.WriteString(`,"merged_at":` + qjsonOrNull(pr.MergedAt))
	b.WriteString(`,"merge_commit_sha":` + qjsonOrNull(pr.MergeCommitSha))
	b.WriteString(`,"head":{"ref":` + qjson(pr.HeadRef) + `,"sha":` + qjson(headSha) + `,"label":` + qjson(repo.Owner+":"+pr.HeadRef) + `}`)
	b.WriteString(`,"base":{"ref":` + qjson(pr.BaseRef) + `,"sha":` + qjson(baseSha) + `,"label":` + qjson(repo.Owner+":"+pr.BaseRef) + `}`)
	b.WriteString(`,"user":{"login":` + qjson(pr.User) + `,"id":` + itoa(userID) + `,"type":"User"}`)
	b.WriteString(`,"created_at":` + qjson(pr.CreatedAt))
	b.WriteString(`,"updated_at":` + qjson(pr.UpdatedAt))
	b.WriteString(`,"html_url":` + qjson("/"+repo.Owner+"/"+repo.Name+"/pull/"+itoa(pr.Number)))
	b.WriteString(`}`)
	return b.Bytes()
}

func qjsonOrNull(s string) string {
	if s == "" {
		return "null"
	}
	return qjson(s)
}

func sendJSON(w http.ResponseWriter, status int, body []byte, requestID string, extra map[string]string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", itoa(len(body)))
	h.Set("X-GitHub-Request-Id", requestID)
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(body)
}

func sendErr(w http.ResponseWriter, status int, msg, requestID string) {
	sendJSON(w, status, []byte(`{"message":`+qjson(msg)+`}`), requestID, nil)
}

func sendGit(w http.ResponseWriter, status int, contentType string, body []byte, requestID string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", itoa(len(body)))
	h.Set("X-GitHub-Request-Id", requestID)
	w.WriteHeader(status)
	w.Write(body)
}

func resolveTip(s *Store, repo *Repo, ref string) (string, bool) {
	if ref == "" {
		return s.defaultTip(repo)
	}
	if tip, ok := repo.Refs["refs/heads/"+ref]; ok {
		return tip, true
	}
	if len(ref) == 40 && isHex40(ref) {
		if s.getObject(ref) != nil {
			return s.peelToCommit(ref), true
		}
	}
	return "", false
}

func isHex40(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func countLines(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	n := 0
	for _, b := range buf {
		if b == '\n' {
			n++
		}
	}
	if buf[len(buf)-1] != '\n' {
		n++
	}
	return n
}

func decodeSeg(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

func parseJSONBody(r *http.Request) (map[string]interface{}, bool) {
	body, err := readBody(r)
	if err != nil || len(body) > 64<<20 {
		return nil, false
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}
	return parsed, true
}

func jstr(m map[string]interface{}, key, def string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return def
}

func jbool(m map[string]interface{}, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func jnum(m map[string]interface{}, key string, def float64) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return def
}

func perPageOf(query string) int {
	v, ok := queryParam(query, "per_page")
	if !ok {
		return 30
	}
	n, err := strconv.Atoi(v)
	if err != nil || n == 0 {
		return 30
	}
	if n < 1 {
		return 1
	}
	if n > 100 {
		return 100
	}
	return n
}

func pageOf(query string) int {
	v, ok := queryParam(query, "page")
	if !ok {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n == 0 {
		return 1
	}
	if n < 1 {
		return 1
	}
	return n
}

var store = NewStore()

func handler(w http.ResponseWriter, r *http.Request) {
	requestID := nextRequestID()
	method := r.Method
	rawPath := r.URL.EscapedPath()
	if rawPath == "" {
		rawPath = r.URL.Path
	}
	query := r.URL.RawQuery
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	origin := "http://" + host

	if method == "GET" && rawPath == "/healthz" {
		sendJSON(w, 200, []byte(`{"ok":true}`), requestID, nil)
		return
	}

	// git smart HTTP (no auth)
	if idx := strings.LastIndex(rawPath, ".git/"); idx > 0 {
		ownerEnd := strings.Index(rawPath[1:], "/") + 1
		owner := decodeSeg(rawPath[1:ownerEnd])
		repoName := decodeSeg(rawPath[ownerEnd+1 : idx])
		rest := rawPath[idx+5:]
		if rest == "info/refs" {
			service, _ := queryParam(query, "service")
			wantV2 := r.Header.Get("Git-Protocol") == "version=2"
			out := handleInfoRefs(store, owner, repoName, service, wantV2)
			sendGit(w, out.Status, out.ContentType, out.Body, requestID)
			return
		}
		if rest == "git-upload-pack" && method == "POST" {
			body, _ := readBody(r)
			out := handleUploadPack(store, owner, repoName, body, r.Header.Get("Git-Protocol") == "version=2")
			sendGit(w, out.Status, out.ContentType, out.Body, requestID)
			return
		}
		if rest == "git-receive-pack" && method == "POST" {
			body, _ := readBody(r)
			out := handleReceivePack(store, owner, repoName, body)
			sendGit(w, out.Status, out.ContentType, out.Body, requestID)
			return
		}
		sendErr(w, 404, "Not Found", requestID)
		return
	}

	authed := false
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") && strings.TrimSpace(a[7:]) != "" {
		authed = true
	}

	// MCP
	if rawPath == "/mcp" {
		if method != "POST" {
			sendErr(w, 405, "Method Not Allowed", requestID)
			return
		}
		if !authed {
			sendJSON(w, 401, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Unauthorized"}}`), requestID, nil)
			return
		}
		body, _ := readBody(r)
		out := handleMcp(store, body)
		if out.Status == 202 {
			w.Header().Set("X-GitHub-Request-Id", requestID)
			w.WriteHeader(202)
			return
		}
		sendJSON(w, out.Status, []byte(out.Body), requestID, nil)
		return
	}

	// GraphQL
	if rawPath == "/api/graphql" {
		if method != "POST" {
			sendErr(w, 405, "Method Not Allowed", requestID)
			return
		}
		if !authed {
			sendErr(w, 401, "Requires authentication", requestID)
			return
		}
		body, _ := readBody(r)
		var parsed interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			sendErr(w, 400, "Invalid JSON", requestID)
			return
		}
		out := executeGraphQL(store, parsed)
		sendJSON(w, out.Status, marshalJSON(out.JSON), requestID, nil)
		return
	}

	// REST v3 root
	if rawPath == "/api/v3" || rawPath == "/api/v3/" {
		if method != "GET" {
			sendErr(w, 404, "Not Found", requestID)
			return
		}
		sendJSON(w, 200, marshalJSON(map[string]interface{}{
			"current_user_url": origin + "/api/v3/user",
			"repository_url":   origin + "/api/v3/repos/{owner}/{repo}",
			"user_url":         origin + "/api/v3/users/{username}",
			"rate_limit_url":   origin + "/api/v3/rate_limit",
			"meta_url":         origin + "/api/v3/meta",
		}), requestID, map[string]string{"X-OAuth-Scopes": "repo, read:org"})
		return
	}
	if rawPath == "/api/v3/meta" && method == "GET" {
		sendJSON(w, 200, marshalJSON(map[string]interface{}{
			"installed_version":                 ForgeVersion,
			"verifiable_password_authentication": true,
			"github_enterprise":                 false,
			"packages":                          []interface{}{},
		}), requestID, nil)
		return
	}
	if !strings.HasPrefix(rawPath, "/api/v3/") {
		sendErr(w, 404, "Not Found", requestID)
		return
	}
	if !authed {
		sendErr(w, 401, "Requires authentication", requestID)
		return
	}

	// Fast path: GET /api/v3/repos/{owner}/{repo}/issues/{n}
	if method == "GET" && strings.HasPrefix(rawPath, "/api/v3/repos/") {
		rest := rawPath[len("/api/v3/repos/"):]
		s1 := strings.Index(rest, "/")
		s2 := -1
		if s1 >= 0 {
			if j := strings.Index(rest[s1+1:], "/"); j >= 0 {
				s2 = s1 + 1 + j
			}
		}
		if s1 > 0 && s2 > s1 {
			tail := rest[s2+1:]
			if strings.HasPrefix(tail, "issues/") {
				numStr := tail[len("issues/"):]
				if isDigits(numStr) {
					owner := decodeSeg(rest[:s1])
					repoName := decodeSeg(rest[s1+1 : s2])
					repo := store.getRepo(owner, repoName)
					var issue *Issue
					if repo != nil && owner == ViewerLogin {
						issue = repo.Issues[atoi(numStr)]
					}
					if issue != nil && repo != nil {
						sendJSON(w, 200, issueJSON(issue, repo), requestID, nil)
						return
					}
				}
			}
		}
	}

	segs := []string{}
	for _, s := range strings.Split(rawPath, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) >= 3 && segs[2] == "user" {
		handleUserRoutes(w, r, method, segs, query, origin, requestID)
		return
	}
	if len(segs) >= 3 && segs[2] == "repos" && len(segs) >= 5 {
		owner := decodeSeg(segs[3])
		repoName := decodeSeg(segs[4])
		handleRepoRoutes(w, r, method, owner, repoName, segs[5:], query, requestID)
		return
	}
	sendErr(w, 404, "Not Found", requestID)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func handleUserRoutes(w http.ResponseWriter, r *http.Request, method string, segs []string, query, origin, requestID string) {
	if len(segs) == 3 && method == "GET" && segs[2] == "user" {
		sendJSON(w, 200, marshalJSON(map[string]interface{}{
			"login":      ViewerLogin,
			"id":         ViewerID,
			"node_id":    gid("U", ViewerLogin),
			"name":       ViewerName,
			"email":      ViewerEmail,
			"type":       "User",
			"avatar_url": "",
			"html_url":   "/" + ViewerLogin,
			"created_at": "1970-01-01T00:00:00.000Z",
			"updated_at": isoNow(),
		}), requestID, nil)
		return
	}
	if len(segs) == 4 && segs[3] == "repos" {
		if method == "GET" {
			perPage := perPageOf(query)
			page := pageOf(query)
			all := store.listViewerRepos()
			items, hasNext, _ := paginate(all, perPage, page)
			var extra map[string]string
			if hasNext {
				extra = map[string]string{"Link": "<" + origin + "/api/v3/user/repos?per_page=" + itoa(perPage) + "&page=" + itoa(page+1) + ">; rel=\"next\""}
			}
			var out [][]byte
			for _, repo := range items {
				out = append(out, repoJSON(repo))
			}
			sendJSON(w, 200, joinJSONArr(out), requestID, extra)
			return
		}
		if method == "POST" {
			parsed, ok := parseJSONBody(r)
			if !ok {
				sendErr(w, 400, "Invalid JSON", requestID)
				return
			}
			name := jstr(parsed, "name", "")
			if name == "" {
				sendErr(w, 422, "name is required", requestID)
				return
			}
			repo := store.createRepo(ViewerLogin, name, jstr(parsed, "description", ""), jbool(parsed, "private"))
			if repo == nil {
				sendErr(w, 422, "Repository already exists", requestID)
				return
			}
			sendJSON(w, 201, repoJSON(repo), requestID, nil)
			return
		}
	}
	sendErr(w, 404, "Not Found", requestID)
}

func joinJSONArr(items [][]byte) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(it)
	}
	b.WriteByte(']')
	return b.Bytes()
}

func handleRepoRoutes(w http.ResponseWriter, r *http.Request, method, owner, repoName string, tail []string, query, requestID string) {
	repo := store.getRepo(owner, repoName)
	needRepo := func() *Repo {
		if repo == nil || owner != ViewerLogin {
			return nil
		}
		return repo
	}

	if len(tail) == 0 {
		if method == "GET" {
			rp := needRepo()
			if rp == nil {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			sendJSON(w, 200, repoJSON(rp), requestID, nil)
			return
		}
		if method == "PATCH" {
			rp := needRepo()
			if rp == nil {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			parsed, ok := parseJSONBody(r)
			if !ok {
				sendErr(w, 400, "Invalid JSON", requestID)
				return
			}
			if v, ok := parsed["description"].(string); ok {
				rp.Description = v
			}
			if v, ok := parsed["private"].(bool); ok {
				rp.Private = v
			}
			sendJSON(w, 200, repoJSON(rp), requestID, nil)
			return
		}
		if method == "DELETE" {
			rp := needRepo()
			if rp == nil {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			store.deleteRepo(owner, repoName)
			w.Header().Set("X-GitHub-Request-Id", requestID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(204)
			return
		}
		sendErr(w, 404, "Not Found", requestID)
		return
	}

	rp := needRepo()
	if rp == nil {
		sendErr(w, 404, "Not Found", requestID)
		return
	}

	// git refs
	if tail[0] == "git" && len(tail) >= 2 && tail[1] == "refs" {
		if method == "POST" && len(tail) == 2 {
			parsed, ok := parseJSONBody(r)
			if !ok {
				sendErr(w, 400, "Invalid JSON", requestID)
				return
			}
			ref := jstr(parsed, "ref", "")
			sha := jstr(parsed, "sha", "")
			if !strings.HasPrefix(ref, "refs/") || sha == "" || store.getObject(sha) == nil {
				sendErr(w, 422, "Invalid ref or sha", requestID)
				return
			}
			if _, exists := rp.Refs[ref]; exists {
				sendErr(w, 422, "Reference already exists", requestID)
				return
			}
			store.setRef(rp, ref, sha, false)
			sendJSON(w, 201, marshalJSON(map[string]interface{}{
				"ref":    ref,
				"object": map[string]interface{}{"sha": sha},
				"url":    "/api/v3/repos/" + owner + "/" + repoName + "/git/refs/" + ref[5:],
			}), requestID, nil)
			return
		}
		if method == "GET" && len(tail) >= 3 {
			var parts []string
			for _, s := range tail[2:] {
				parts = append(parts, decodeSeg(s))
			}
			ref := "refs/" + strings.Join(parts, "/")
			oid, ok := rp.Refs[ref]
			if !ok {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			sendJSON(w, 200, marshalJSON(map[string]interface{}{"ref": ref, "object": map[string]interface{}{"sha": oid}}), requestID, nil)
			return
		}
		sendErr(w, 404, "Not Found", requestID)
		return
	}

	// branches
	if tail[0] == "branches" {
		if method == "GET" && len(tail) == 1 {
			var names []string
			for k := range rp.Refs {
				if strings.HasPrefix(k, "refs/heads/") {
					names = append(names, k)
				}
			}
			sort.Strings(names)
			var list []map[string]interface{}
			for _, ref := range names {
				name := ref[len("refs/heads/"):]
				list = append(list, map[string]interface{}{"name": name, "commit": map[string]interface{}{"sha": rp.Refs[ref]}, "protected": false})
			}
			if list == nil {
				list = []map[string]interface{}{}
			}
			sendJSON(w, 200, marshalJSON(list), requestID, nil)
			return
		}
		if method == "GET" && len(tail) >= 2 {
			var parts []string
			for _, s := range tail[1:] {
				parts = append(parts, decodeSeg(s))
			}
			branch := strings.Join(parts, "/")
			oid, ok := rp.Refs["refs/heads/"+branch]
			if !ok {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			sendJSON(w, 200, marshalJSON(map[string]interface{}{"name": branch, "commit": map[string]interface{}{"sha": oid}, "protected": false}), requestID, nil)
			return
		}
		sendErr(w, 404, "Not Found", requestID)
		return
	}

	// commits
	if tail[0] == "commits" {
		if method == "GET" && len(tail) == 1 {
			shaParam, hasSha := queryParam(query, "sha")
			tip, ok := resolveTip(store, rp, shaParam)
			if !ok {
				if hasSha && shaParam != "" {
					sendErr(w, 404, "No commit found for SHA", requestID)
					return
				}
				sendJSON(w, 200, []byte("[]"), requestID, nil)
				return
			}
			list := store.listCommits(tip, 100)
			var out []map[string]interface{}
			for _, item := range list {
				author := parseIdent(item.Commit.Author)
				committer := parseIdent(item.Commit.Committer)
				var parents []map[string]interface{}
				for _, p := range item.Commit.Parents {
					parents = append(parents, map[string]interface{}{"sha": p})
				}
				if parents == nil {
					parents = []map[string]interface{}{}
				}
				out = append(out, map[string]interface{}{
					"sha": item.Sha,
					"commit": map[string]interface{}{
						"message":   stripTrailingNewline(item.Commit.Message),
						"author":    map[string]interface{}{"name": author.name, "email": author.email, "date": author.date},
						"committer": map[string]interface{}{"name": committer.name, "email": committer.email, "date": committer.date},
						"tree":      map[string]interface{}{"sha": item.Commit.Tree},
					},
					"author":    map[string]interface{}{"login": ViewerLogin},
					"committer": map[string]interface{}{"login": ViewerLogin},
					"parents":   parents,
				})
			}
			if out == nil {
				out = []map[string]interface{}{}
			}
			sendJSON(w, 200, marshalJSON(out), requestID, nil)
			return
		}
		sendErr(w, 404, "Not Found", requestID)
		return
	}

	// contents
	if tail[0] == "contents" {
		var parts []string
		for _, s := range tail[1:] {
			parts = append(parts, decodeSeg(s))
		}
		filePath := strings.Join(parts, "/")
		if method == "GET" {
			refParam, _ := queryParam(query, "ref")
			tip, ok := resolveTip(store, rp, refParam)
			if !ok {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			commitObj := store.getObject(tip)
			if commitObj == nil || commitObj.Commit == nil {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			flat := store.flattenTree(commitObj.Commit.Tree)
			if filePath == "" {
				children := map[string]struct {
					typ string
					oid string
				}{}
				for p, entry := range flat {
					slash := strings.Index(p, "/")
					first := p
					if slash >= 0 {
						first = p[:slash]
					}
					if _, ok := children[first]; !ok {
						typ := "file"
						oid := entry.OID
						if slash >= 0 {
							typ = "dir"
							oid = ""
						}
						children[first] = struct {
							typ string
							oid string
						}{typ, oid}
					}
				}
				var names []string
				for n := range children {
					names = append(names, n)
				}
				sort.Strings(names)
				var out []map[string]interface{}
				for _, name := range names {
					v := children[name]
					out = append(out, map[string]interface{}{"type": v.typ, "name": name, "path": name, "sha": v.oid})
				}
				if out == nil {
					out = []map[string]interface{}{}
				}
				sendJSON(w, 200, marshalJSON(out), requestID, nil)
				return
			}
			if entry, ok := flat[filePath]; ok {
				blob := store.getObject(entry.OID)
				var data []byte
				if blob != nil {
					data = blob.Data
				}
				name := filePath
				if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
					name = filePath[idx+1:]
				}
				sendJSON(w, 200, marshalJSON(map[string]interface{}{
					"type":     "file",
					"encoding": "base64",
					"size":     len(data),
					"name":     name,
					"path":     filePath,
					"content":  base64.StdEncoding.EncodeToString(data),
					"sha":      entry.OID,
				}), requestID, nil)
				return
			}
			prefix := filePath + "/"
			children := map[string]struct {
				typ string
				oid string
			}{}
			for p, e := range flat {
				if !strings.HasPrefix(p, prefix) {
					continue
				}
				rest := p[len(prefix):]
				slash := strings.Index(rest, "/")
				first := rest
				if slash >= 0 {
					first = rest[:slash]
				}
				if _, ok := children[first]; !ok {
					typ := "file"
					oid := e.OID
					if slash >= 0 {
						typ = "dir"
						oid = ""
					}
					children[first] = struct {
						typ string
						oid string
					}{typ, oid}
				}
			}
			if len(children) == 0 {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			var names []string
			for n := range children {
				names = append(names, n)
			}
			sort.Strings(names)
			var out []map[string]interface{}
			for _, name := range names {
				v := children[name]
				out = append(out, map[string]interface{}{"type": v.typ, "name": name, "path": filePath + "/" + name, "sha": v.oid})
			}
			sendJSON(w, 200, marshalJSON(out), requestID, nil)
			return
		}
		if method == "PUT" {
			parsed, ok := parseJSONBody(r)
			if !ok {
				sendErr(w, 400, "Invalid JSON", requestID)
				return
			}
			message := jstr(parsed, "message", "Update file")
			contentB64 := jstr(parsed, "content", "")
			branch := jstr(parsed, "branch", rp.DefaultBranch)
			givenSha, hasSha := parsed["sha"].(string)
			content, _ := base64.StdEncoding.DecodeString(contentB64)
			tip, hasTip := rp.Refs["refs/heads/"+branch]
			if !hasTip && branch != rp.DefaultBranch {
				tip, hasTip = store.defaultTip(rp)
			}
			flat := map[string]FlatEntry{}
			var parents []string
			if hasTip {
				if commitObj := store.getObject(tip); commitObj != nil && commitObj.Commit != nil {
					parents = []string{tip}
					for p, e := range store.flattenTree(commitObj.Commit.Tree) {
						flat[p] = FlatEntry{OID: e.OID, Mode: e.Mode}
					}
				}
			}
			existing, hasExisting := flat[filePath]
			if hasSha && hasExisting && existing.OID != givenSha {
				sendErr(w, 409, "sha does not match", requestID)
				return
			}
			if hasSha && !hasExisting {
				sendErr(w, 422, "sha does not match any existing file", requestID)
				return
			}
			isCreate := !hasExisting
			blobOID := store.putObject(ObjBlob, content)
			flat[filePath] = FlatEntry{OID: blobOID, Mode: "100644"}
			treeOID := store.buildTreeFromFlat(flat)
			commitOID := store.createCommit(treeOID, parents, message)
			store.setRef(rp, "refs/heads/"+branch, commitOID, false)
			name := filePath
			if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
				name = filePath[idx+1:]
			}
			status := 200
			if isCreate {
				status = 201
			}
			sendJSON(w, status, marshalJSON(map[string]interface{}{
				"content": map[string]interface{}{
					"name":     name,
					"path":     filePath,
					"sha":      blobOID,
					"size":     len(content),
					"encoding": "base64",
					"content":  contentB64,
					"type":     "file",
				},
				"commit": map[string]interface{}{"sha": commitOID, "message": message},
			}), requestID, nil)
			return
		}
		if method == "DELETE" {
			parsed, ok := parseJSONBody(r)
			if !ok {
				sendErr(w, 400, "Invalid JSON", requestID)
				return
			}
			message := jstr(parsed, "message", "Delete file")
			givenSha, hasSha := parsed["sha"].(string)
			branch := jstr(parsed, "branch", rp.DefaultBranch)
			tip, hasTip := rp.Refs["refs/heads/"+branch]
			if !hasTip {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			commitObj := store.getObject(tip)
			if commitObj == nil || commitObj.Commit == nil {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			flat := map[string]FlatEntry{}
			for p, e := range store.flattenTree(commitObj.Commit.Tree) {
				flat[p] = FlatEntry{OID: e.OID, Mode: e.Mode}
			}
			existing, hasExisting := flat[filePath]
			if !hasExisting {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			if hasSha && existing.OID != givenSha {
				sendErr(w, 409, "sha does not match", requestID)
				return
			}
			delete(flat, filePath)
			treeOID := store.buildTreeFromFlat(flat)
			commitOID := store.createCommit(treeOID, []string{tip}, message)
			store.setRef(rp, "refs/heads/"+branch, commitOID, false)
			sendJSON(w, 200, marshalJSON(map[string]interface{}{"commit": map[string]interface{}{"sha": commitOID, "message": message}}), requestID, nil)
			return
		}
		sendErr(w, 404, "Not Found", requestID)
		return
	}

	// labels
	if tail[0] == "labels" {
		if method == "POST" && len(tail) == 1 {
			parsed, ok := parseJSONBody(r)
			if !ok {
				sendErr(w, 400, "Invalid JSON", requestID)
				return
			}
			name := jstr(parsed, "name", "")
			color := jstr(parsed, "color", "000000")
			if name == "" {
				sendErr(w, 422, "name is required", requestID)
				return
			}
			rp.Labels[name] = &Label{Name: name, Color: color}
			sendJSON(w, 201, marshalJSON(map[string]interface{}{"name": name, "color": color}), requestID, nil)
			return
		}
		sendErr(w, 404, "Not Found", requestID)
		return
	}

	// issues
	if tail[0] == "issues" {
		if len(tail) == 1 {
			if method == "GET" {
				state, _ := queryParam(query, "state")
				if state == "" {
					state = "open"
				}
				sortParam, _ := queryParam(query, "sort")
				if sortParam == "" {
					sortParam = "created"
				}
				direction, _ := queryParam(query, "direction")
				if direction == "" {
					direction = "desc"
				}
				perPage := perPageOf(query)
				page := pageOf(query)
				var list []*Issue
				for _, i := range rp.Issues {
					list = append(list, i)
				}
				if state != "all" {
					var filtered []*Issue
					for _, i := range list {
						if i.State == state {
							filtered = append(filtered, i)
						}
					}
					list = filtered
				}
				if sortParam == "created" {
					sort.Slice(list, func(a, b int) bool {
						if direction == "asc" {
							return list[a].Seq < list[b].Seq
						}
						return list[a].Seq > list[b].Seq
					})
				}
				items, _, _ := paginate(list, perPage, page)
				var out [][]byte
				for _, i := range items {
					out = append(out, issueJSON(i, rp))
				}
				sendJSON(w, 200, joinJSONArr(out), requestID, nil)
				return
			}
			if method == "POST" {
				parsed, ok := parseJSONBody(r)
				if !ok {
					sendErr(w, 400, "Invalid JSON", requestID)
					return
				}
				title := jstr(parsed, "title", "")
				issueBody := jstr(parsed, "body", "")
				var labels []string
				if raw, ok := parsed["labels"].([]interface{}); ok {
					for _, l := range raw {
						if s, ok := l.(string); ok {
							labels = append(labels, s)
						} else if m, ok := l.(map[string]interface{}); ok {
							if s, ok := m["name"].(string); ok {
								labels = append(labels, s)
							}
						}
					}
				}
				made, errStr := store.createIssue(rp, title, issueBody, labels)
				if errStr != "" {
					sendErr(w, 422, errStr, requestID)
					return
				}
				sendJSON(w, 201, issueJSON(made, rp), requestID, nil)
				return
			}
			sendErr(w, 404, "Not Found", requestID)
			return
		}
		num, err := strconv.Atoi(tail[1])
		if err != nil || num < 1 {
			sendErr(w, 404, "Not Found", requestID)
			return
		}
		issue := rp.Issues[num]
		if len(tail) == 2 {
			if method == "GET" {
				if issue == nil {
					sendErr(w, 404, "Not Found", requestID)
					return
				}
				sendJSON(w, 200, issueJSON(issue, rp), requestID, nil)
				return
			}
			if method == "PATCH" {
				if issue == nil {
					sendErr(w, 404, "Not Found", requestID)
					return
				}
				parsed, ok := parseJSONBody(r)
				if !ok {
					sendErr(w, 400, "Invalid JSON", requestID)
					return
				}
				if st, ok := parsed["state"].(string); ok && (st == "open" || st == "closed") {
					issue.State = st
				}
				if v, ok := parsed["title"].(string); ok {
					issue.Title = v
				}
				if v, ok := parsed["body"].(string); ok {
					issue.Body = v
				}
				issue.UpdatedAt = isoNow()
				issue.JSONBuf = nil
				sendJSON(w, 200, issueJSON(issue, rp), requestID, nil)
				return
			}
			sendErr(w, 404, "Not Found", requestID)
			return
		}
		if len(tail) == 3 && tail[2] == "comments" {
			if issue == nil {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			if method == "GET" {
				var out []map[string]interface{}
				for _, c := range issue.Comments {
					out = append(out, map[string]interface{}{
						"id": c.ID, "body": c.Body, "user": map[string]interface{}{"login": c.User},
						"created_at": c.CreatedAt, "updated_at": c.CreatedAt,
					})
				}
				if out == nil {
					out = []map[string]interface{}{}
				}
				sendJSON(w, 200, marshalJSON(out), requestID, nil)
				return
			}
			if method == "POST" {
				parsed, ok := parseJSONBody(r)
				if !ok {
					sendErr(w, 400, "Invalid JSON", requestID)
					return
				}
				commentBody := jstr(parsed, "body", "")
				comment := &Comment{
					ID: store.NextItem, Body: commentBody, User: ViewerLogin,
					CreatedAt: isoNow(), Seq: store.NextSeq,
				}
				store.NextItem++
				store.NextSeq++
				issue.Comments = append(issue.Comments, comment)
				issue.JSONBuf = nil
				sendJSON(w, 201, marshalJSON(map[string]interface{}{
					"id": comment.ID, "body": comment.Body, "user": map[string]interface{}{"login": comment.User},
					"created_at": comment.CreatedAt, "updated_at": comment.CreatedAt,
				}), requestID, nil)
				return
			}
			sendErr(w, 404, "Not Found", requestID)
			return
		}
		sendErr(w, 404, "Not Found", requestID)
		return
	}

	// pulls
	if tail[0] == "pulls" {
		if len(tail) == 1 {
			if method == "GET" {
				state, _ := queryParam(query, "state")
				if state == "" {
					state = "open"
				}
				perPage := perPageOf(query)
				page := pageOf(query)
				var list []*Pull
				for _, p := range rp.Pulls {
					list = append(list, p)
				}
				if state != "all" {
					var filtered []*Pull
					for _, p := range list {
						if p.State == state && !p.Merged {
							filtered = append(filtered, p)
						}
					}
					list = filtered
				}
				sort.Slice(list, func(a, b int) bool { return list[a].Number < list[b].Number })
				items, _, _ := paginate(list, perPage, page)
				var out [][]byte
				for _, p := range items {
					out = append(out, prJSON(rp, p))
				}
				sendJSON(w, 200, joinJSONArr(out), requestID, nil)
				return
			}
			if method == "POST" {
				parsed, ok := parseJSONBody(r)
				if !ok {
					sendErr(w, 400, "Invalid JSON", requestID)
					return
				}
				title := jstr(parsed, "title", "")
				prBody := jstr(parsed, "body", "")
				head := jstr(parsed, "head", "")
				base := jstr(parsed, "base", rp.DefaultBranch)
				made, errStr := store.createPull(rp, title, prBody, head, base)
				if errStr != "" {
					sendErr(w, 422, errStr, requestID)
					return
				}
				sendJSON(w, 201, prJSON(rp, made), requestID, nil)
				return
			}
			sendErr(w, 404, "Not Found", requestID)
			return
		}
		num, err := strconv.Atoi(tail[1])
		if err != nil || num < 1 {
			sendErr(w, 404, "Not Found", requestID)
			return
		}
		pr := rp.Pulls[num]
		if len(tail) == 2 {
			if method == "GET" {
				if pr == nil {
					sendErr(w, 404, "Not Found", requestID)
					return
				}
				sendJSON(w, 200, prJSON(rp, pr), requestID, nil)
				return
			}
			sendErr(w, 404, "Not Found", requestID)
			return
		}
		if len(tail) == 3 && tail[2] == "files" && method == "GET" {
			if pr == nil {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			files := store.pullFiles(rp, pr)
			var out []map[string]interface{}
			for _, f := range files {
				additions, deletions := 0, 0
				if f.NewOID != "" {
					if obj := store.getObject(f.NewOID); obj != nil {
						additions = countLines(obj.Data)
					}
				}
				if f.OldOID != "" {
					if obj := store.getObject(f.OldOID); obj != nil {
						deletions = countLines(obj.Data)
					}
				}
				if f.Status == "removed" {
					additions = 0
				}
				if f.Status == "added" {
					deletions = 0
				}
				if f.Status == "modified" && additions == 0 && deletions == 0 {
					additions = 1
				}
				out = append(out, map[string]interface{}{
					"filename": f.Path, "status": f.Status,
					"additions": additions, "deletions": deletions, "changes": additions + deletions,
				})
			}
			if out == nil {
				out = []map[string]interface{}{}
			}
			sendJSON(w, 200, marshalJSON(out), requestID, nil)
			return
		}
		if len(tail) == 3 && tail[2] == "merge" && method == "PUT" {
			if pr == nil {
				sendErr(w, 404, "Not Found", requestID)
				return
			}
			_, _ = readBody(r)
			ok, sha, reason := store.mergePull(rp, pr)
			if !ok {
				sendErr(w, 405, reason, requestID)
				return
			}
			sendJSON(w, 200, marshalJSON(map[string]interface{}{
				"merged": true, "sha": sha, "message": "Merge pull request #" + itoa(pr.Number) + " from " + pr.HeadRef,
			}), requestID, nil)
			return
		}
		sendErr(w, 404, "Not Found", requestID)
		return
	}

	sendErr(w, 404, "Not Found", requestID)
}
