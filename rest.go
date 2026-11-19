// The GitHub REST v3 surface: repositories, branches, commits, contents,
// labels, issues, comments and pull requests (including squash merges with
// three-way conflict detection). Every reply is JSON and carries
// X-GitHub-Request-Id; errors carry {"message": ...}.
package main

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const forgeWho = "Forge <forge@localhost>"

// ---------------------------------------------------------------- shapes

func repoJSON(s *Store, r *Repo) map[string]any {
	return map[string]any{
		"id":               r.ID,
		"node_id":          globalID("repo", r.ID),
		"name":             r.Name,
		"full_name":        r.Owner + "/" + r.Name,
		"owner":            map[string]any{"login": r.Owner, "id": 1000, "type": "User"},
		"private":          r.Private,
		"description":      r.Desc,
		"default_branch":   r.DefaultBranch,
		"html_url":         "https://example.invalid/" + r.Owner + "/" + r.Name,
		"url":              "/api/v3/repos/" + r.Owner + "/" + r.Name,
		"created_at":       stamp(r.Seq),
		"updated_at":       stamp(r.Seq),
		"pushed_at":        stamp(r.Seq),
		"has_issues":       true,
		"has_projects":     true,
		"has_wiki":         true,
		"has_pages":        false,
		"forks_count":      0,
		"open_issues_count": len(r.Issues),
		"watchers_count":   0,
		"size":             0,
		"language":         nil,
		"topics":           []string{},
		"visibility":       map[bool]string{true: "private", false: "public"}[r.Private],
	}
}

func labelJSON(l *Label) map[string]any {
	return map[string]any{
		"name":        l.Name,
		"color":       l.Color,
		"description": l.Desc,
		"default":     false,
	}
}

func issueJSON(s *Store, r *Repo, is *Issue) map[string]any {
	labels := make([]any, 0, len(is.Labels))
	for _, name := range is.Labels {
		if l := r.Labels[name]; l != nil {
			labels = append(labels, labelJSON(l))
		} else {
			labels = append(labels, map[string]any{"name": name, "color": "ededed"})
		}
	}
	var closedAt any
	if is.ClosedAt != "" {
		closedAt = is.ClosedAt
	}
	return map[string]any{
		"id":         is.ID,
		"node_id":    globalID("issue", is.ID),
		"number":     is.Number,
		"title":      is.Title,
		"body":       is.Body,
		"state":      is.State,
		"labels":     labels,
		"user":       map[string]any{"login": s.viewer, "id": 1000, "type": "User"},
		"assignee":   nil,
		"assignees":  []any{},
		"milestone":  nil,
		"comments":   len(is.Comments),
		"created_at": stamp(is.Seq),
		"updated_at": stamp(is.Seq),
		"closed_at":  closedAt,
		"html_url":   "https://example.invalid/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(is.Number),
		"url":        "/api/v3/repos/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(is.Number),
	}
}

func commentJSON(s *Store, r *Repo, is *Issue, c *Comment) map[string]any {
	return map[string]any{
		"id":         c.ID,
		"node_id":    globalID("comment", c.ID),
		"body":       c.Body,
		"user":       map[string]any{"login": s.viewer, "id": 1000, "type": "User"},
		"created_at": stamp(c.Seq),
		"updated_at": stamp(c.Seq),
		"html_url":   "https://example.invalid/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(is.Number) + "#issuecomment-" + strconv.Itoa(c.ID),
		"url":        "/api/v3/repos/" + r.Owner + "/" + r.Name + "/issues/comments/" + strconv.Itoa(c.ID),
	}
}

func pullJSON(s *server, r *Repo, p *Pull) map[string]any {
	var mergedAt, mergeSHA any
	if p.MergedAt != "" {
		mergedAt = p.MergedAt
	}
	if p.MergeSHA != "" {
		mergeSHA = p.MergeSHA
	}
	mergeable := s.pullMergeable(r, p)
	return map[string]any{
		"id":               p.ID,
		"node_id":          globalID("pull", p.ID),
		"number":           p.Number,
		"state":            p.State,
		"title":            p.Title,
		"body":             p.Body,
		"head":             map[string]any{"ref": p.Head, "sha": p.HeadSHA, "label": r.Owner + ":" + p.Head, "user": map[string]any{"login": r.Owner}},
		"base":             map[string]any{"ref": p.Base, "sha": p.BaseSHA, "label": r.Owner + ":" + p.Base, "user": map[string]any{"login": r.Owner}},
		"merged":           p.Merged,
		"mergeable":        mergeable,
		"merge_commit_sha": mergeSHA,
		"merged_at":        mergedAt,
		"created_at":       stamp(p.Seq),
		"updated_at":       stamp(p.Seq),
		"closed_at":        mergedAt,
		"draft":            false,
		"html_url":         "https://example.invalid/" + r.Owner + "/" + r.Name + "/pull/" + strconv.Itoa(p.Number),
		"url":              "/api/v3/repos/" + r.Owner + "/" + r.Name + "/pulls/" + strconv.Itoa(p.Number),
	}
}

