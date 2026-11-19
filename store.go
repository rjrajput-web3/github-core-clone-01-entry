// GitForge store: the in-memory forge. One process, no database, no git binary.
// Faithful port of the Node store.ts: git objects, refs, repositories, issues,
// pull requests, labels and comments, all in maps.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"time"
)

const ZeroOID = "0000000000000000000000000000000000000000"
const ViewerLogin = "adaiversen"
const ViewerID = 1000
const ViewerName = "ada iversen"
const ViewerEmail = "adaiversen@example.invalid"
const StrangerLogin = "olgabauer"
const DefaultBranch = "main"
const ForgeName = "GitForge"
const ForgeVersion = "1.0.0"

type GitObjectType string

const (
	ObjCommit GitObjectType = "commit"
	ObjTree   GitObjectType = "tree"
	ObjBlob   GitObjectType = "blob"
	ObjTag    GitObjectType = "tag"
)

func sha1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}

func hashObject(typ GitObjectType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(string(typ) + " " + itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	AuthorTs  int64
	Message   string
}

type TagInfo struct {
	Object string
	Type   string
	Tag    string
	Tagger string
}

func parseCommit(data []byte) *CommitInfo {
	text := string(data)
	end := strings.Index(text, "\n\n")
	var head, message string
	if end == -1 {
		head, message = text, ""
	} else {
		head, message = text[:end], text[end+2:]
	}
	ci := &CommitInfo{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			ci.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			ci.Parents = append(ci.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			ci.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			ci.Committer = line[10:]
		}
	}
	ci.AuthorTs = tsOf(ci.Author)
	ci.Message = message
	return ci
}

var tsRe = regexp.MustCompile(`> (\d+) `)

func tsOf(line string) int64 {
	m := tsRe.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	var n int64
	for _, c := range m[1] {
		n = n*10 + int64(c-'0')
	}
	return n
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], 0x20)
		if sp == -1 {
			break
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul == -1 {
			break
		}
		nul += sp + 1
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[off:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		an := sorted[i].Name
		if sorted[i].Mode == "40000" {
			an += "/"
		}
		bn := sorted[j].Name
		if sorted[j].Mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

func parseTag(data []byte) *TagInfo {
	text := string(data)
	end := strings.Index(text, "\n\n")
	var head string
	if end == -1 {
		head = text
	} else {
		head = text[:end]
	}
	ti := &TagInfo{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			ti.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			ti.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			ti.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			ti.Tagger = line[7:]
		}
	}
	return ti
}

func identLine(name, email string, ts int64) string {
	return name + " <" + email + "> " + itoa(int(ts)) + " +0000"
}

func buildCommit(tree string, parents []string, message string) []byte {
	ts := time.Now().Unix()
	author := identLine(ViewerName, ViewerEmail, ts)
	committer := identLine(ViewerName, ViewerEmail, ts)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	var head strings.Builder
	head.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		head.WriteString("parent " + p + "\n")
	}
	head.WriteString("author " + author + "\ncommitter " + committer + "\n")
	return []byte(head.String() + "\n" + message)
}

type StoredObject struct {
	Type    GitObjectType
	Data    []byte
	Commit  *CommitInfo
	Entries []TreeEntry
	Tag     *TagInfo
}

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID        int
	Body      string
	User      string
	CreatedAt string
	Seq       int
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	User      string
	CreatedAt string
	UpdatedAt string
	Seq       int
	Comments  []*Comment
	JSONBuf   []byte // cached JSON, nil = dirty
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       string
	MergeCommitSha string
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	User           string
	CreatedAt      string
	UpdatedAt      string
	Seq            int
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	Seq           int
	Refs          map[string]string
	Labels        map[string]*Label
	Issues        map[int]*Issue
	Pulls         map[int]*Pull
	NextNumber    int
	FetchCache    map[string][]byte
	AdvV2         []byte
	AdvV0Up       []byte
	AdvV0Recv     []byte
}

func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func gid(kind string, parts ...interface{}) string {
	var sb strings.Builder
	sb.WriteString(kind)
	for _, p := range parts {
		sb.WriteString(":")
		switch v := p.(type) {
		case string:
			sb.WriteString(v)
		case int:
			sb.WriteString(itoa(v))
		}
	}
	return base64.RawURLEncoding.EncodeToString([]byte(sb.String()))
}

type ungidResult struct {
	Kind  string
	Parts []string
}

func ungid(id string) *ungidResult {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	text := string(raw)
	sep := strings.Index(text, ":")
	if sep < 1 {
		return nil
	}
	return &ungidResult{Kind: text[:sep], Parts: strings.Split(text[sep+1:], ":")}
}

