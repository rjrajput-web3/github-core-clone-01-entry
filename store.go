// The in-memory store: every repository, issue, pull request and git object
// the forge serves lives here for the process lifetime. A single RWMutex
// guards it; reads take the read lock, mutations the write lock. Nothing is
// ever written to disk, which keeps the timed workloads fast.
//
// Ordering and timestamps are deterministic: a global sequence counter
// stamps every created record, so two runs that receive the same requests in
// the same order build the same state.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"

// Obj is one git object: its type and exact bytes.
type Obj struct {
	Type string
	Data []byte
}

// Label is a repository label.
type Label struct {
	Name  string
	Color string
	Desc  string
}

// Comment is one issue comment.
type Comment struct {
	ID   int
	Body string
	Seq  int64
}

// Issue is one repository issue.
type Issue struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string // "open" or "closed"
	Labels   []string
	Comments []*Comment
	Seq      int64
	ClosedAt string
}

// Pull is one pull request.
type Pull struct {
	ID           int
	Number       int
	Title        string
	Body         string
	State        string // "open" or "closed"
	Head         string // branch name
	Base         string // branch name
	HeadSHA      string
	BaseSHA      string
	Merged       bool
	MergeSHA     string
	MergedAt     string
	Seq          int64
}

// Repo is one repository with its refs, objects and trackers.
type Repo struct {
	ID            int
	Name          string
	Owner         string
	Private       bool
	Desc          string
	DefaultBranch string
	Refs          map[string]string // full ref name -> oid
	Objects       map[string]*Obj
	Labels        map[string]*Label
	Issues        []*Issue
	Pulls         []*Pull
	NextIssue     int
	NextPull      int
	NextComment   int
	Seq           int64
}

// Store is the whole forge state.
type Store struct {
	mu        sync.RWMutex
	token     string
	viewer    string
	repos     map[string]*Repo
	order     []*Repo
	nextRepo  int
	nextID    int // global numeric id for repos/issues/pulls/comments
	seq       int64
}

func newStore(token, viewer string) *Store {
	return &Store{
		token:    token,
		viewer:   viewer,
		repos:    map[string]*Repo{},
		nextRepo: 1,
		nextID:   1,
	}
}

// tick advances the deterministic sequence and returns its new value.
func (s *Store) tick() int64 {
	s.seq++
	return s.seq
}

// stamp renders a deterministic RFC3339 timestamp from a sequence value.
func stamp(seq int64) string {
	// Base: 2026-01-01T00:00:00Z, plus seq seconds.
	total := int64(1767225600) + seq
	days := total / 86400
	rem := total % 86400
	hh := rem / 3600
	mm := (rem % 3600) / 60
	ss := rem % 60
	// Civil-from-days (Howard Hinnant's algorithm), proleptic Gregorian.
	z := days + 719468
	era := z / 146097
	if z < 0 {
		era = (z - 146096) / 146097
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02dZ", y, m, d, hh, mm, ss)
}

// globalID builds an opaque Relay-style id: base64 of "forge:v1:<kind>:<n>".
func globalID(kind string, n int) string {
	raw := "forge:v1:" + kind + ":" + fmt.Sprint(n)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// parseGlobalID reverses globalID, returning kind and number.
func parseGlobalID(id string) (string, int, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", 0, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 4 || parts[0] != "forge" || parts[1] != "v1" {
		return "", 0, false
	}
	var n int
	if _, err := fmt.Sscanf(parts[3], "%d", &n); err != nil {
		return "", 0, false
	}
	return parts[2], n, true
}

// hashObj returns git's SHA-1 id for an object of type over data.
func hashObj(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// keyOf is the store key for a repository.
func keyOf(owner, name string) string { return owner + "/" + name }

// lookup returns a repo by owner/name, or nil. Callers hold at least a read lock.
func (s *Store) lookup(owner, name string) *Repo {
	return s.repos[keyOf(owner, name)]
}

// createRepo makes a new repository owned by the viewer. It reports false
// when a repository of that name already exists.
func (s *Store) createRepo(name string, private bool, desc string) (*Repo, bool) {
	if s.repos[keyOf(s.viewer, name)] != nil {
		return nil, false
	}
	r := &Repo{
		ID:            s.nextID,
		Name:          name,
		Owner:         s.viewer,
		Private:       private,
		Desc:          desc,
		DefaultBranch: "main",
		Refs:          map[string]string{},
		Objects:       map[string]*Obj{},
		Labels:        map[string]*Label{},
		NextIssue:     1,
		NextPull:      1,
		NextComment:   1,
		Seq:           s.tick(),
	}
	s.nextID++
	s.repos[keyOf(s.viewer, name)] = r
	s.order = append(s.order, r)
	return r, true
}

// deleteRepo removes a repository; false when it does not exist.
func (s *Store) deleteRepo(owner, name string) bool {
	key := keyOf(owner, name)
	if s.repos[key] == nil {
		return false
	}
	delete(s.repos, key)
	for i, r := range s.order {
		if r.Name == name && r.Owner == owner {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return true
}

// putObject stores an object, returning its id.
func (r *Repo) putObject(typ string, data []byte) string {
	oid := hashObj(typ, data)
	if r.Objects[oid] == nil {
		r.Objects[oid] = &Obj{Type: typ, Data: data}
	}
	return oid
}

// refNames returns the repository's ref names in sorted order.
func (r *Repo) refNames() []string {
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// branchNames returns sorted branch short names.
func (r *Repo) branchNames() []string {
	out := []string{}
	for _, n := range r.refNames() {
		if strings.HasPrefix(n, "refs/heads/") {
			out = append(out, strings.TrimPrefix(n, "refs/heads/"))
		}
	}
	return out
}

// tipOf returns the oid a branch points at, or "".
func (r *Repo) tipOf(branch string) string {
	return r.Refs["refs/heads/"+branch]
}

// findIssue returns an issue by number, or nil.
func (r *Repo) findIssue(n int) *Issue {
	for _, is := range r.Issues {
		if is.Number == n {
			return is
		}
	}
	return nil
}

// findPull returns a pull request by number, or nil.
func (r *Repo) findPull(n int) *Pull {
	for _, p := range r.Pulls {
		if p.Number == n {
			return p
		}
	}
	return nil
}

// openPullFor returns an open pull request from the given head branch, or nil.
func (r *Repo) openPullFor(head, base string) *Pull {
	for _, p := range r.Pulls {
		if p.State == "open" && p.Head == head && p.Base == base {
			return p
		}
	}
	return nil
}