// pullMergeable reports whether the pull request can merge cleanly.
func (s *server) pullMergeable(r *Repo, p *Pull) bool {
	if p.State != "open" {
		return false
	}
	return !s.pullConflicts(r, p)
}

// pullConflicts reports whether a squash merge of p would hit a conflict:
// the same path changed differently on both sides of the merge-base.
func (s *server) pullConflicts(r *Repo, p *Pull) bool {
	lookup := func(id string) *Obj { return r.Objects[id] }
	headTip, baseTip := r.tipOf(p.Head), r.tipOf(p.Base)
	if headTip == "" || baseTip == "" {
		return true
	}
	base := mergeBase(headTip, baseTip, lookup)
	baseTree := ""
	if base != "" {
		if obj := lookup(base); obj != nil && obj.Type == "commit" {
			baseTree = parseCommit(obj.Data).Tree
		}
	}
	flatBase := map[string]TreeEntry{}
	if baseTree != "" {
		flattenTree(baseTree, lookup, flatBase, "")
	}
	flatHead := map[string]TreeEntry{}
	if obj := lookup(headTip); obj != nil && obj.Type == "commit" {
		flattenTree(parseCommit(obj.Data).Tree, lookup, flatHead, "")
	}
	flatTip := map[string]TreeEntry{}
	if obj := lookup(baseTip); obj != nil && obj.Type == "commit" {
		flattenTree(parseCommit(obj.Data).Tree, lookup, flatTip, "")
	}
	changed := map[string]bool{}
	for path, he := range flatHead {
		if be, ok := flatBase[path]; !ok || be.OID != he.OID {
			changed[path] = true
		}
	}
	for path := range flatBase {
		if _, ok := flatHead[path]; !ok {
			changed[path] = true
		}
	}
	for path := range changed {
		baseEnt, inBase := flatBase[path]
		tipEnt, inTip := flatTip[path]
		headEnt := flatHead[path]
		tipChanged := false
		if !inBase && inTip {
			tipChanged = true
		} else if inBase && !inTip {
			tipChanged = true
		} else if inBase && inTip && baseEnt.OID != tipEnt.OID {
			tipChanged = true
		}
		if !tipChanged {
			continue
		}
		// Both sides changed the path: conflict unless they agree.
		if _, inHead := flatHead[path]; !inHead {
			return true
		}
		if !inTip {
			return true
		}
		if headEnt.OID != tipEnt.OID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- pagination

// pageParams reads per_page/page query values.
func pageParams(r *http.Request) (perPage, page int) {
	perPage = 30
	page = 1
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
			if perPage > 100 {
				perPage = 100
			}
		}
	}
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	return perPage, page
}

// paginate slices items and writes a Link rel="next" header when more pages
// remain. It returns the page's items.
func paginate(w http.ResponseWriter, r *http.Request, total int) (start, end int) {
	perPage, page := pageParams(r)
	start = (page - 1) * perPage
	if start > total {
		start = total
	}
	end = start + perPage
	if end > total {
		end = total
	}
	if end < total {
		next := *r.URL
		q := next.Query()
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page+1))
		next.RawQuery = q.Encode()
		abs := "http://" + r.Host + next.String()
		w.Header().Set("Link", "<"+abs+">; rel=\"next\"")
	}
	return start, end
}

// ---------------------------------------------------------------- routing