type Store struct {
	Objects   map[string]*StoredObject
	Repos     map[string]*Repo
	NextRepo  int
	NextItem  int
	NextSeq   int
	flatCache map[string]map[string]FlatEntry
}

type FlatEntry struct {
	OID  string
	Mode string
}

func NewStore() *Store {
	return &Store{
		Objects:   make(map[string]*StoredObject),
		Repos:     make(map[string]*Repo),
		NextRepo:  1,
		NextItem:  1,
		NextSeq:   1,
		flatCache: make(map[string]map[string]FlatEntry),
	}
}

func (s *Store) putObject(typ GitObjectType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := s.Objects[oid]; !ok {
		obj := &StoredObject{Type: typ, Data: data}
		if typ == ObjCommit {
			obj.Commit = parseCommit(data)
		}
		if typ == ObjTag {
			obj.Tag = parseTag(data)
		}
		s.Objects[oid] = obj
	}
	return oid
}

func (s *Store) getObject(oid string) *StoredObject {
	return s.Objects[oid]
}

func (s *Store) treeEntries(oid string) []TreeEntry {
	obj := s.Objects[oid]
	if obj == nil || obj.Type != ObjTree {
		return nil
	}
	if obj.Entries == nil {
		obj.Entries = parseTree(obj.Data)
	}
	return obj.Entries
}

func (s *Store) peelToCommit(oid string) string {
	cur := oid
	for i := 0; i < 8; i++ {
		obj := s.Objects[cur]
		if obj == nil || obj.Type != ObjTag || obj.Tag == nil {
			return cur
		}
		cur = obj.Tag.Object
	}
	return cur
}

func (s *Store) isAncestor(ancestor, tip string) bool {
	if ancestor == tip {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{tip}
	steps := 0
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == ancestor {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		steps++
		if steps > 200000 {
			return false
		}
		if obj := s.Objects[cur]; obj != nil && obj.Commit != nil {
			stack = append(stack, obj.Commit.Parents...)
		}
	}
	return false
}

func (s *Store) mergeBase(a, b string) string {
	ancestors := make(map[string]bool)
	stack := []string{a}
	steps := 0
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancestors[cur] {
			continue
		}
		ancestors[cur] = true
		steps++
		if steps > 200000 {
			break
		}
		if obj := s.Objects[cur]; obj != nil && obj.Commit != nil {
			stack = append(stack, obj.Commit.Parents...)
		}
	}
	queue := []string{b}
	seen := make(map[string]bool)
	for qi := 0; qi < len(queue); qi++ {
		cur := queue[qi]
		if ancestors[cur] {
			return cur
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if obj := s.Objects[cur]; obj != nil && obj.Commit != nil {
			queue = append(queue, obj.Commit.Parents...)
		}
	}
	return ""
}

func (s *Store) flattenTree(treeOID string) map[string]FlatEntry {
	if hit, ok := s.flatCache[treeOID]; ok {
		return hit
	}
	flat := make(map[string]FlatEntry)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		for _, e := range s.treeEntries(oid) {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, path)
			} else {
				flat[path] = FlatEntry{OID: e.OID, Mode: e.Mode}
			}
		}
	}
	walk(treeOID, "")
	if len(s.flatCache) > 4000 {
		s.flatCache = make(map[string]map[string]FlatEntry)
	}
	s.flatCache[treeOID] = flat
	return flat
}

func (s *Store) buildTreeFromFlat(flat map[string]FlatEntry) string {
	var buildDir func(prefix string) string
	buildDir = func(prefix string) string {
		var entries []TreeEntry
		dirs := make(map[string]bool)
		for path, v := range flat {
			if prefix != "" && !strings.HasPrefix(path, prefix+"/") {
				continue
			}
			rest := path
			if prefix != "" {
				rest = path[len(prefix)+1:]
			}
			if idx := strings.Index(rest, "/"); idx == -1 {
				entries = append(entries, TreeEntry{Mode: v.Mode, Name: rest, OID: v.OID})
			} else {
				dirs[rest[:idx]] = true
			}
		}
		for d := range dirs {
			sub := d
			if prefix != "" {
				sub = prefix + "/" + d
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: d, OID: buildDir(sub)})
		}
		return s.putObject(ObjTree, buildTree(entries))
	}
	return buildDir("")
}

func (s *Store) createCommit(tree string, parents []string, message string) string {
	return s.putObject(ObjCommit, buildCommit(tree, parents, message))
}

