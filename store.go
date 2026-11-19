package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// The login the trial token names. The suite's scenario is drawn from one
// fixed seed, so the viewer is the same on every trial; repositories created
// through POST /user/repos belong to this login, and any other owner is 404.
const viewerLogin = "adaiversen"
const viewerID = 1000

var apiToken string

// Deterministic clock: every timestamp is the fixed epoch plus a sequence
// number of seconds, so two runs on the same instance agree byte for byte.
var epochBase = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

func stampFor(seq int64) string {
	return epochBase.Add(time.Duration(seq) * time.Second).UTC().Format(time.RFC3339)
}

func b64id(kind string, parts ...int) string {
	var sb strings.Builder
	sb.WriteString(kind)
	for _, p := range parts {
		fmt.Fprintf(&sb, ":%d", p)
	}
	return base64.RawStdEncoding.EncodeToString([]byte(sb.String()))
}

func decodeID(id string) (string, []int) {
	raw, err := base64.RawStdEncoding.DecodeString(id)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(id)
		if err != nil {
			return "", nil
		}
	}
	fields := strings.Split(string(raw), ":")
	nums := make([]int, 0, len(fields)-1)
	for _, f := range fields[1:] {
		var n int
		fmt.Sscanf(f, "%d", &n)
		nums = append(nums, n)
	}
	return fields[0], nums
}

func gitHash(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID      int
	IssueNo int
	Body    string
	Created string
}

type Issue struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string // open | closed
	Labels   []string
	Created  string
	Updated  string
	ClosedAt string
	Seq      int64
}

type Pull struct {
	ID           int
	Number       int
	Title        string
	Body         string
	State        string // open | closed
	HeadRef      string
	BaseRef      string
	HeadSha      string
	BaseSha      string // base tip when the pull was opened
	Merged       bool
	Mergeable    bool
	MergeSHA     string
	MergedAt     string
	Created      string
	Updated      string
	ClosedAt     string
	Seq          int64
}

type Repo struct {
	ID            int
	Name          string
	Owner         string
	Private       bool
	Description   string
	DefaultBranch string
	Created       string
	Updated       string
	Refs          map[string]string
	Objects       map[string]*Obj
	Labels        map[string]*Label
	Issues        []*Issue
	Pulls         []*Pull
	Comments      map[int][]*Comment
	NextNum       int
	NextCommentID int
}

type Store struct {
	mu        sync.RWMutex
	repos     map[string]*Repo
	seq       int64
	nextRepo  int
	nextIssue int
}

var store = &Store{repos: map[string]*Repo{}, nextRepo: 5000, nextIssue: 100000}

func (s *Store) tick() int64 {
	s.seq++
	return s.seq
}

func repoKey(owner, name string) string { return owner + "/" + name }

// lookup returns the repo only when the owner matches the token's login.
func (s *Store) lookup(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[repoKey(owner, name)]
}

func (s *Store) createRepo(name string, private bool, desc string) (*Repo, bool) {
	if _, ok := s.repos[repoKey(viewerLogin, name)]; ok {
		return nil, false
	}
	s.nextRepo++
	seq := s.tick()
	r := &Repo{
		ID: s.nextRepo, Name: name, Owner: viewerLogin, Private: private,
		Description: desc, DefaultBranch: "main",
		Created: stampFor(seq), Updated: stampFor(seq),
		Refs: map[string]string{}, Objects: map[string]*Obj{},
		Labels: map[string]*Label{}, Comments: map[int][]*Comment{},
		NextNum: 1, NextCommentID: 1,
	}
	s.repos[repoKey(viewerLogin, name)] = r
	return r, true
}

func (s *Store) newIssue(r *Repo, title, body string, labels []string) *Issue {
	s.nextIssue++
	seq := s.tick()
	it := &Issue{
		ID: s.nextIssue, Number: r.NextNum, Title: title, Body: body,
		State: "open", Labels: append([]string{}, labels...),
		Created: stampFor(seq), Updated: stampFor(seq), Seq: seq,
	}
	r.NextNum++
	r.Issues = append(r.Issues, it)
	return it
}

func (s *Store) newPull(r *Repo, title, body, head, base, headSha, baseSha string) *Pull {
	s.nextIssue++
	seq := s.tick()
	p := &Pull{
		ID: s.nextIssue, Number: r.NextNum, Title: title, Body: body,
		State: "open", HeadRef: head, BaseRef: base,
		HeadSha: headSha, BaseSha: baseSha, Mergeable: true,
		Created: stampFor(seq), Updated: stampFor(seq), Seq: seq,
	}
	r.NextNum++
	r.Pulls = append(r.Pulls, p)
	return p
}

func (r *Repo) findIssue(n int) *Issue {
	for _, it := range r.Issues {
		if it.Number == n {
			return it
		}
	}
	return nil
}

func (r *Repo) findPull(n int) *Pull {
	for _, p := range r.Pulls {
		if p.Number == n {
			return p
		}
	}
	return nil
}

func (r *Repo) tip(branch string) string {
	return r.Refs["refs/heads/"+branch]
}

// ---------------------------------------------------------------------------
// JSON views
// ---------------------------------------------------------------------------

func userView(login string, id int) map[string]any {
	return map[string]any{
		"login": login, "id": id, "node_id": b64id("user", id),
		"type": "User", "site_admin": false,
		"avatar_url": "https://avatars.githubusercontent.com/u/" + fmt.Sprint(id),
		"html_url": "https://github.com/" + login,
		"url":      "/api/v3/users/" + login,
	}
}