func (s *server) serveRest(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v3")
	trimmed = strings.Trim(trimmed, "/")
	var segs []string
	if trimmed != "" {
		// Split on "/" but keep %-decoding per segment.
		raw := strings.Split(trimmed, "/")
		for _, p := range raw {
			dec, err := url.PathUnescape(p)
			if err != nil {
				dec = p
			}
			segs = append(segs, dec)
		}
	}
	method := r.Method

	// API root and meta.
	if len(segs) == 0 {
		if method == "GET" {
			jsonOut(w, 200, map[string]any{
				"current_user_url": "/api/v3/user",
				"rate_limit_url":   "/api/v3/rate_limit",
				"repository_url":   "/api/v3/repos/{owner}/{repo}",
			}, map[string]string{"X-OAuth-Scopes": "repo, read:org"})
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	if len(segs) == 1 && segs[0] == "meta" && method == "GET" {
		jsonOut(w, 200, map[string]any{"installed_version": "1.0.0"}, nil)
		return
	}
	if len(segs) == 1 && segs[0] == "user" && method == "GET" {
		jsonOut(w, 200, map[string]any{
			"login": s.store.viewer, "id": 1000, "node_id": globalID("user", 1000),
			"type": "User", "name": s.store.viewer,
		}, nil)
		return
	}
	if len(segs) == 2 && segs[0] == "user" && segs[1] == "repos" {
		if method == "GET" {
			s.listUserRepos(w, r)
			return
		}
		if method == "POST" {
			s.createUserRepo(w, r)
			return
		}
	}

	// /repos/{owner}/{name}/...
	if len(segs) >= 3 && segs[0] == "repos" {
		s.serveRepoRest(w, r, segs)
		return
	}
	errJSON(w, 404, "Not Found")
}

func (s *server) listUserRepos(w http.ResponseWriter, r *http.Request) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	total := len(s.store.order)
	start, end := paginate(w, r, total)
	items := make([]any, 0, end-start)
	for _, repo := range s.store.order[start:end] {
		items = append(items, repoJSON(s.store, repo))
	}
	jsonOut(w, 200, items, nil)
}

func (s *server) createUserRepo(w http.ResponseWriter, r *http.Request) {
	body, ok := bodyMap(r)
	if !ok {
		errJSON(w, 400, "Invalid JSON body")
		return
	}
	name := strVal(body, "name")
	if name == "" {
		errJSON(w, 422, "Repository name is required")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	rep, created := s.store.createRepo(name, boolVal(body, "private", false), strVal(body, "description"))
	if !created {
		errJSON(w, 422, "Repository "+name+" already exists")
		return
	}
	jsonOut(w, 201, repoJSON(s.store, rep), nil)
}

// serveRepoRest handles /repos/{owner}/{name} and everything below it.
func (s *server) serveRepoRest(w http.ResponseWriter, r *http.Request, segs []string) {
	owner, name := segs[0+1], segs[1+1]
	rest := segs[3:]
	method := r.Method

	// The repository belongs to the token's login; any other owner is 404.
	if owner != s.store.viewer {
		errJSON(w, 404, "Not Found")
		return
	}
	s.store.mu.RLock()
	rep := s.store.lookup(owner, name)
	s.store.mu.RUnlock()
	if rep == nil {
		errJSON(w, 404, "Not Found")
		return
	}

	// /repos/{o}/{r} itself.
	if len(rest) == 0 {
		switch method {
		case "GET":
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			jsonOut(w, 200, repoJSON(s.store, rep), nil)
		case "PATCH":
			body, ok := bodyMap(r)
			if !ok {
				errJSON(w, 400, "Invalid JSON body")
				return
			}
			s.store.mu.Lock()
			defer s.store.mu.Unlock()
			if d, present := body["description"]; present {
				if ds, isStr := d.(string); isStr {
					rep.Desc = ds
				}
			}
			if p, present := body["private"]; present {
				if pb, isBool := p.(bool); isBool {
					rep.Private = pb
				}
			}
			jsonOut(w, 200, repoJSON(s.store, rep), nil)
		case "DELETE":
			s.store.mu.Lock()
			defer s.store.mu.Unlock()
			s.store.deleteRepo(owner, name)
			w.WriteHeader(204)
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}

	switch rest[0] {
	case "branches":
		if method == "GET" && len(rest) == 1 {
			s.listBranches(w, r, rep)
			return
		}
		if method == "GET" && len(rest) == 2 {
			s.getBranch(w, r, rep, rest[1])
			return
		}
	case "commits":
		if method == "GET" && len(rest) == 1 {
			s.listCommits(w, r, rep)
			return
		}
	case "git":
		if len(rest) >= 2 && rest[1] == "refs" {
			s.serveGitRefs(w, r, rep, rest[2:])
			return
		}
	case "contents":
		if len(rest) >= 2 {
			path := strings.Join(rest[1:], "/")
			s.serveContents(w, r, rep, path)
			return
		}
		if len(rest) == 1 {
			// GET /contents on the repository root lists its top level.
			if method == "GET" {
				s.getContents(w, r, rep, "")
				return
			}
		}
	case "labels":
		if method == "POST" && len(rest) == 1 {
			s.createLabel(w, r, rep)
			return
		}
		if method == "GET" && len(rest) == 1 {
			s.listLabels(w, r, rep)
			return
		}
	case "issues":
		s.serveIssues(w, r, rep, rest[1:])
		return
	case "pulls":
		s.servePulls(w, r, rep, rest[1:])
		return
	}
	errJSON(w, 404, "Not Found")
}

func (s *server) listBranches(w http.ResponseWriter, r *http.Request, rep *Repo) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	items := make([]any, 0)
	for _, b := range rep.branchNames() {
		items = append(items, map[string]any{
			"name":      b,
			"commit":    map[string]any{"sha": rep.tipOf(b)},
			"protected": false,
		})
	}
	jsonOut(w, 200, items, nil)
}

func (s *server) getBranch(w http.ResponseWriter, r *http.Request, rep *Repo, branch string) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	sha := rep.tipOf(branch)
	if sha == "" {
		errJSON(w, 404, "Not Found")
		return
	}
	jsonOut(w, 200, map[string]any{
		"name":      branch,
		"commit":    map[string]any{"sha": sha},
		"protected": false,
	}, nil)
}

func (s *server) listCommits(w http.ResponseWriter, r *http.Request, rep *Repo) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	ref := r.URL.Query().Get("sha")
	var tip string
	if ref == "" {
		tip = rep.tipOf(rep.DefaultBranch)
	} else if strings.HasPrefix(ref, "refs/") {
		tip = rep.Refs[ref]
	} else if rep.Refs["refs/heads/"+ref] != "" {
		tip = rep.Refs["refs/heads/"+ref]
	} else {
		tip = ref // maybe a raw sha
	}
	items := make([]any, 0)
	if tip != "" {
		lookup := func(id string) *Obj { return rep.Objects[id] }
		seen := map[string]bool{}
		queue := []string{tip}
		for len(queue) > 0 && len(items) < 100 {
			oid := queue[0]
			queue = queue[1:]
			if seen[oid] || oid == zeroOID {
				continue
			}
			seen[oid] = true
			obj := lookup(oid)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			cv := parseCommit(obj.Data)
			author := parseIdent(cv.Author)
			items = append(items, map[string]any{
				"sha": oid,
				"commit": map[string]any{
					"message": cv.Message,
					"author":  map[string]any{"name": author[0], "email": author[1]},
					"tree":    map[string]any{"sha": cv.Tree},
				},
				"parents": parentsJSON(cv.Parents),
			})
			queue = append(queue, cv.Parents...)
		}
	}
	jsonOut(w, 200, items, nil)
}