type FetchResult struct {
	Objects   map[string]*StoredObject
	Shallow   []string
	Unshallow []string
}

func (s *Store) collectForFetch(wants, haves, shallowIn []string, depth int, hasDepth bool) FetchResult {
	shallowSet := make(map[string]bool)
	for _, x := range shallowIn {
		shallowSet[x] = true
	}
	excluded := make(map[string]bool)
	{
		stack := append([]string{}, haves...)
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if excluded[cur] {
				continue
			}
			excluded[cur] = true
			obj := s.Objects[cur]
			if obj == nil {
				continue
			}
			if obj.Type == ObjCommit && obj.Commit != nil {
				stack = append(stack, obj.Commit.Tree)
				if !shallowSet[cur] {
					stack = append(stack, obj.Commit.Parents...)
				}
			} else if obj.Type == ObjTree {
				for _, e := range s.treeEntries(cur) {
					stack = append(stack, e.OID)
				}
			} else if obj.Type == ObjTag && obj.Tag != nil {
				stack = append(stack, obj.Tag.Object)
			}
		}
	}
	wanted := make(map[string]*StoredObject)
	shallowOut := make(map[string]bool)
	seenDepth := make(map[string]int)
	type qitem struct {
		oid   string
		depth int
	}
	queue := []qitem{}
	for _, w := range wants {
		queue = append(queue, qitem{w, 1})
	}
	addObj := func(oid string) *StoredObject {
		obj := s.Objects[oid]
		if obj != nil {
			if _, ok := wanted[oid]; !ok {
				wanted[oid] = obj
			}
		}
		return obj
	}
	pushTreeContents := func(treeOID string) {
		stack := []string{treeOID}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			addObj(cur)
			for _, e := range s.treeEntries(cur) {
				addObj(e.OID)
				if e.Mode == "40000" {
					stack = append(stack, e.OID)
				}
			}
		}
	}
	for qi := 0; qi < len(queue); qi++ {
		it := queue[qi]
		obj := s.Objects[it.oid]
		if obj == nil {
			continue
		}
		if obj.Type == ObjCommit {
			if prev, ok := seenDepth[it.oid]; ok && prev <= it.depth {
				continue
			}
			seenDepth[it.oid] = it.depth
			addObj(it.oid)
			if obj.Commit != nil {
				addObj(obj.Commit.Tree)
				pushTreeContents(obj.Commit.Tree)
			}
			if hasDepth && it.depth >= depth {
				shallowOut[it.oid] = true
				continue
			}
			delete(shallowOut, it.oid)
			if obj.Commit != nil {
				for _, p := range obj.Commit.Parents {
					queue = append(queue, qitem{p, it.depth + 1})
				}
			}
		} else if obj.Type == ObjTag {
			addObj(it.oid)
			if obj.Tag != nil {
				queue = append(queue, qitem{obj.Tag.Object, it.depth})
			}
		} else if obj.Type == ObjTree {
			addObj(it.oid)
			pushTreeContents(it.oid)
		} else {
			addObj(it.oid)
		}
	}
	objects := make(map[string]*StoredObject)
	for oid, obj := range wanted {
		if !excluded[oid] {
			objects[oid] = obj
		}
	}
	var shallow []string
	for oid := range shallowOut {
		shallow = append(shallow, oid)
	}
	sort.Strings(shallow)
	var unshallow []string
	for _, old := range shallowIn {
		if _, ok := seenDepth[old]; ok && !shallowOut[old] {
			unshallow = append(unshallow, old)
		}
	}
	return FetchResult{Objects: objects, Shallow: shallow, Unshallow: unshallow}
}

func (s *Store) repoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) createRepo(owner, name, description string, isPrivate bool) *Repo {
	key := s.repoKey(owner, name)
	if _, ok := s.Repos[key]; ok {
		return nil
	}
	repo := &Repo{
		ID:            s.NextRepo,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       isPrivate,
		DefaultBranch: DefaultBranch,
		CreatedAt:     isoNow(),
		Seq:           s.NextSeq,
		Refs:          make(map[string]string),
		Labels:        make(map[string]*Label),
		Issues:        make(map[int]*Issue),
		Pulls:         make(map[int]*Pull),
		NextNumber:    1,
		FetchCache:    make(map[string][]byte),
	}
	s.NextRepo++
	s.NextSeq++
	s.Repos[key] = repo
	return repo
}

func (s *Store) getRepo(owner, name string) *Repo {
	return s.Repos[s.repoKey(owner, name)]
}

