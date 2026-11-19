package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

var reqCounter int64

func prepRest(w http.ResponseWriter) {
	id := atomic.AddInt64(&reqCounter, 1)
	w.Header().Set("X-GitHub-Request-Id", fmt.Sprintf("req-%d", id))
	w.Header().Set("Content-Type", "application/json")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	prepRest(w)
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"message": msg})
}

func checkAuth(r *http.Request) bool {
	if apiToken == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+apiToken
}

func readBody(r *http.Request) (map[string]any, bool, int) {
	// returns (object, hadBody, statusIfBad)
	if r.ContentLength == 0 {
		return map[string]any{}, false, 0
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil || len(data) == 0 {
		return map[string]any{}, false, 0
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, true, 400
	}
	if m, ok := v.(map[string]any); ok {
		return m, true, 0
	}
	return map[string]any{}, true, 0
}

func strField(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

func boolField(m map[string]any, k string, def bool) bool {
	if b, ok := m[k].(bool); ok {
		return b
	}
	return def
}

func pageParams(r *http.Request) (page, perPage int) {
	page, perPage = 1, 30
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	return page, perPage
}

func paginate[T any](items []T, page, perPage int) []T {
	start := (page - 1) * perPage
	if start >= len(items) {
		return []T{}
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

func linkNext(w http.ResponseWriter, r *http.Request, page, perPage int, more bool) {
	if !more {
		return
	}
	q := r.URL.Query()
	q.Set("page", strconv.Itoa(page+1))
	q.Set("per_page", strconv.Itoa(perPage))
	u := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: q.Encode()}
	w.Header().Set("Link", "<"+u.String()+">; rel=\"next\"")
}

func getRepo(w http.ResponseWriter, r *http.Request, owner, name string) *Repo {
	store.mu.RLock()
	defer store.mu.RUnlock()
	repo := store.lookup(owner, name)
	if repo == nil {
		errJSON(w, 404, "Not Found")
		return nil
	}
	return repo
}

// ---------------------------------------------------------------------------
// Entry point: path segments after /api/v3
// ---------------------------------------------------------------------------

func handleRest(w http.ResponseWriter, r *http.Request, segs []string) {
	if !checkAuth(r) {
		errJSON(w, 401, "Requires authentication")
		return
	}
	if len(segs) == 0 {
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		writeJSON(w, 200, map[string]any{
			"current_user_url": "/api/v3/user",
			"repository_url":   "/api/v3/repos/{owner}/{repo}",
			"user_url":         "/api/v3/users/{username}",
		})
		return
	}
	switch segs[0] {
	case "meta":
		if r.Method != http.MethodGet {
			errJSON(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": true})
		return
	case "user":
		handleUser(w, r, segs[1:])
		return
	case "users":
		if len(segs) == 2 && r.Method == http.MethodGet {
			writeJSON(w, 200, userView(segs[1], viewerID))
			return
		}
		errJSON(w, 404, "Not Found")
		return
	case "repos":
		handleRepos(w, r, segs[1:])
		return
	}
	errJSON(w, 404, "Not Found")
}

func handleUser(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) == 0 {
		if r.Method == http.MethodGet {
			writeJSON(w, 200, userView(viewerLogin, viewerID))
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	if len(segs) == 1 && segs[0] == "repos" {
		if r.Method == http.MethodGet {
			page, perPage := pageParams(r)
			store.mu.RLock()
			var all []*Repo
			for _, repo := range store.repos {
				if repo.Owner == viewerLogin {
					all = append(all, repo)
				}
			}
			sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
			total := len(all)
			items := paginate(all, page, perPage)
			out := make([]any, 0, len(items))
			for _, repo := range items {
				out = append(out, repoView(repo))
			}
			store.mu.RUnlock()
			linkNext(w, r, page, perPage, page*perPage < total)
			writeJSON(w, 200, out)
			return
		}
		if r.Method == http.MethodPost {
			body, _, bad := readBody(r)
			if bad != 0 {
				errJSON(w, 400, "Problems parsing JSON")
				return
			}
			name := strField(body, "name")
			if name == "" {
				errJSON(w, 422, "name is required")
				return
			}
			store.mu.Lock()
			repo, ok := store.createRepo(name, boolField(body, "private", false), strField(body, "description"))
			store.mu.Unlock()
			if !ok {
				errJSON(w, 422, "Repository already exists")
				return
			}
			writeJSON(w, 201, repoView(repo))
			return
		}
	}
	errJSON(w, 404, "Not Found")
}

func handleRepos(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) < 2 {
		errJSON(w, 404, "Not Found")
		return
	}
	owner, name := segs[0], segs[1]
	rest := segs[2:]
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			if repo := getRepo(w, r, owner, name); repo != nil {
				store.mu.RLock()
				v := repoView(repo)
				store.mu.RUnlock()
				writeJSON(w, 200, v)
			}
		case http.MethodPatch:
			body, _, bad := readBody(r)
			if bad != 0 {
				errJSON(w, 400, "Problems parsing JSON")
				return
			}
			store.mu.Lock()
			repo := store.lookup(owner, name)
			if repo == nil {
				store.mu.Unlock()
				errJSON(w, 404, "Not Found")
				return
			}
			if d, ok := body["description"]; ok {
				if s, ok := d.(string); ok {
					repo.Description = s
				}
			}
			if p, ok := body["private"]; ok {
				if b, ok := p.(bool); ok {
					repo.Private = b
				}
			}
			repo.Updated = stampFor(store.tick())
			v := repoView(repo)
			store.mu.Unlock()
			writeJSON(w, 200, v)
		case http.MethodDelete:
			store.mu.Lock()
			repo := store.lookup(owner, name)
			if repo == nil {
				store.mu.Unlock()
				errJSON(w, 404, "Not Found")
				return
			}
			delete(store.repos, repoKey(owner, name))
			store.mu.Unlock()
			prepRest(w)
			w.WriteHeader(204)
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	switch rest[0] {
	case "branches":
		if r.Method == http.MethodGet {
			repo := getRepo(w, r, owner, name)
			if repo == nil {
				return
			}
			store.mu.RLock()
			out := []any{}
			for _, ref := range sortedRefs(repo) {
				if strings.HasPrefix(ref, "refs/heads/") {
					out = append(out, map[string]any{
						"name": strings.TrimPrefix(ref, "refs/heads/"),
						"commit": map[string]any{"sha": repo.Refs[ref]},
						"protected": false,
					})
				}
			}
			store.mu.RUnlock()
			writeJSON(w, 200, out)
			return
		}
	case "commits":
		handleCommits(w, r, owner, name, rest[1:])
		return
	case "contents":
		handleContents(w, r, owner, name, rest[1:])
		return
	case "labels":
		handleLabels(w, r, owner, name)
		return
	case "issues":
		handleIssues(w, r, owner, name, rest[1:])
		return
	case "pulls":
		handlePulls(w, r, owner, name, rest[1:])
		return
	case "git":
		if len(rest) >= 2 && rest[1] == "refs" {
			handleGitRefs(w, r, owner, name, rest[2:])
			return
		}
	}
	errJSON(w, 404, "Not Found")
}

// ---------------------------------------------------------------------------
// Commits / branches
// ---------------------------------------------------------------------------

func commitView(r *Repo, sha string) map[string]any {
	o := r.Objects[sha]
	if o == nil || o.Typ != "commit" {
		return nil
	}
	c := parseCommit(o.Data)
	parents := make([]any, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, map[string]any{"sha": p})
	}
	return map[string]any{
		"sha": sha,
		"commit": map[string]any{
			"message": c.Message,
			"author": map[string]any{"name": viewerLogin, "email": viewerLogin + "@example.invalid", "date": "2025-01-01T00:00:00Z"},
			"committer": map[string]any{"name": viewerLogin, "email": viewerLogin + "@example.invalid", "date": "2025-01-01T00:00:00Z"},
		},
		"author": userView(viewerLogin, viewerID),
		"committer": userView(viewerLogin, viewerID),
		"parents": parents,
		"url": "/api/v3/repos/" + r.Owner + "/" + r.Name + "/commits/" + sha,
	}
}

func walkCommits(r *Repo, tip string) []string {
	var out []string
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		o := r.Objects[cur]
		if o == nil || o.Typ != "commit" {
			continue
		}
		out = append(out, cur)
		parents := parseCommit(o.Data).Parents
		for i := len(parents) - 1; i >= 0; i-- {
			stack = append(stack, parents[i])
		}
	}
	return out
}

func handleCommits(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	repo := getRepo(w, r, owner, name)
	if repo == nil {
		return
	}
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			errJSON(w, 404, "Not Found")
			return
		}
		branch := r.URL.Query().Get("sha")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		store.mu.RLock()
		tip := repo.tip(branch)
		if tip == "" {
			// maybe a raw sha
			if _, ok := repo.Objects[branch]; ok {
				tip = branch
			}
		}
		var out []any
		for _, sha := range walkCommits(repo, tip) {
			if v := commitView(repo, sha); v != nil {
				out = append(out, v)
			}
		}
		store.mu.RUnlock()
		if out == nil {
			out = []any{}
		}
		writeJSON(w, 200, out)
		return
	}
	if len(rest) == 1 && r.Method == http.MethodGet {
		store.mu.RLock()
		v := commitView(repo, rest[0])
		store.mu.RUnlock()
		if v == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, v)
		return
	}
	errJSON(w, 404, "Not Found")
}