func parentsJSON(parents []string) []any {
	out := make([]any, 0, len(parents))
	for _, p := range parents {
		out = append(out, map[string]any{"sha": p})
	}
	return out
}

// parseIdent splits "Name <email> ts tz" into name and email.
func parseIdent(s string) [2]string {
	lt := strings.IndexByte(s, '<')
	gt := strings.IndexByte(s, '>')
	if lt < 0 || gt < 0 || gt < lt {
		return [2]string{strings.TrimSpace(s), ""}
	}
	return [2]string{strings.TrimSpace(s[:lt]), s[lt+1 : gt]}
}

// ---------------------------------------------------------------- git refs

func (s *server) serveGitRefs(w http.ResponseWriter, r *http.Request, rep *Repo, rest []string) {
	method := r.Method
	if method == "POST" && len(rest) == 0 {
		body, ok := bodyMap(r)
		if !ok {
			errJSON(w, 400, "Invalid JSON body")
			return
		}
		ref := strVal(body, "ref")
		sha := strVal(body, "sha")
		if ref == "" || sha == "" {
			errJSON(w, 422, "ref and sha are required")
			return
		}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		if rep.Objects[sha] == nil {
			errJSON(w, 422, "object does not exist")
			return
		}
		if _, exists := rep.Refs[ref]; exists {
			errJSON(w, 422, "ref already exists")
			return
		}
		rep.Refs[ref] = sha
		jsonOut(w, 201, map[string]any{
			"ref":    ref,
			"object": map[string]any{"sha": sha, "type": rep.Objects[sha].Type},
		}, nil)
		return
	}
	if method == "GET" && len(rest) == 0 {
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
		items := make([]any, 0)
		for _, name := range rep.refNames() {
			items = append(items, map[string]any{
				"ref":    name,
				"object": map[string]any{"sha": rep.Refs[name]},
			})
		}
		jsonOut(w, 200, items, nil)
		return
	}
	if method == "GET" && len(rest) >= 1 {
		ref := "refs/" + strings.Join(rest, "/")
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
		sha, ok := rep.Refs[ref]
		if !ok {
			errJSON(w, 404, "Not Found")
			return
		}
		jsonOut(w, 200, map[string]any{
			"ref":    ref,
			"object": map[string]any{"sha": sha},
		}, nil)
		return
	}
	errJSON(w, 404, "Not Found")
}

// ---------------------------------------------------------------- labels

func (s *server) createLabel(w http.ResponseWriter, r *http.Request, rep *Repo) {
	body, ok := bodyMap(r)
	if !ok {
		errJSON(w, 400, "Invalid JSON body")
		return
	}
	name := strVal(body, "name")
	if name == "" {
		errJSON(w, 422, "Label name is required")
		return
	}
	color := strVal(body, "color")
	if color == "" {
		color = "ededed"
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if existing := rep.Labels[name]; existing != nil {
		jsonOut(w, 201, labelJSON(existing), nil)
		return
	}
	l := &Label{Name: name, Color: color, Desc: strVal(body, "description")}
	rep.Labels[name] = l
	jsonOut(w, 201, labelJSON(l), nil)
}

func (s *server) listLabels(w http.ResponseWriter, r *http.Request, rep *Repo) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	names := make([]string, 0, len(rep.Labels))
	for n := range rep.Labels {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]any, 0, len(names))
	for _, n := range names {
		items = append(items, labelJSON(rep.Labels[n]))
	}
	jsonOut(w, 200, items, nil)
}

// ---------------------------------------------------------------- issues

