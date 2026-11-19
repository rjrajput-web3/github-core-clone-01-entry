// GraphQL v4: a small hand-rolled parser and executor for the documents the
// conformance suite sends — viewer, repository, node, __type introspection,
// issue/pull-request queries and the five mutations. It is not a general
// GraphQL engine; it covers fields, arguments, variables, enums, aliases,
// inline fragments and batched request arrays.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- lexer

type gqlToken struct {
	kind string // name, string, int, float, punct, eof
	val  string
}

func lexGQL(src string) []gqlToken {
	var toks []gqlToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '!' || c == '=' || c == '@' || c == '$' || c == '|':
			toks = append(toks, gqlToken{"punct", string(c)})
			i++
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlToken{"punct", "..."})
			i += 3
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					default:
						sb.WriteByte(src[j])
					}
					j++
					continue
				}
				sb.WriteByte(src[j])
				j++
			}
			toks = append(toks, gqlToken{"string", sb.String()})
			i = j + 1
		case c >= '0' && c <= '9' || c == '-':
			j := i
			isFloat := false
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '-' || src[j] == '+') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			toks = append(toks, gqlToken{kind, src[i:j]})
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			toks = append(toks, gqlToken{"name", src[i:j]})
			i = j
		default:
			i++
		}
	}
	toks = append(toks, gqlToken{"eof", ""})
	return toks
}

// ---------------------------------------------------------------- AST