func handleGitRefs(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	if r.Method == http.MethodPost && len(rest) == 0 {
		body, _, bad := readBody(r)
		if bad != 0 {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		ref := strField(body, "ref")
		sha := strField(body, "sha")
		if ref == "" || sha == "" {
			errJSON(w, 422, "ref and sha are required")
			return
		}
		store.mu.Lock()
		repo := store.lookup(owner, name)
		if repo == nil {
			store.mu.Unlock()
			errJSON(w, 404, "Not Found")
			return
		}
		if _, exists := repo.Refs[ref]; exists {
			store.mu.Unlock()
			errJSON(w, 422, "Reference already exists")
			return
		}
		if _, ok := repo.Objects[sha]; !ok {
			store.mu.Unlock()
			errJSON(w, 422, "Object does not exist")
			return
		}
		repo.Refs[ref] = sha
		store.tick()
		v := map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}}
		store.mu.Unlock()
		writeJSON(w, 201, v)
		return
	}
	if r.Method == http.MethodGet && len(rest) > 0 {
		ref := "refs/" + strings.Join(rest, "/")
		repo := getRepo(w, r, owner, name)
		if repo == nil {
			return
		}
		store.mu.RLock()
		sha, ok := repo.Refs[ref]
		store.mu.RUnlock()
		if !ok {
			errJSON(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}})
		return
	}
	errJSON(w, 404, "Not Found")
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