func (s *server) serveIssues(w http.ResponseWriter, r *http.Request, rep *Repo, rest []string) {
	method := r.Method
	if len(rest) == 0 {
		if method == "POST" {
			s.createIssue(w, r, rep)
			return
		}
		if method == "GET" {
			s.listIssues(w, r, rep)
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		if method == "GET" {
			s.getIssue(w, r, rep, num)
			return
		}
		if method == "PATCH" {
			s.patchIssue(w, r, rep, num)
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		if method == "POST" {
			s.createComment(w, r, rep, num)
			return
		}
		if method == "GET" {
			s.listComments(w, r, rep, num)
			return
		}
	}
	errJSON(w, 404, "Not Found")
}

func (s *server) createIssue(w http.ResponseWriter, r *http.Request, rep *Repo) {
	body, ok := bodyMap(r)
	if !ok {
		errJSON(w, 400, "Invalid JSON body")
		return
	}
	title := strVal(body, "title")
	if title == "" {
		errJSON(w, 422, "title is required")
		return
	}
	var labelNames []string
	if raw, present := body["labels"]; present {
		if arr, isArr := raw.([]any); isArr {
			for _, item := range arr {
				switch v := item.(type) {
				case string:
					labelNames = append(labelNames, v)
				case map[string]any:
					if n := strVal(v, "name"); n != "" {
						labelNames = append(labelNames, n)
					}
				}
			}
		}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, name := range labelNames {
		if rep.Labels[name] == nil {
			errJSON(w, 422, "label "+name+" does not exist")
			return
		}
	}
	is := &Issue{
		ID:     s.store.nextID,
		Number: rep.NextIssue,
		Title:  title,
		Body:   strVal(body, "body"),
		State:  "open",
		Labels: labelNames,
		Seq:    s.store.tick(),
	}
	s.store.nextID++
	rep.NextIssue++
	rep.Issues = append(rep.Issues, is)
	jsonOut(w, 201, issueJSON(s.store, rep, is), nil)
}

func (s *server) listIssues(w http.ResponseWriter, r *http.Request, rep *Repo) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	sortParam := r.URL.Query().Get("sort")
	direction := r.URL.Query().Get("direction")
	if direction == "" {
		direction = "desc"
	}
	var filtered []*Issue
	for _, is := range rep.Issues {
		if state == "all" || is.State == state {
			filtered = append(filtered, is)
		}
	}
	if sortParam == "created" {
		sort.SliceStable(filtered, func(i, j int) bool {
			if direction == "asc" {
				return filtered[i].Seq < filtered[j].Seq
			}
			return filtered[i].Seq > filtered[j].Seq
		})
	} else if direction == "asc" {
		sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Seq < filtered[j].Seq })
	} else {
		sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Seq > filtered[j].Seq })
	}
	start, end := paginate(w, r, len(filtered))
	items := make([]any, 0, end-start)
	for _, is := range filtered[start:end] {
		items = append(items, issueJSON(s.store, rep, is))
	}
	jsonOut(w, 200, items, nil)
}

func (s *server) getIssue(w http.ResponseWriter, r *http.Request, rep *Repo, num int) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	is := rep.findIssue(num)
	if is == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	jsonOut(w, 200, issueJSON(s.store, rep, is), nil)
}