type gqlValue struct {
	kind string // string, int, float, bool, null, enum, var, list, object
	str  string
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlField struct {
	alias     string
	name      string
	args      map[string]*gqlValue
	selection []*gqlField
	frag      string // inline fragment type condition ("" = plain field)
	fragName  string // named fragment spread
}

type gqlOp struct {
	kind      string // query, mutation, subscription
	name      string
	varDefs   map[string]*gqlValue // default values by variable name
	selection []*gqlField
}

type gqlDoc struct {
	ops       []*gqlOp
	fragments map[string][]*gqlField
}

type gqlParser struct {
	toks []gqlToken
	pos  int
}

func (p *gqlParser) peek() gqlToken  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlToken  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) atPunct(v string) bool {
	t := p.peek()
	return t.kind == "punct" && t.val == v
}
func (p *gqlParser) eatPunct(v string) bool {
	if p.atPunct(v) {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) parseDocument() *gqlDoc {
	doc := &gqlDoc{fragments: map[string][]*gqlField{}}
	for p.peek().kind != "eof" {
		if p.atPunct("{") {
			op := &gqlOp{kind: "query", varDefs: map[string]*gqlValue{}}
			op.selection = p.parseSelection()
			doc.ops = append(doc.ops, op)
			continue
		}
		kw := p.next()
		if kw.kind != "name" {
			continue
		}
		switch kw.val {
		case "query", "mutation", "subscription":
			op := &gqlOp{kind: kw.val, varDefs: map[string]*gqlValue{}}
			if p.peek().kind == "name" {
				op.name = p.next().val
			}
			if p.eatPunct("(") {
				for !p.eatPunct(")") && p.peek().kind != "eof" {
					if p.eatPunct("$") {
						vname := p.next().val
						// Skip the type (name, list, non-null markers).
						for !p.atPunct("=") && !p.atPunct(",") && !p.atPunct(")") {
							p.next()
						}
						if p.eatPunct("=") {
							op.varDefs[vname] = p.parseValue()
						}
					} else {
						p.next()
					}
					p.eatPunct(",")
				}
			}
			op.selection = p.parseSelection()
			doc.ops = append(doc.ops, op)
		case "fragment":
			name := p.next().val
			p.next() // "on"
			p.next() // type name
			doc.fragments[name] = p.parseSelection()
		default:
			// Unknown keyword: skip to the next selection set.
			for !p.atPunct("{") && p.peek().kind != "eof" {
				p.next()
			}
			if p.atPunct("{") {
				p.parseSelection()
			}
		}
	}
	return doc
}

func (p *gqlParser) parseSelection() []*gqlField {
	var fields []*gqlField
	if !p.eatPunct("{") {
		return fields
	}
	for !p.eatPunct("}") && p.peek().kind != "eof" {
		f := p.parseField()
		if f != nil {
			fields = append(fields, f)
		}
	}
	return fields
}

func (p *gqlParser) parseField() *gqlField {
	// Inline fragment or spread.
	if p.atPunct("...") {
		p.next()
		if p.peek().kind == "name" && p.peek().val == "on" {
			p.next()
			typ := p.next().val
			return &gqlField{frag: typ, selection: p.parseSelection()}
		}
		name := p.next().val
		return &gqlField{fragName: name}
	}
	f := &gqlField{args: map[string]*gqlValue{}}
	if p.peek().kind != "name" {
		p.next()
		return nil
	}
	f.name = p.next().val
	if p.eatPunct(":") {
		f.alias = f.name
		f.name = p.next().val
	}
	if p.eatPunct("(") {
		for !p.eatPunct(")") && p.peek().kind != "eof" {
			argName := p.next().val
			p.eatPunct(":")
			f.args[argName] = p.parseValue()
			p.eatPunct(",")
		}
	}
	// Directives: skip @name(args).
	for p.atPunct("@") {
		p.next()
		if p.peek().kind == "name" {
			p.next()
		}
		if p.eatPunct("(") {
			depth := 1
			for depth > 0 && p.peek().kind != "eof" {
				t := p.next()
				if t.kind == "punct" && t.val == "(" {
					depth++
				} else if t.kind == "punct" && t.val == ")" {
					depth--
				}
			}
		}
	}
	if p.atPunct("{") {
		f.selection = p.parseSelection()
	}
	return f
}

func (p *gqlParser) parseValue() *gqlValue {
	t := p.next()
	switch t.kind {
	case "string":
		return &gqlValue{kind: "string", str: t.val}
	case "int":
		return &gqlValue{kind: "int", str: t.val}
	case "float":
		return &gqlValue{kind: "float", str: t.val}
	case "name":
		switch t.val {
		case "true", "false":
			return &gqlValue{kind: "bool", str: t.val}
		case "null":
			return &gqlValue{kind: "null"}
		default:
			return &gqlValue{kind: "enum", str: t.val}
		}
	case "punct":
		switch t.val {
		case "$":
			return &gqlValue{kind: "var", str: p.next().val}
		case "[":
			v := &gqlValue{kind: "list"}
			for !p.eatPunct("]") && p.peek().kind != "eof" {
				v.list = append(v.list, p.parseValue())
				p.eatPunct(",")
			}
			return v
		case "{":
			v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
			for !p.eatPunct("}") && p.peek().kind != "eof" {
				key := p.next().val
				p.eatPunct(":")
				v.obj[key] = p.parseValue()
				p.eatPunct(",")
			}
			return v
		}
	}
	return &gqlValue{kind: "null"}
}

// ---------------------------------------------------------------- values

// resolveValue turns an AST value into a Go value using the operation's
// variable bindings.
func resolveValue(v *gqlValue, vars map[string]any) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "string":
		return v.str
	case "int":
		n, _ := strconv.Atoi(v.str)
		return n
	case "float":
		f, _ := strconv.ParseFloat(v.str, 64)
		return f
	case "bool":
		return v.str == "true"
	case "null":
		return nil
	case "enum":
		return v.str
	case "var":
		if val, ok := vars[v.str]; ok {
			return val
		}
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, resolveValue(item, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, item := range v.obj {
			out[k] = resolveValue(item, vars)
		}
		return out
	}
	return nil
}

func (f *gqlField) arg(name string, vars map[string]any) any {
	if v, ok := f.args[name]; ok {
		return resolveValue(v, vars)
	}
	return nil
}

func (f *gqlField) argStr(name string, vars map[string]any) string {
	if v := f.arg(name, vars); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func (f *gqlField) argInt(name string, vars map[string]any, def int) int {
	if v := f.arg(name, vars); v != nil {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return int(i)
			}
		}
	}
	return def
}

func (f *gqlField) key() string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

// ---------------------------------------------------------------- executor

type gqlExecutor struct {
	s      *server
	doc    *gqlDoc
	vars   map[string]any
	errors []map[string]any
}

func (e *gqlExecutor) failf(format string, args ...any) {
	e.errors = append(e.errors, map[string]any{"message": fmt.Sprintf(format, args...)})
}