func b64decode(s string) ([]byte, error) {
	clean := strings.Map(func(c rune) rune {
		if c == '\n' || c == '\r' || c == ' ' {
			return -1
		}
		return c
	}, s)
	return base64.StdEncoding.DecodeString(clean)
}

func handleContents(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	path := strings.Join(rest, "/")
	refName := r.URL.Query().Get("ref")
	switch r.Method {
	case http.MethodGet:
		repo := getRepo(w, r, owner, name)
		if repo == nil {
			return
		}
		store.mu.RLock()
		defer store.mu.RUnlock()
		branch := refName
		if branch == "" {
			branch = repo.DefaultBranch
		}
		tip := repo.tip(branch)
		if tip == "" {
			if _, ok := repo.Objects[branch]; ok {
				tip = branch
			}
		}
		if tip == "" {
			errJSON(w, 404, "Not Found")
			return
		}
		flat := map[string]TreeEntry{}
		flattenTree(repo, parseCommit(repo.Objects[tip].Data).Tree, "", flat)
		if path == "" {
			writeJSON(w, 200, dirEntries(repo, tip, ""))
			return
		}
		if e, ok := flat[path]; ok {
			blob := repo.Objects[e.OID]
			writeJSON(w, 200, map[string]any{
				"type": "file", "encoding": "base64",
				"content": base64.StdEncoding.EncodeToString(blob.Data),
				"sha": e.OID, "size": len(blob.Data),
				"name": path[strings.LastIndex(path, "/")+1:], "path": path,
			})
			return
		}
		// Directory?
		prefix := path + "/"
		found := false
		for p := range flat {
			if strings.HasPrefix(p, prefix) {
				found = true
				break
			}
		}
		if found {
			writeJSON(w, 200, dirEntries(repo, tip, path))
			return
		}
		errJSON(w, 404, "Not Found")
	case http.MethodPut:
		body, _, bad := readBody(r)
		if bad != 0 {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		contentB64 := strField(body, "content")
		raw, err := b64decode(contentB64)
		if err != nil {
			errJSON(w, 422, "content is not valid base64")
			return
		}
		branch := strField(body, "branch")
		sha := strField(body, "sha")
		msg := strField(body, "message")
		if msg == "" {
			msg = "Update " + path
		}
		store.mu.Lock()
		repo := store.lookup(owner, name)
		if repo == nil {
			store.mu.Unlock()
			errJSON(w, 404, "Not Found")
			return
		}
		if branch == "" {
			branch = repo.DefaultBranch
		}
		tip := repo.tip(branch)
		if branch != repo.DefaultBranch && tip == "" {
			if _, ok := repo.Refs["refs/heads/"+branch]; !ok {
				store.mu.Unlock()
				errJSON(w, 404, "Branch not found")
				return
			}
		}
		flat := map[string]TreeEntry{}
		if tip != "" {
			flattenTree(repo, parseCommit(repo.Objects[tip].Data).Tree, "", flat)
		}
		existing, exists := flat[path]
		if exists {
			if sha == "" || sha != existing.OID {
				store.mu.Unlock()
				errJSON(w, 409, "sha does not match")
				return
			}
		} else if sha != "" {
			store.mu.Unlock()
			errJSON(w, 409, "sha does not match")
			return
		}
		content := string(raw)
		newTip := commitFiles(repo, tip, msg, map[string]*string{path: &content})
		repo.Refs["refs/heads/"+branch] = newTip
		blobOID := gitHash("blob", raw)
		store.tick()
		status := 201
		if exists {
			status = 200
		}
		v := map[string]any{
			"content": map[string]any{"name": path[strings.LastIndex(path, "/")+1:], "path": path, "sha": blobOID, "size": len(raw), "encoding": "base64", "type": "file"},
			"commit": map[string]any{"sha": newTip, "message": msg},
		}
		store.mu.Unlock()
		writeJSON(w, status, v)
	case http.MethodDelete:
		body, _, bad := readBody(r)
		if bad != 0 {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		sha := strField(body, "sha")
		msg := strField(body, "message")
		if msg == "" {
			msg = "Delete " + path
		}
		store.mu.Lock()
		repo := store.lookup(owner, name)
		if repo == nil {
			store.mu.Unlock()
			errJSON(w, 404, "Not Found")
			return
		}
		tip := repo.tip(repo.DefaultBranch)
		flat := map[string]TreeEntry{}
		if tip != "" {
			flattenTree(repo, parseCommit(repo.Objects[tip].Data).Tree, "", flat)
		}
		existing, exists := flat[path]
		if !exists {
			store.mu.Unlock()
			errJSON(w, 404, "Not Found")
			return
		}
		if sha != "" && sha != existing.OID {
			store.mu.Unlock()
			errJSON(w, 409, "sha does not match")
			return
		}
		newTip := commitFiles(repo, tip, msg, map[string]*string{path: nil})
		repo.Refs["refs/heads/"+repo.DefaultBranch] = newTip
		store.tick()
		v := map[string]any{"commit": map[string]any{"sha": newTip, "message": msg}}
		store.mu.Unlock()
		writeJSON(w, 200, v)
	default:
		errJSON(w, 404, "Not Found")
	}
}

// dirEntries lists the immediate children of dir ("" for the root).
func dirEntries(r *Repo, tip, dir string) []any {
	flat := map[string]TreeEntry{}
	flattenTree(r, parseCommit(r.Objects[tip].Data).Tree, "", flat)
	prefix := dir
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]TreeEntry{}
	isDir := map[string]bool{}
	for p, e := range flat {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 {
			isDir[parts[0]] = true
			continue
		}
		seen[parts[0]] = e
	}
	// Tree oids for directories: rebuild from the tree object.
	out := []any{}
	treeOID := parseCommit(r.Objects[tip].Data).Tree
	treeAt := func(dirPath string) string {
		cur := treeOID
		if dirPath == "" {
			return cur
		}
		for _, seg := range strings.Split(dirPath, "/") {
			found := ""
			for _, e := range parseTree(r.Objects[cur].Data) {
				if e.Name == seg && e.Mode == "40000" {
					found = e.OID
					break
				}
			}
			if found == "" {
				return ""
			}
			cur = found
		}
		return cur
	}
	parentTree := treeAt(dir)
	_ = parentTree
	names := []string{}
	for n := range seen {
		names = append(names, n)
	}
	for n := range isDir {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if e, ok := seen[n]; ok {
			out = append(out, map[string]any{"type": "file", "name": n, "path": prefix + n, "sha": e.OID, "size": len(r.Objects[e.OID].Data)})
		} else {
			// find the subtree oid
			oid := ""
			for _, e := range parseTree(r.Objects[treeAt(dir)].Data) {
				if e.Name == n {
					oid = e.OID
				}
			}
			out = append(out, map[string]any{"type": "dir", "name": n, "path": prefix + n, "sha": oid})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

func handleLabels(w http.ResponseWriter, r *http.Request, owner, name string) {
	switch r.Method {
	case http.MethodGet:
		repo := getRepo(w, r, owner, name)
		if repo == nil {
			return
		}
		store.mu.RLock()
		out := []any{}
		names := []string{}
		for n := range repo.Labels {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			out = append(out, labelView(repo.Labels[n]))
		}
		store.mu.RUnlock()
		writeJSON(w, 200, out)
	case http.MethodPost:
		body, _, bad := readBody(r)
		if bad != 0 {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		lname := strField(body, "name")
		if lname == "" {
			errJSON(w, 422, "name is required")
			return
		}
		color := strField(body, "color")
		if color == "" {
			color = "ededed"
		}
		store.mu.Lock()
		repo := store.lookup(owner, name)
		if repo == nil {
			store.mu.Unlock()
			errJSON(w, 404, "Not Found")
			return
		}
		if _, ok := repo.Labels[lname]; !ok {
			repo.Labels[lname] = &Label{Name: lname, Color: color}
			store.tick()
		}
		v := labelView(repo.Labels[lname])
		store.mu.Unlock()
		writeJSON(w, 201, v)
	default:
		errJSON(w, 404, "Not Found")
	}
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func handleIssues(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			repo := getRepo(w, r, owner, name)
			if repo == nil {
				return
			}
			q := r.URL.Query()
			state := q.Get("state")
			if state == "" {
				state = "open"
			}
			asc := q.Get("direction") == "asc"
			page, perPage := pageParams(r)
			store.mu.RLock()
			var all []*Issue
			for _, it := range repo.sortedIssues(asc) {
				if state == "all" || it.State == state {
					all = append(all, it)
				}
			}
			total := len(all)
			items := paginate(all, page, perPage)
			out := make([]any, 0, len(items))
			for _, it := range items {
				out = append(out, issueView(repo, it))
			}
			store.mu.RUnlock()
			linkNext(w, r, page, perPage, page*perPage < total)
			writeJSON(w, 200, out)
		case http.MethodPost:
			body, _, bad := readBody(r)
			if bad != 0 {
				errJSON(w, 400, "Problems parsing JSON")
				return
			}
			title := strField(body, "title")
			bodyText := strField(body, "body")
			var labels []string
			if raw, ok := body["labels"]; ok {
				if arr, ok := raw.([]any); ok {
					for _, v := range arr {
						if s, ok := v.(string); ok {
							labels = append(labels, s)
						}
					}
				}
			}
			store.mu.Lock()
			repo := store.lookup(owner, name)
			if repo == nil {
				store.mu.Unlock()
				errJSON(w, 404, "Not Found")
				return
			}
			for _, l := range labels {
				if _, ok := repo.Labels[l]; !ok {
					store.mu.Unlock()
					errJSON(w, 422, "Label does not exist")
					return
				}
			}
			it := store.newIssue(repo, title, bodyText, labels)
			v := issueView(repo, it)
			store.mu.Unlock()
			writeJSON(w, 201, v)
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	// /issues/{n} or /issues/{n}/comments
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodGet:
			repo := getRepo(w, r, owner, name)
			if repo == nil {
				return
			}
			store.mu.RLock()
			it := repo.findIssue(n)
			var v any
			if it != nil {
				v = issueView(repo, it)
			}
			store.mu.RUnlock()
			if v == nil {
				errJSON(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, v)
		case http.MethodPatch:
			body, _, bad := readBody(r)
			if bad != 0 {
				errJSON(w, 400, "Problems parsing JSON")
				return
			}
			store.mu.Lock()
			repo := store.lookup(owner, name)
			if repo == nil {
				store.mu.Unlock()
				errJSON(w, 404, "Not Found")
				return
			}
			it := repo.findIssue(n)
			if it == nil {
				store.mu.Unlock()
				errJSON(w, 404, "Not Found")
				return
			}
			if t, ok := body["title"].(string); ok {
				it.Title = t
			}
			if b, ok := body["body"].(string); ok {
				it.Body = b
			}
			if s, ok := body["state"].(string); ok && (s == "open" || s == "closed") {
				it.State = s
				if s == "closed" {
					it.ClosedAt = stampFor(store.tick())
				} else {
					it.ClosedAt = ""
				}
			}
			it.Updated = stampFor(store.tick())
			v := issueView(repo, it)
			store.mu.Unlock()
			writeJSON(w, 200, v)
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case http.MethodGet:
			repo := getRepo(w, r, owner, name)
			if repo == nil {
				return
			}
			store.mu.RLock()
			it := repo.findIssue(n)
			out := []any{}
			if it != nil {
				for _, c := range repo.Comments[n] {
					out = append(out, commentView(repo, c))
				}
			}
			store.mu.RUnlock()
			if it == nil {
				errJSON(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, out)
		case http.MethodPost:
			body, _, bad := readBody(r)
			if bad != 0 {
				errJSON(w, 400, "Problems parsing JSON")
				return
			}
			text := strField(body, "body")
			store.mu.Lock()
			repo := store.lookup(owner, name)
			if repo == nil {
				store.mu.Unlock()
				errJSON(w, 404, "Not Found")
				return
			}
			it := repo.findIssue(n)
			if it == nil {
				store.mu.Unlock()
				errJSON(w, 404, "Not Found")
				return
			}
			c := &Comment{ID: repo.NextCommentID, IssueNo: n, Body: text, Created: stampFor(store.tick())}
			repo.NextCommentID++
			repo.Comments[n] = append(repo.Comments[n], c)
			v := commentView(repo, c)
			store.mu.Unlock()
			writeJSON(w, 201, v)
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	errJSON(w, 404, "Not Found")
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

func diffFiles(r *Repo, baseTree, headTree string) []map[string]any {
	baseFlat := map[string]TreeEntry{}
	headFlat := map[string]TreeEntry{}
	if baseTree != "" {
		flattenTree(r, baseTree, "", baseFlat)
	}
	if headTree != "" {
		flattenTree(r, headTree, "", headFlat)
	}
	countLines := func(oid string) int {
		o := r.Objects[oid]
		if o == nil {
			return 0
		}
		n := 0
		for _, b := range o.Data {
			if b == '\n' {
				n++
			}
		}
		if len(o.Data) > 0 && o.Data[len(o.Data)-1] != '\n' {
			n++
		}
		return n
	}
	paths := map[string]bool{}
	for p := range baseFlat {
		paths[p] = true
	}
	for p := range headFlat {
		paths[p] = true
	}
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	out := []map[string]any{}
	for _, p := range names {
		b, inBase := baseFlat[p]
		h, inHead := headFlat[p]
		switch {
		case inHead && !inBase:
			lines := countLines(h.OID)
			out = append(out, map[string]any{"filename": p, "status": "added", "additions": lines, "deletions": 0, "changes": lines})
		case inBase && !inHead:
			lines := countLines(b.OID)
			out = append(out, map[string]any{"filename": p, "status": "removed", "additions": 0, "deletions": lines, "changes": lines})
		case b.OID != h.OID:
			add := countLines(h.OID)
			del := countLines(b.OID)
			out = append(out, map[string]any{"filename": p, "status": "modified", "additions": add, "deletions": del, "changes": add + del})
		}
	}
	return out
}

func treeOf(r *Repo, commitOID string) string {
	if o := r.Objects[commitOID]; o != nil && o.Typ == "commit" {
		return parseCommit(o.Data).Tree
	}
	return ""
}

func handlePulls(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			repo := getRepo(w, r, owner, name)
			if repo == nil {
				return
			}
			q := r.URL.Query()
			state := q.Get("state")
			if state == "" {
				state = "open"
			}
			page, perPage := pageParams(r)
			store.mu.RLock()
			var all []*Pull
			for _, p := range repo.sortedPulls(false) {
				if state == "all" || p.State == state {
					all = append(all, p)
				}
			}
			total := len(all)
			items := paginate(all, page, perPage)
			out := make([]any, 0, len(items))
			for _, p := range items {
				out = append(out, pullView(repo, p))
			}
			store.mu.RUnlock()
			linkNext(w, r, page, perPage, page*perPage < total)
			writeJSON(w, 200, out)
		case http.MethodPost:
			body, _, bad := readBody(r)
			if bad != 0 {
				errJSON(w, 400, "Problems parsing JSON")
				return
			}
			head := strField(body, "head")
			base := strField(body, "base")
			title := strField(body, "title")
			bodyText := strField(body, "body")
			store.mu.Lock()
			repo := store.lookup(owner, name)
			if repo == nil {
				store.mu.Unlock()
				errJSON(w, 404, "Not Found")
				return
			}
			headSha := repo.tip(head)
			baseSha := repo.tip(base)
			if headSha == "" || baseSha == "" {
				store.mu.Unlock()
				errJSON(w, 422, "head or base branch not found")
				return
			}
			p := store.newPull(repo, title, bodyText, head, base, headSha, baseSha)
			v := pullView(repo, p)
			store.mu.Unlock()
			writeJSON(w, 201, v)
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		if r.Method != http.MethodGet {
			errJSON(w, 404, "Not Found")
			return
		}
		repo := getRepo(w, r, owner, name)
		if repo == nil {
			return
		}
		store.mu.RLock()
		p := repo.findPull(n)
		var v any
		if p != nil {
			v = pullView(repo, p)
		}
		store.mu.RUnlock()
		if v == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, v)
		return
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == http.MethodGet {
		repo := getRepo(w, r, owner, name)
		if repo == nil {
			return
		}
		store.mu.RLock()
		p := repo.findPull(n)
		var out []any
		if p != nil {
			for _, f := range diffFiles(repo, treeOf(repo, p.BaseSha), treeOf(repo, p.HeadSha)) {
				out = append(out, f)
			}
		}
		store.mu.RUnlock()
		if p == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		if out == nil {
			out = []any{}
		}
		writeJSON(w, 200, out)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && r.Method == http.MethodPut {
		body, _, bad := readBody(r)
		if bad != 0 {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		_ = body
		store.mu.Lock()
		repo := store.lookup(owner, name)
		if repo == nil {
			store.mu.Unlock()
			errJSON(w, 404, "Not Found")
			return
		}
		p := repo.findPull(n)
		if p == nil {
			store.mu.Unlock()
			errJSON(w, 404, "Not Found")
			return
		}
		if p.State != "open" {
			store.mu.Unlock()
			errJSON(w, 405, "Pull request is already merged or closed")
			return
		}
		mergeSha, ok := squashMerge(repo, p)
		if !ok {
			store.mu.Unlock()
			errJSON(w, 405, "Merge conflict")
			return
		}
		v := map[string]any{"sha": mergeSha, "merged": true, "message": "Pull Request successfully merged"}
		store.mu.Unlock()
		writeJSON(w, 200, v)
		return
	}
	errJSON(w, 404, "Not Found")
}