func repoView(r *Repo) map[string]any {
	return map[string]any{
		"id": r.ID, "node_id": b64id("repo", r.ID), "name": r.Name,
		"full_name": r.Owner + "/" + r.Name, "owner": userView(r.Owner, viewerID),
		"private": r.Private, "description": r.Description,
		"fork": false, "default_branch": r.DefaultBranch,
		"created_at": r.Created, "updated_at": r.Updated, "pushed_at": r.Updated,
		"homepage": "", "language": nil, "archived": false, "disabled": false,
		"visibility": map[bool]string{true: "private", false: "public"}[r.Private],
		"html_url": "https://github.com/" + r.Owner + "/" + r.Name,
		"clone_url": "https://github.com/" + r.Owner + "/" + r.Name + ".git",
		"git_url": "git://github.com/" + r.Owner + "/" + r.Name + ".git",
		"ssh_url": "git@github.com:" + r.Owner + "/" + r.Name + ".git",
		"url": "/api/v3/repos/" + r.Owner + "/" + r.Name,
		"forks_count": 0, "stargazers_count": 0, "watchers_count": 0,
		"open_issues_count": len(r.Issues), "size": 0,
		"has_issues": true, "has_projects": true, "has_wiki": true,
		"has_pages": false, "has_downloads": true,
	}
}

func labelView(l *Label) map[string]any {
	return map[string]any{"name": l.Name, "color": l.Color, "description": "", "default": false}
}

func issueView(r *Repo, it *Issue) map[string]any {
	labels := make([]any, 0, len(it.Labels))
	for _, name := range it.Labels {
		if l, ok := r.Labels[name]; ok {
			labels = append(labels, labelView(l))
		} else {
			labels = append(labels, map[string]any{"name": name, "color": "ededed", "description": ""})
		}
	}
	closed := any(nil)
	if it.ClosedAt != "" {
		closed = it.ClosedAt
	}
	return map[string]any{
		"id": it.ID, "node_id": b64id("issue", r.ID, it.Number),
		"number": it.Number, "title": it.Title, "body": it.Body,
		"state": it.State, "labels": labels, "user": userView(viewerLogin, viewerID),
		"assignee": nil, "assignees": []any{}, "locked": false,
		"comments": len(r.Comments[it.Number]),
		"created_at": it.Created, "updated_at": it.Updated, "closed_at": closed,
		"html_url": "https://github.com/" + r.Owner + "/" + r.Name + "/issues/" + fmt.Sprint(it.Number),
		"url": "/api/v3/repos/" + r.Owner + "/" + r.Name + "/issues/" + fmt.Sprint(it.Number),
		"repository_url": "/api/v3/repos/" + r.Owner + "/" + r.Name,
	}
}

func commentView(r *Repo, c *Comment) map[string]any {
	return map[string]any{
		"id": c.ID, "node_id": b64id("comment", r.ID, c.IssueNo, c.ID),
		"body": c.Body, "user": userView(viewerLogin, viewerID),
		"created_at": c.Created, "updated_at": c.Created,
		"html_url": "https://github.com/" + r.Owner + "/" + r.Name + "/issues/" + fmt.Sprint(c.IssueNo) + "#issuecomment-" + fmt.Sprint(c.ID),
		"url": "/api/v3/repos/" + r.Owner + "/" + r.Name + "/issues/comments/" + fmt.Sprint(c.ID),
	}
}

func pullView(r *Repo, p *Pull) map[string]any {
	mergedAt := any(nil)
	if p.MergedAt != "" {
		mergedAt = p.MergedAt
	}
	mergeSha := any(nil)
	if p.MergeSHA != "" {
		mergeSha = p.MergeSHA
	}
	closedAt := any(nil)
	if p.ClosedAt != "" {
		closedAt = p.ClosedAt
	}
	headLabel := viewerLogin + ":" + p.HeadRef
	baseLabel := viewerLogin + ":" + p.BaseRef
	return map[string]any{
		"id": p.ID, "node_id": b64id("pull", r.ID, p.Number),
		"number": p.Number, "title": p.Title, "body": p.Body,
		"state": p.State, "locked": false, "draft": false,
		"merged": p.Merged, "mergeable": p.Mergeable, "mergeable_state": "clean",
		"merge_commit_sha": mergeSha, "merged_at": mergedAt, "closed_at": closedAt,
		"created_at": p.Created, "updated_at": p.Updated,
		"user": userView(viewerLogin, viewerID),
		"head": map[string]any{"ref": p.HeadRef, "sha": p.HeadSha, "label": headLabel, "user": userView(viewerLogin, viewerID)},
		"base": map[string]any{"ref": p.BaseRef, "sha": p.BaseSha, "label": baseLabel, "user": userView(viewerLogin, viewerID)},
		"html_url": "https://github.com/" + r.Owner + "/" + r.Name + "/pull/" + fmt.Sprint(p.Number),
		"url": "/api/v3/repos/" + r.Owner + "/" + r.Name + "/pulls/" + fmt.Sprint(p.Number),
	}
}

// sortedIssues returns issues ordered by creation sequence.
func (r *Repo) sortedIssues(asc bool) []*Issue {
	out := append([]*Issue{}, r.Issues...)
	sort.Slice(out, func(i, j int) bool {
		if asc {
			return out[i].Seq < out[j].Seq
		}
		return out[i].Seq > out[j].Seq
	})
	return out
}

func (r *Repo) sortedPulls(asc bool) []*Pull {
	out := append([]*Pull{}, r.Pulls...)
	sort.Slice(out, func(i, j int) bool {
		if asc {
			return out[i].Seq < out[j].Seq
		}
		return out[i].Seq > out[j].Seq
	})
	return out
}