// execute runs one operation and returns its data object.
func (e *gqlExecutor) execute(op *gqlOp) map[string]any {
	data := map[string]any{}
	for _, f := range op.selection {
		e.execFields("Query", nil, f, data)
	}
	return data
}

// execFields resolves one field (or fragment) on a parent of the given type
// and writes its result into out.
func (e *gqlExecutor) execFields(parentType string, parent any, f *gqlField, out map[string]any) {
	if f.fragName != "" {
		if sel, ok := e.doc.fragments[f.fragName]; ok {
			for _, sub := range sel {
				e.execFields(parentType, parent, sub, out)
			}
		}
		return
	}
	if f.frag != "" {
		// Inline fragment: apply when the parent type matches the condition.
		if parentType == f.frag {
			for _, sub := range f.selection {
				e.execFields(parentType, parent, sub, out)
			}
		}
		return
	}
	val, typ := e.resolveField(parentType, parent, f)
	if typ == "" {
		if val == nil {
			e.failf("Cannot query field %q on type %q.", f.name, parentType)
			out[f.key()] = nil
			return
		}
		// A scalar (e.g. __typename) with no sub-selection type.
		out[f.key()] = val
		return
	}
	out[f.key()] = e.project(typ, val, f)
}

// project applies a field's sub-selection to a resolved value.
func (e *gqlExecutor) project(typ string, val any, f *gqlField) any {
	if val == nil {
		return nil
	}
	if len(f.selection) == 0 {
		return val
	}
	if list, ok := val.([]any); ok {
		out := make([]any, 0, len(list))
		for _, item := range list {
			out = append(out, e.projectObject(typ, item, f))
		}
		return out
	}
	return e.projectObject(typ, val, f)
}

func (e *gqlExecutor) projectObject(typ string, val any, f *gqlField) any {
	obj := map[string]any{}
	for _, sub := range f.selection {
		e.execFields(typ, val, sub, obj)
	}
	return obj
}