func (s *server) patchIssue(w http.ResponseWriter, r *http.Request, rep *Repo, num int) {
	body, ok := bodyMap(r)
	if !ok {
		errJSON(w, 400, "Invalid JSON body")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	is := rep.findIssue(num)
	if is == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if v, present := body["state"]; present {
		if st, isStr := v.(string); isStr && (st == "open" || st == "closed") {
			is.State = st
			if st == "closed" {
				is.ClosedAt = stamp(s.store.tick())
			} else {
				is.ClosedAt = ""
			}
		}
	}
	if v, present := body["title"]; present {
		if t, isStr := v.(string); isStr {
			is.Title = t
		}
	}
	if v, present := body["body"]; present {
		if b, isStr := v.(string); isStr {
			is.Body = b
		}
	}
	jsonOut(w, 200, issueJSON(s.store, rep, is), nil)
}

func (s *server) createComment(w http.ResponseWriter, r *http.Request, rep *Repo, num int) {
	body, ok := bodyMap(r)
	if !ok {
		errJSON(w, 400, "Invalid JSON body")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	is := rep.findIssue(num)
	if is == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	c := &Comment{ID: s.store.nextID, Body: strVal(body, "body"), Seq: s.store.tick()}
	s.store.nextID++
	is.Comments = append(is.Comments, c)
	jsonOut(w, 201, commentJSON(s.store, rep, is, c), nil)
}

func (s *server) listComments(w http.ResponseWriter, r *http.Request, rep *Repo, num int) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	is := rep.findIssue(num)
	if is == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	items := make([]any, 0, len(is.Comments))
	for _, c := range is.Comments {
		items = append(items, commentJSON(s.store, rep, is, c))
	}
	jsonOut(w, 200, items, nil)
}

// ---------------------------------------------------------------- pulls

func (s *server) servePulls(w http.ResponseWriter, r *http.Request, rep *Repo, rest []string) {
	method := r.Method
	if len(rest) == 0 {
		if method == "POST" {
			s.createPull(w, r, rep)
			return
		}
		if method == "GET" {
			s.listPulls(w, r, rep)
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		if method == "GET" {
			s.getPull(w, r, rep, num)
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "files" && method == "GET" {
		s.listPullFiles(w, r, rep, num)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && method == "PUT" {
		s.mergePull(w, r, rep, num)
		return
	}
	errJSON(w, 404, "Not Found")
}

func (s *server) createPull(w http.ResponseWriter, r *http.Request, rep *Repo) {
	body, ok := bodyMap(r)
	if !ok {
		errJSON(w, 400, "Invalid JSON body")
		return
	}
	head := strVal(body, "head")
	base := strVal(body, "base")
	if head == "" || base == "" {
		errJSON(w, 422, "head and base are required")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	headSHA := rep.tipOf(head)
	baseSHA := rep.tipOf(base)
	if headSHA == "" || baseSHA == "" {
		errJSON(w, 422, "head or base branch does not exist")
		return
	}
	if existing := rep.openPullFor(head, base); existing != nil {
		errJSON(w, 422, "a pull request already exists for this branch")
		return
	}
	p := &Pull{
		ID:      s.store.nextID,
		Number:  rep.NextPull,
		Title:   strVal(body, "title"),
		Body:    strVal(body, "body"),
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
	jsonOut(w, 201, pullJSON(s, rep, p), nil)
}

func (s *server) listPulls(w http.ResponseWriter, r *http.Request, rep *Repo) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	var filtered []*Pull
	for _, p := range rep.Pulls {
		if state == "all" || p.State == state {
			filtered = append(filtered, p)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Seq < filtered[j].Seq })
	start, end := paginate(w, r, len(filtered))
	items := make([]any, 0, end-start)
	for _, p := range filtered[start:end] {
		items = append(items, pullJSON(s, rep, p))
	}
	jsonOut(w, 200, items, nil)
}

func (s *server) getPull(w http.ResponseWriter, r *http.Request, rep *Repo, num int) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	p := rep.findPull(num)
	if p == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	jsonOut(w, 200, pullJSON(s, rep, p), nil)
}

// pullFiles computes the merge-base vs head file diff for a pull request.
func (s *server) pullFiles(rep *Repo, p *Pull) []map[string]any {
	lookup := func(id string) *Obj { return rep.Objects[id] }
	base := mergeBase(p.HeadSHA, p.BaseSHA, lookup)
	baseTree := ""
	if base != "" {
		if obj := lookup(base); obj != nil && obj.Type == "commit" {
			baseTree = parseCommit(obj.Data).Tree
		}
	}
	flatBase := map[string]TreeEntry{}
	if baseTree != "" {
		flattenTree(baseTree, lookup, flatBase, "")
	}
	flatHead := map[string]TreeEntry{}
	if obj := lookup(p.HeadSHA); obj != nil && obj.Type == "commit" {
		flattenTree(parseCommit(obj.Data).Tree, lookup, flatHead, "")
	}
	added, modified, removed := diffFlat(flatBase, flatHead)
	out := []map[string]any{}
	emit := func(path, status string, add, del int) {
		out = append(out, map[string]any{
			"filename":  path,
			"status":    status,
			"additions": add,
			"deletions": del,
			"changes":   add + del,
		})
	}
	for _, path := range added {
		n := 0
		if obj := lookup(flatHead[path].OID); obj != nil {
			n = countLines(obj.Data)
		}
		emit(path, "added", n, 0)
	}
	for _, path := range modified {
		add, del := 0, 0
		if obj := lookup(flatHead[path].OID); obj != nil {
			add = countLines(obj.Data)
		}
		if obj := lookup(flatBase[path].OID); obj != nil {
			del = countLines(obj.Data)
		}
		emit(path, "modified", add, del)
	}
	for _, path := range removed {
		n := 0
		if obj := lookup(flatBase[path].OID); obj != nil {
			n = countLines(obj.Data)
		}
		emit(path, "removed", 0, n)
	}
	return out
}

func (s *server) listPullFiles(w http.ResponseWriter, r *http.Request, rep *Repo, num int) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	p := rep.findPull(num)
	if p == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	files := s.pullFiles(rep, p)
	items := make([]any, 0, len(files))
	for _, f := range files {
		items = append(items, f)
	}
	jsonOut(w, 200, items, nil)
}

// squashMerge applies a squash merge of an open, conflict-free pull request
// onto its base branch. It reports the new commit oid and true on success.
// Callers hold the store write lock.
func (s *server) squashMerge(rep *Repo, p *Pull) (string, bool) {
	if p.State != "open" || s.pullConflicts(rep, p) {
		return "", false
	}
	lookup := func(id string) *Obj { return rep.Objects[id] }
	headTip, baseTip := rep.tipOf(p.Head), rep.tipOf(p.Base)
	base := mergeBase(headTip, baseTip, lookup)
	baseTree := ""
	if base != "" {
		if obj := lookup(base); obj != nil && obj.Type == "commit" {
			baseTree = parseCommit(obj.Data).Tree
		}
	}
	flatBase := map[string]TreeEntry{}
	if baseTree != "" {
		flattenTree(baseTree, lookup, flatBase, "")
	}
	flatHead := map[string]TreeEntry{}
	if obj := lookup(headTip); obj != nil && obj.Type == "commit" {
		flattenTree(parseCommit(obj.Data).Tree, lookup, flatHead, "")
	}
	flatTip := map[string]TreeEntry{}
	if obj := lookup(baseTip); obj != nil && obj.Type == "commit" {
		flattenTree(parseCommit(obj.Data).Tree, lookup, flatTip, "")
	}
	// Apply the head's changes (vs the merge-base) onto the base tip tree.
	merged := map[string]TreeEntry{}
	for path, e := range flatTip {
		merged[path] = e
	}
	for path, he := range flatHead {
		if be, inBase := flatBase[path]; !inBase || be.OID != he.OID {
			merged[path] = he
		}
	}
	for path := range flatBase {
		if _, inHead := flatHead[path]; !inHead {
			delete(merged, path)
		}
	}
	put := func(typ string, data []byte) string { return rep.putObject(typ, data) }
	treeOID := buildTreeFromFlat(merged, put)
	seq := s.store.tick()
	msg := "Merge pull request #" + strconv.Itoa(p.Number) + " from " + p.Head + "\n\n" + p.Title
	commitOID := rep.putObject("commit", buildCommit(treeOID, []string{baseTip}, msg, forgeWho, 1700000000+seq))
	rep.Refs["refs/heads/"+p.Base] = commitOID
	p.State = "closed"
	p.Merged = true
	p.MergeSHA = commitOID
	p.MergedAt = stamp(seq)
	return commitOID, true
}

// mergePull squash-merges an open pull request onto its base branch.
func (s *server) mergePull(w http.ResponseWriter, r *http.Request, rep *Repo, num int) {
	_, ok := bodyMap(r)
	if !ok {
		errJSON(w, 400, "Invalid JSON body")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	p := rep.findPull(num)
	if p == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if p.State != "open" {
		errJSON(w, 405, "pull request is not open")
		return
	}
	if s.pullConflicts(rep, p) {
		errJSON(w, 405, "merge conflict")
		return
	}
	commitOID, merged := s.squashMerge(rep, p)
	if !merged {
		errJSON(w, 405, "merge conflict")
		return
	}
	jsonOut(w, 200, map[string]any{
		"sha":     commitOID,
		"merged":  true,
		"message": "Pull Request successfully merged",
	}, nil)
}

// ---------------------------------------------------------------- contents

// serveContents routes contents PUT/GET/DELETE for one path.
func (s *server) serveContents(w http.ResponseWriter, r *http.Request, rep *Repo, path string) {
	switch r.Method {
	case "GET":
		s.getContents(w, r, rep, path)
	case "PUT":
		s.putContents(w, r, rep, path)
	case "DELETE":
		s.deleteContents(w, r, rep, path)
	default:
		errJSON(w, 404, "Not Found")
	}
}

// resolveRefTip returns the tip oid for a ref query (?ref=): a branch name,
// a full ref, or a raw oid. Empty ref means the default branch.
func (s *server) resolveRefTip(rep *Repo, ref string) string {
	if ref == "" {
		return rep.tipOf(rep.DefaultBranch)
	}
	if strings.HasPrefix(ref, "refs/") {
		return rep.Refs[ref]
	}
	if rep.Refs["refs/heads/"+ref] != "" {
		return rep.Refs["refs/heads/"+ref]
	}
	if rep.Objects[ref] != nil {
		return ref
	}
	return ""
}

func (s *server) getContents(w http.ResponseWriter, r *http.Request, rep *Repo, path string) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	lookup := func(id string) *Obj { return rep.Objects[id] }
	tip := s.resolveRefTip(rep, r.URL.Query().Get("ref"))
	if tip == "" {
		errJSON(w, 404, "Not Found")
		return
	}
	obj := lookup(tip)
	if obj == nil || obj.Type != "commit" {
		errJSON(w, 404, "Not Found")
		return
	}
	treeOID := parseCommit(obj.Data).Tree
	flat := map[string]TreeEntry{}
	flattenTree(treeOID, lookup, flat, "")
	if path == "" {
		// Root listing: immediate children only.
		items := s.dirListing(rep, flat, "")
		jsonOut(w, 200, items, nil)
		return
	}
	if ent, ok := flat[path]; ok {
		blob := lookup(ent.OID)
		if blob == nil {
			errJSON(w, 404, "Not Found")
			return
		}
		jsonOut(w, 200, map[string]any{
			"type":     "file",
			"encoding": "base64",
			"size":     len(blob.Data),
			"name":     baseName(path),
			"path":     path,
			"content":  base64.StdEncoding.EncodeToString(blob.Data),
			"sha":      ent.OID,
		}, nil)
		return
	}
	// A directory path lists its immediate children.
	prefix := path + "/"
	found := false
	for p := range flat {
		if strings.HasPrefix(p, prefix) {
			found = true
			break
		}
	}
	if !found {
		errJSON(w, 404, "Not Found")
		return
	}
	jsonOut(w, 200, s.dirListing(rep, flat, path), nil)
}

// dirListing lists the immediate children of dir ("" for the root).
func (s *server) dirListing(rep *Repo, flat map[string]TreeEntry, dir string) []any {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	seen := map[string]map[string]any{}
	for path, ent := range flat {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := path[len(prefix):]
		seg := rest
		isDir := false
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			seg = rest[:idx]
			isDir = true
		}
		childPath := prefix + seg
		if _, done := seen[seg]; done {
			continue
		}
		if isDir {
			seen[seg] = map[string]any{
				"type": "dir",
				"name": seg,
				"path": childPath,
				"sha":  "",
			}
		} else {
			seen[seg] = map[string]any{
				"type": "file",
				"name": seg,
				"path": childPath,
				"sha":  ent.OID,
				"size": len(rep.Objects[ent.OID].Data),
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]any, 0, len(names))
	for _, n := range names {
		// Directories need a sha; use the tree oid when available.
		items = append(items, seen[n])
	}
	return items
}

func baseName(path string) string {
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// putContents creates or updates a file, committing the change on a branch.
func (s *server) putContents(w http.ResponseWriter, r *http.Request, rep *Repo, path string) {
	body, ok := bodyMap(r)
	if !ok {
		errJSON(w, 400, "Invalid JSON body")
		return
	}
	contentB64 := strVal(body, "content")
	raw, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(contentB64))
	if err != nil {
		errJSON(w, 422, "content is not valid base64")
		return
	}
	branch := strVal(body, "branch")
	if branch == "" {
		branch = rep.DefaultBranch
	}
	message := strVal(body, "message")
	if message == "" {
		message = "Update " + path
	}
	sha := strVal(body, "sha")

	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	lookup := func(id string) *Obj { return rep.Objects[id] }
	tip := rep.tipOf(branch)
	flat := map[string]TreeEntry{}
	var parents []string
	if tip != "" {
		if obj := lookup(tip); obj != nil && obj.Type == "commit" {
			flattenTree(parseCommit(obj.Data).Tree, lookup, flat, "")
			parents = []string{tip}
		}
	}
	// A branch that does not exist yet starts from the default branch tip.
	if tip == "" && branch != rep.DefaultBranch {
		baseTip := rep.tipOf(rep.DefaultBranch)
		if baseTip != "" {
			if obj := lookup(baseTip); obj != nil && obj.Type == "commit" {
				flattenTree(parseCommit(obj.Data).Tree, lookup, flat, "")
				parents = []string{baseTip}
			}
		}
	}
	existing, exists := flat[path]
	if exists {
		if sha == "" {
			errJSON(w, 422, "sha is required to update a file")
			return
		}
		if sha != existing.OID {
			errJSON(w, 409, "sha does not match the current file")
			return
		}
	} else if sha != "" {
		errJSON(w, 422, "sha does not match: the file does not exist")
		return
	}
	blobOID := rep.putObject("blob", raw)
	flat[path] = TreeEntry{Mode: "100644", Name: baseName(path), OID: blobOID}
	put := func(typ string, data []byte) string { return rep.putObject(typ, data) }
	treeOID := buildTreeFromFlat(flat, put)
	seq := s.store.tick()
	commitOID := rep.putObject("commit", buildCommit(treeOID, parents, message, forgeWho, 1700000000+seq))
	rep.Refs["refs/heads/"+branch] = commitOID
	status := 201
	if exists {
		status = 200
	}
	jsonOut(w, status, map[string]any{
		"content": map[string]any{
			"name":     baseName(path),
			"path":     path,
			"sha":      blobOID,
			"size":     len(raw),
			"encoding": "base64",
			"content":  contentB64,
		},
		"commit": map[string]any{"sha": commitOID, "message": message},
	}, nil)
}

// deleteContents removes a file, committing the removal on a branch.
func (s *server) deleteContents(w http.ResponseWriter, r *http.Request, rep *Repo, path string) {
	body, ok := bodyMap(r)
	if !ok {
		errJSON(w, 400, "Invalid JSON body")
		return
	}
	branch := strVal(body, "branch")
	if branch == "" {
		branch = rep.DefaultBranch
	}
	message := strVal(body, "message")
	if message == "" {
		message = "Delete " + path
	}
	sha := strVal(body, "sha")

	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	lookup := func(id string) *Obj { return rep.Objects[id] }
	tip := rep.tipOf(branch)
	if tip == "" {
		errJSON(w, 404, "Not Found")
		return
	}
	obj := lookup(tip)
	if obj == nil || obj.Type != "commit" {
		errJSON(w, 404, "Not Found")
		return
	}
	flat := map[string]TreeEntry{}
	flattenTree(parseCommit(obj.Data).Tree, lookup, flat, "")
	existing, exists := flat[path]
	if !exists {
		errJSON(w, 404, "Not Found")
		return
	}
	if sha != "" && sha != existing.OID {
		errJSON(w, 409, "sha does not match the current file")
		return
	}
	delete(flat, path)
	put := func(typ string, data []byte) string { return rep.putObject(typ, data) }
	treeOID := buildTreeFromFlat(flat, put)
	seq := s.store.tick()
	commitOID := rep.putObject("commit", buildCommit(treeOID, []string{tip}, message, forgeWho, 1700000000+seq))
	rep.Refs["refs/heads/"+branch] = commitOID
	jsonOut(w, 200, map[string]any{
		"commit": map[string]any{"sha": commitOID, "message": message},
	}, nil)
}