func (s *Store) deleteRepo(owner, name string) bool {
	key := s.repoKey(owner, name)
	if _, ok := s.Repos[key]; !ok {
		return false
	}
	delete(s.Repos, key)
	return true
}

func (s *Store) listViewerRepos() []*Repo {
	var out []*Repo
	for _, repo := range s.Repos {
		if repo.Owner == ViewerLogin {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (s *Store) invalidateRepoCache(repo *Repo) {
	repo.FetchCache = make(map[string][]byte)
	repo.AdvV2 = nil
	repo.AdvV0Up = nil
	repo.AdvV0Recv = nil
}

func (s *Store) setRef(repo *Repo, ref string, oid string, del bool) {
	if del {
		delete(repo.Refs, ref)
	} else {
		repo.Refs[ref] = oid
	}
	s.invalidateRepoCache(repo)
}

func (s *Store) defaultTip(repo *Repo) (string, bool) {
	tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
	return tip, ok
}

func (s *Store) createIssue(repo *Repo, title, body string, labels []string) (*Issue, string) {
	for _, name := range labels {
		if _, ok := repo.Labels[name]; !ok {
			return nil, `label "` + name + `" does not exist`
		}
	}
	now := isoNow()
	issue := &Issue{
		ID:        s.NextItem,
		Number:    repo.NextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]string{}, labels...),
		User:      ViewerLogin,
		CreatedAt: now,
		UpdatedAt: now,
		Seq:       s.NextSeq,
		Comments:  []*Comment{},
	}
	s.NextItem++
	repo.NextNumber++
	s.NextSeq++
	repo.Issues[issue.Number] = issue
	return issue, ""
}

func (s *Store) createPull(repo *Repo, title, body, head, base string) (*Pull, string) {
	headTip, ok := repo.Refs["refs/heads/"+head]
	if !ok {
		return nil, `head branch "` + head + `" does not exist`
	}
	baseTip, ok := repo.Refs["refs/heads/"+base]
	if !ok {
		return nil, `base branch "` + base + `" does not exist`
	}
	for _, pr := range repo.Pulls {
		if pr.State == "open" && pr.HeadRef == head && pr.BaseRef == base {
			return nil, "a pull request already exists for this head and base"
		}
	}
	now := isoNow()
	pr := &Pull{
		ID:        s.NextItem,
		Number:    repo.NextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		Merged:    false,
		MergedAt:  "",
		HeadRef:   head,
		BaseRef:   base,
		HeadSha:   headTip,
		BaseSha:   baseTip,
		User:      ViewerLogin,
		CreatedAt: now,
		UpdatedAt: now,
		Seq:       s.NextSeq,
	}
	s.NextItem++
	repo.NextNumber++
	s.NextSeq++
	repo.Pulls[pr.Number] = pr
	return pr, ""
}

type DiffEntry struct {
	Path   string
	Status string // added | removed | modified
	OldOID string // "" = null
	NewOID string // "" = null
}

func (s *Store) diffTrees(baseTree, headTree string, hasBase, hasHead bool) []DiffEntry {
	var base, head map[string]FlatEntry
	if hasBase {
		base = s.flattenTree(baseTree)
	} else {
		base = map[string]FlatEntry{}
	}
	if hasHead {
		head = s.flattenTree(headTree)
	} else {
		head = map[string]FlatEntry{}
	}
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var out []DiffEntry
	for path := range paths {
		b, bok := base[path]
		h, hok := head[path]
		if bok && !hok {
			out = append(out, DiffEntry{path, "removed", b.OID, ""})
		} else if !bok && hok {
			out = append(out, DiffEntry{path, "added", "", h.OID})
		} else if bok && hok && b.OID != h.OID {
			out = append(out, DiffEntry{path, "modified", b.OID, h.OID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (s *Store) pullFiles(repo *Repo, pr *Pull) []DiffEntry {
	headTip, ok := repo.Refs["refs/heads/"+pr.HeadRef]
	if !ok {
		return nil
	}
	baseTip, ok := repo.Refs["refs/heads/"+pr.BaseRef]
	if !ok {
		return nil
	}
	base := s.mergeBase(headTip, baseTip)
	var baseTree string
	var hasBase bool
	if base != "" {
		if obj := s.Objects[base]; obj != nil && obj.Commit != nil {
			baseTree, hasBase = obj.Commit.Tree, true
		}
	}
	var headTree string
	var hasHead bool
	if obj := s.Objects[headTip]; obj != nil && obj.Commit != nil {
		headTree, hasHead = obj.Commit.Tree, true
	}
	return s.diffTrees(baseTree, headTree, hasBase, hasHead)
}

func (s *Store) pullMergeable(repo *Repo, pr *Pull) string {
	headTip, ok := repo.Refs["refs/heads/"+pr.HeadRef]
	if !ok {
		return "CONFLICTING"
	}
	baseTip, ok := repo.Refs["refs/heads/"+pr.BaseRef]
	if !ok {
		return "CONFLICTING"
	}
	if conflict, _ := s.threeWay(repo, baseTip, headTip); conflict {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

func (s *Store) threeWay(repo *Repo, baseTip, headTip string) (bool, string) {
	base := s.mergeBase(headTip, baseTip)
	var baseTree string
	var hasBase bool
	if base != "" {
		if obj := s.Objects[base]; obj != nil && obj.Commit != nil {
			baseTree, hasBase = obj.Commit.Tree, true
		}
	}
	var oursTree string
	var hasOurs bool
	if obj := s.Objects[baseTip]; obj != nil && obj.Commit != nil {
		oursTree, hasOurs = obj.Commit.Tree, true
	}
	var theirsTree string
	var hasTheirs bool
	if obj := s.Objects[headTip]; obj != nil && obj.Commit != nil {
		theirsTree, hasTheirs = obj.Commit.Tree, true
	}
	var b, o, t map[string]FlatEntry
	if hasBase {
		b = s.flattenTree(baseTree)
	} else {
		b = map[string]FlatEntry{}
	}
	if hasOurs {
		o = s.flattenTree(oursTree)
	} else {
		o = map[string]FlatEntry{}
	}
	if hasTheirs {
		t = s.flattenTree(theirsTree)
	} else {
		t = map[string]FlatEntry{}
	}
	merged := make(map[string]FlatEntry)
	conflict := false
	paths := make(map[string]bool)
	for p := range b {
		paths[p] = true
	}
	for p := range o {
		paths[p] = true
	}
	for p := range t {
		paths[p] = true
	}
	eq := func(x, y *FlatEntry) bool {
		if x == nil && y == nil {
			return true
		}
		return x != nil && y != nil && x.OID == y.OID
	}
	for path := range paths {
		var bv, ov, tv *FlatEntry
		if v, ok := b[path]; ok {
			vv := v
			bv = &vv
		}
		if v, ok := o[path]; ok {
			vv := v
			ov = &vv
		}
		if v, ok := t[path]; ok {
			vv := v
			tv = &vv
		}
		if eq(ov, tv) {
			if ov != nil {
				merged[path] = *ov
			}
		} else if eq(ov, bv) {
			if tv != nil {
				merged[path] = *tv
			}
		} else if eq(tv, bv) {
			if ov != nil {
				merged[path] = *ov
			}
		} else {
			conflict = true
		}
	}
	if conflict {
		return true, ""
	}
	return false, s.buildTreeFromFlat(merged)
}

func (s *Store) mergePull(repo *Repo, pr *Pull) (bool, string, string) {
	if pr.State != "open" {
		return false, "", "pull request is already closed"
	}
	headTip, ok := repo.Refs["refs/heads/"+pr.HeadRef]
	if !ok {
		return false, "", "head or base branch is missing"
	}
	baseTip, ok := repo.Refs["refs/heads/"+pr.BaseRef]
	if !ok {
		return false, "", "head or base branch is missing"
	}
	conflict, treeOID := s.threeWay(repo, baseTip, headTip)
	if conflict || treeOID == "" {
		return false, "", "merge conflict"
	}
	sha := s.createCommit(treeOID, []string{baseTip}, "Merge pull request #"+itoa(pr.Number)+" from "+pr.HeadRef)
	s.setRef(repo, "refs/heads/"+pr.BaseRef, sha, false)
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = isoNow()
	pr.MergeCommitSha = sha
	pr.UpdatedAt = isoNow()
	return true, sha, ""
}

func (s *Store) listCommits(tip string, limit int) []struct {
	Sha    string
	Commit *CommitInfo
} {
	var out []struct {
		Sha    string
		Commit *CommitInfo
	}
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 && len(out) < limit {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := s.Objects[cur]
		if obj == nil || obj.Commit == nil {
			continue
		}
		out = append(out, struct {
			Sha    string
			Commit *CommitInfo
		}{cur, obj.Commit})
		for i := len(obj.Commit.Parents) - 1; i >= 0; i-- {
			stack = append(stack, obj.Commit.Parents[i])
		}
	}
	return out
}