// resolveField computes a field's raw value and the object type its
// sub-selection should be projected against ("" when unknown).
func (e *gqlExecutor) resolveField(parentType string, parent any, f *gqlField) (any, string) {
	s := e.s
	vars := e.vars
	switch parentType {
	case "Query":
		switch f.name {
		case "viewer":
			return map[string]any{"login": s.store.viewer}, "User"
		case "repository":
			owner := f.argStr("owner", vars)
			name := f.argStr("name", vars)
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			rep := s.store.lookup(owner, name)
			if rep == nil || owner != s.store.viewer {
				return nil, "Repository"
			}
			return rep, "Repository"
		case "node":
			id := f.argStr("id", vars)
			kind, num, ok := parseGlobalID(id)
			if !ok {
				return nil, "Node"
			}
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			if kind == "repo" {
				for _, rep := range s.store.order {
					if rep.ID == num {
						return rep, "Repository"
					}
				}
			}
			return nil, "Node"
		case "__type":
			name := f.argStr("name", vars)
			info := typeInfo(name)
			if info == nil {
				return nil, "__Type"
			}
			return info, "__Type"
		case "__typename":
			return "Query", ""
		}
	case "User":
		if u, ok := parent.(map[string]any); ok && f.name == "login" {
			return u["login"], ""
		}
	case "Repository":
		rep := parent.(*Repo)
		switch f.name {
		case "id":
			return globalID("repo", rep.ID), ""
		case "databaseId":
			return rep.ID, ""
		case "name":
			return rep.Name, ""
		case "nameWithOwner":
			return rep.Owner + "/" + rep.Name, ""
		case "owner":
			return map[string]any{"login": rep.Owner}, "User"
		case "defaultBranchRef":
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			if rep.tipOf(rep.DefaultBranch) == "" {
				return nil, "Ref"
			}
			return map[string]any{"name": rep.DefaultBranch}, "Ref"
		case "viewerPermission":
			return "ADMIN", ""
		case "hasIssuesEnabled":
			return true, ""
		case "issue":
			num := f.argInt("number", vars, 0)
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			is := rep.findIssue(num)
			if is == nil {
				return nil, "Issue"
			}
			return is, "Issue"
		case "issues":
			return e.repoIssues(rep, f), "IssueConnection"
		case "pullRequest":
			num := f.argInt("number", vars, 0)
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			p := rep.findPull(num)
			if p == nil {
				return nil, "PullRequest"
			}
			return p, "PullRequest"
		case "pullRequests":
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			first := f.argInt("first", vars, 10)
			list := make([]any, 0)
			for i, p := range rep.Pulls {
				if i >= first {
					break
				}
				list = append(list, p)
			}
			return map[string]any{"nodes": list}, "PullRequestConnection"
		case "__typename":
			return "Repository", ""
		}
	case "Issue":
		is := parent.(*Issue)
		switch f.name {
		case "id":
			return globalID("issue", is.ID), ""
		case "number":
			return is.Number, ""
		case "title":
			return is.Title, ""
		case "body":
			return is.Body, ""
		case "state":
			if is.State == "closed" {
				return "CLOSED", ""
			}
			return "OPEN", ""
		case "__typename":
			return "Issue", ""
		}
	case "PullRequest":
		p := parent.(*Pull)
		switch f.name {
		case "id":
			return globalID("pull", p.ID), ""
		case "number":
			return p.Number, ""
		case "title":
			return p.Title, ""
		case "body":
			return p.Body, ""
		case "headRefName":
			return p.Head, ""
		case "baseRefName":
			return p.Base, ""
		case "mergeable":
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			rep := s.findRepoByPull(p)
			if rep == nil {
				return "UNKNOWN", ""
			}
			if s.pullConflicts(rep, p) {
				return "CONFLICTING", ""
			}
			return "MERGEABLE", ""
		case "state":
			if p.State == "closed" {
				return "CLOSED", ""
			}
			return "OPEN", ""
		case "merged":
			return p.Merged, ""
		case "files":
			s.store.mu.RLock()
			defer s.store.mu.RUnlock()
			rep := s.findRepoByPull(p)
			if rep == nil {
				return []any{}, "PullRequestFile"
			}
			files := s.pullFiles(rep, p)
			first := f.argInt("first", vars, 10)
			out := make([]any, 0)
			for i, fl := range files {
				if i >= first {
					break
				}
				out = append(out, map[string]any{"path": fl["filename"]})
			}
			return map[string]any{"nodes": out}, "PullRequestFileConnection"
		case "__typename":
			return "PullRequest", ""
		}
	case "PullRequestFileConnection":
		if m, ok := parent.(map[string]any); ok {
			switch f.name {
			case "nodes":
				return m["nodes"], "PullRequestFile"
			case "__typename":
				return "PullRequestFileConnection", ""
			}
		}
	case "PullRequestFile":
		if m, ok := parent.(map[string]any); ok && f.name == "path" {
			return m["path"], ""
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		if m, ok := parent.(map[string]any); ok {
			switch f.name {
			case "issue":
				if is, ok := m["issue"].(*Issue); ok {
					return is, "Issue"
				}
			case "__typename":
				return parentType, ""
			}
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if m, ok := parent.(map[string]any); ok {
			switch f.name {
			case "pullRequest":
				if p, ok := m["pullRequest"].(*Pull); ok {
					return p, "PullRequest"
				}
			case "__typename":
				return parentType, ""
			}
		}
	case "AddCommentPayload":
		if m, ok := parent.(map[string]any); ok {
			switch f.name {
			case "commentEdge":
				return m["commentEdge"], "CommentEdge"
			case "__typename":
				return parentType, ""
			}
		}
	case "CommentEdge":
		if m, ok := parent.(map[string]any); ok {
			switch f.name {
			case "node":
				return m["node"], "Comment"
			case "__typename":
				return parentType, ""
			}
		}
	case "Comment":
		if m, ok := parent.(map[string]any); ok {
			switch f.name {
			case "id":
				return m["id"], ""
			case "body":
				return m["body"], ""
			case "__typename":
				return "Comment", ""
			}
		}
	case "Ref":
		if m, ok := parent.(map[string]any); ok && f.name == "name" {
			return m["name"], ""
		}
	case "IssueConnection":
		// Connections are projected through a wrapper map.
		if m, ok := parent.(map[string]any); ok {
			switch f.name {
			case "nodes":
				return m["nodes"], "Issue"
			case "pageInfo":
				return m["pageInfo"], "PageInfo"
			case "__typename":
				return "IssueConnection", ""
			}
		}
	case "PullRequestConnection":
		if m, ok := parent.(map[string]any); ok {
			switch f.name {
			case "nodes":
				return m["nodes"], "PullRequest"
			case "__typename":
				return "PullRequestConnection", ""
			}
		}
	case "PageInfo":
		if m, ok := parent.(map[string]any); ok {
			switch f.name {
			case "hasNextPage":
				return m["hasNextPage"], ""
			case "endCursor":
				return m["endCursor"], ""
			case "__typename":
				return "PageInfo", ""
			}
		}
	case "__Type":
		if m, ok := parent.(map[string]any); ok {
			switch f.name {
			case "name":
				return m["name"], ""
			case "kind":
				return m["kind"], ""
			case "fields":
				return m["fields"], "__Field"
			case "enumValues":
				return m["enumValues"], "__EnumValue"
			case "__typename":
				return "__Type", ""
			}
		}
	case "__Field":
		if m, ok := parent.(map[string]any); ok && f.name == "name" {
			return m["name"], ""
		}
	case "__EnumValue":
		if m, ok := parent.(map[string]any); ok && f.name == "name" {
			return m["name"], ""
		}
	}
	return nil, ""
}

// repoIssues resolves repository.issues with states, orderBy and cursor
// pagination, returning a connection wrapper.
func (e *gqlExecutor) repoIssues(rep *Repo, f *gqlField) map[string]any {
	vars := e.vars
	e.s.store.mu.RLock()
	defer e.s.store.mu.RUnlock()
	states := map[string]bool{}
	if raw := f.arg("states", vars); raw != nil {
		if arr, ok := raw.([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					states[s] = true
				}
			}
		}
	}
	var filtered []*Issue
	for _, is := range rep.Issues {
		state := "OPEN"
		if is.State == "closed" {
			state = "CLOSED"
		}
		if len(states) > 0 && !states[state] {
			continue
		}
		filtered = append(filtered, is)
	}
	direction := "DESC"
	if raw := f.arg("orderBy", vars); raw != nil {
		if m, ok := raw.(map[string]any); ok {
			if d, ok := m["direction"].(string); ok && d != "" {
				direction = d
			}
		}
	}
	if direction == "ASC" {
		sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Seq < filtered[j].Seq })
	} else {
		sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Seq > filtered[j].Seq })
	}
	// Cursor: base64 of "cursor:<seq>".
	afterSeq := int64(-1)
	if cur := f.argStr("after", vars); cur != "" {
		if raw, err := base64.RawURLEncoding.DecodeString(cur); err == nil {
			parts := strings.Split(string(raw), ":")
			if len(parts) == 2 {
				if n, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
					afterSeq = n
				}
			}
		}
	}
	first := f.argInt("first", vars, 10)
	var page []*Issue
	for _, is := range filtered {
		if direction == "ASC" {
			if is.Seq <= afterSeq {
				continue
			}
		} else {
			if afterSeq >= 0 && is.Seq >= afterSeq {
				continue
			}
		}
		page = append(page, is)
		if len(page) >= first {
			break
		}
	}
	// Has a next page when an unreturned issue remains after the page.
	hasNext := false
	if len(page) > 0 {
		lastSeq := page[len(page)-1].Seq
		for _, is := range filtered {
			if direction == "ASC" {
				if is.Seq > lastSeq {
					hasNext = true
				}
			} else {
				if is.Seq < lastSeq {
					hasNext = true
				}
			}
		}
	}
	var endCursor any
	if len(page) > 0 {
		endCursor = base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.FormatInt(page[len(page)-1].Seq, 10)))
	}
	nodes := make([]any, 0, len(page))
	for _, is := range page {
		nodes = append(nodes, is)
	}
	return map[string]any{
		"nodes":    nodes,
		"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": endCursor},
	}
}

// findRepoByPull locates the repository holding a pull request.
func (s *server) findRepoByPull(p *Pull) *Repo {
	for _, rep := range s.store.order {
		for _, q := range rep.Pulls {
			if q == p {
				return rep
			}
		}
	}
	return nil
}

func (s *server) findRepoByIssue(is *Issue) *Repo {
	for _, rep := range s.store.order {
		for _, q := range rep.Issues {
			if q == is {
				return rep
			}
		}
	}
	return nil
}

// typeInfo returns introspection data for the types the suite queries.
func typeInfo(name string) map[string]any {
	fields := func(names ...string) []any {
		out := make([]any, 0, len(names))
		for _, n := range names {
			out = append(out, map[string]any{"name": n})
		}
		return out
	}
	enumVals := func(names ...string) []any {
		out := make([]any, 0, len(names))
		for _, n := range names {
			out = append(out, map[string]any{"name": n})
		}
		return out
	}
	switch name {
	case "Issue":
		return map[string]any{"name": "Issue", "kind": "OBJECT", "fields": fields("id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "labels", "comments"), "enumValues": nil}
	case "PullRequest":
		return map[string]any{"name": "PullRequest", "kind": "OBJECT", "fields": fields("id", "number", "title", "state", "body", "headRefName", "baseRefName", "mergeable", "merged", "files", "createdAt", "author"), "enumValues": nil}
	case "Repository":
		return map[string]any{"name": "Repository", "kind": "OBJECT", "fields": fields("id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "createdAt", "description", "isPrivate"), "enumValues": nil}
	case "SearchType":
		return map[string]any{"name": "SearchType", "kind": "ENUM", "fields": nil, "enumValues": enumVals("ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT")}
	}
	return nil
}

// ---------------------------------------------------------------- mutations

// executeMutation runs one mutation operation's root field.
func (e *gqlExecutor) executeMutation(op *gqlOp) map[string]any {
	data := map[string]any{}
	for _, f := range op.selection {
		if f.fragName != "" || f.frag != "" {
			e.execFields("Mutation", nil, f, data)
			continue
		}
		val := e.resolveMutation(f)
		if val == nil && len(e.errors) == 0 {
			e.failf("Cannot query field %q on type %q.", f.name, "Mutation")
			data[f.key()] = nil
			continue
		}
		data[f.key()] = e.project(mutationPayloadType(f.name), val, f)
	}
	return data
}

func mutationPayloadType(name string) string {
	switch name {
	case "createIssue":
		return "CreateIssuePayload"
	case "closeIssue":
		return "CloseIssuePayload"
	case "createPullRequest":
		return "CreatePullRequestPayload"
	case "mergePullRequest":
		return "MergePullRequestPayload"
	case "addComment":
		return "AddCommentPayload"
	}
	return ""
}

// resolveMutation executes one mutation root field.
func (e *gqlExecutor) resolveMutation(f *gqlField) any {
	s := e.s
	vars := e.vars
	input, _ := f.arg("input", vars).(map[string]any)
	if input == nil {
		e.failf("input is required")
		return nil
	}
	switch f.name {
	case "createIssue":
		repoID := strAny(input["repositoryId"])
		kind, num, ok := parseGlobalID(repoID)
		if !ok || kind != "repo" {
			e.failf("repository not found")
			return nil
		}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		var rep *Repo
		for _, r := range s.store.order {
			if r.ID == num {
				rep = r
				break
			}
		}
		if rep == nil {
			e.failf("repository not found")
			return nil
		}
		is := &Issue{
			ID:     s.store.nextID,
			Number: rep.NextIssue,
			Title:  strAny(input["title"]),
			Body:   strAny(input["body"]),
			State:  "open",
			Seq:    s.store.tick(),
		}
		s.store.nextID++
		rep.NextIssue++
		rep.Issues = append(rep.Issues, is)
		return map[string]any{"issue": is}
	case "closeIssue":
		issueID := strAny(input["issueId"])
		kind, num, ok := parseGlobalID(issueID)
		if !ok || kind != "issue" {
			e.failf("issue not found")
			return nil
		}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		for _, rep := range s.store.order {
			for _, is := range rep.Issues {
				if is.ID == num {
					is.State = "closed"
					is.ClosedAt = stamp(s.store.tick())
					return map[string]any{"issue": is}
				}
			}
		}
		e.failf("issue not found")
		return nil
	case "createPullRequest":
		repoID := strAny(input["repositoryId"])
		kind, num, ok := parseGlobalID(repoID)
		if !ok || kind != "repo" {
			e.failf("repository not found")
			return nil
		}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		var rep *Repo
		for _, r := range s.store.order {
			if r.ID == num {
				rep = r
				break
			}
		}
		if rep == nil {
			e.failf("repository not found")
			return nil
		}
		head := strAny(input["headRefName"])
		base := strAny(input["baseRefName"])
		if existing := rep.openPullFor(head, base); existing != nil {
			return map[string]any{"pullRequest": existing}
		}
		headSHA := rep.tipOf(head)
		baseSHA := rep.tipOf(base)
		if headSHA == "" || baseSHA == "" {
			e.failf("head or base branch does not exist")
			return nil
		}
		p := &Pull{
			ID:      s.store.nextID,
			Number:  rep.NextPull,
			Title:   strAny(input["title"]),
			Body:    strAny(input["body"]),
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
		return map[string]any{"pullRequest": p}
	case "mergePullRequest":
		prID := strAny(input["pullRequestId"])
		kind, num, ok := parseGlobalID(prID)
		if !ok || kind != "pull" {
			e.failf("pull request not found")
			return nil
		}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		for _, rep := range s.store.order {
			for _, p := range rep.Pulls {
				if p.ID == num {
					if p.State != "open" {
						e.failf("pull request is not open")
						return nil
					}
					if _, merged := s.squashMerge(rep, p); !merged {
						e.failf("merge conflict")
						return nil
					}
					return map[string]any{"pullRequest": p}
				}
			}
		}
		e.failf("pull request not found")
		return nil
	case "addComment":
		subjectID := strAny(input["subjectId"])
		kind, num, ok := parseGlobalID(subjectID)
		if !ok || kind != "issue" {
			e.failf("subject not found")
			return nil
		}
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		for _, rep := range s.store.order {
			for _, is := range rep.Issues {
				if is.ID == num {
					c := &Comment{ID: s.store.nextID, Body: strAny(input["body"]), Seq: s.store.tick()}
					s.store.nextID++
					is.Comments = append(is.Comments, c)
					return map[string]any{"commentEdge": map[string]any{"node": map[string]any{"id": globalID("comment", c.ID), "body": c.Body}}}
				}
			}
		}
		e.failf("subject not found")
		return nil
	}
	return nil
}

func strAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ---------------------------------------------------------------- HTTP entry

func (s *server) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		errJSON(w, 400, "Invalid JSON body")
		return
	}
	// A batch is a JSON array of request objects.
	var batch []json.RawMessage
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &batch); err != nil {
			errJSON(w, 400, "Invalid JSON body")
			return
		}
	} else {
		batch = []json.RawMessage{raw}
	}
	results := make([]map[string]any, 0, len(batch))
	for _, item := range batch {
		results = append(results, s.runGraphQL(item))
	}
	if len(raw) > 0 && raw[0] == '[' {
		jsonOut(w, 200, results, nil)
	} else {
		jsonOut(w, 200, results[0], nil)
	}
}

func (s *server) runGraphQL(raw json.RawMessage) map[string]any {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return map[string]any{"errors": []map[string]any{{"message": "Invalid request"}}}
	}
	if req.Variables == nil {
		req.Variables = map[string]any{}
	}
	// Normalise json.Number values in variables to plain numbers.
	for k, v := range req.Variables {
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				req.Variables[k] = int(i)
			} else if f, err := n.Float64(); err == nil {
				req.Variables[k] = f
			}
		}
	}
	doc := (&gqlParser{toks: lexGQL(req.Query)}).parseDocument()
	if len(doc.ops) == 0 {
		return map[string]any{"errors": []map[string]any{{"message": "No operation found"}}}
	}
	op := doc.ops[0]
	// Apply variable defaults.
	vars := map[string]any{}
	for k, v := range req.Variables {
		vars[k] = v
	}
	for name, def := range op.varDefs {
		if _, ok := vars[name]; !ok {
			vars[name] = resolveValue(def, vars)
		}
	}
	e := &gqlExecutor{s: s, doc: doc, vars: vars}
	var data map[string]any
	if op.kind == "mutation" {
		data = e.executeMutation(op)
	} else {
		data = e.execute(op)
	}
	out := map[string]any{"data": data}
	if len(e.errors) > 0 {
		out["errors"] = e.errors
	}
	return out
}


