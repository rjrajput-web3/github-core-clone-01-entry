// GitForge GraphQL v4: a small parser and executor for the documents the
// suite sends (viewer, repository reads, issue and pull request reads, the
// create/close/merge mutations, __type introspection and Relay node()).
// Faithful port of the Node graphql.ts.
package main

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
)

type valueNode struct {
	kind   string // variable|string|int|float|boolean|null|enum|list|object
	name   string
	str    string
	num    float64
	b      bool
	values []*valueNode
	fields []struct {
		name  string
		value *valueNode
	}
}

type fieldNode struct {
	alias      string
	hasAlias   bool
	name       string
	args       []struct {
		name  string
		value *valueNode
	}
	selections []selectionNode
}

type fragmentNode struct {
	typeCondition string
	hasCondition  bool
	selections    []selectionNode
}

type selectionNode struct {
	isFragment bool
	field      *fieldNode
	fragment   *fragmentNode
}

type operationNode struct {
	typ        string // query | mutation
	name       string
	variables  []string
	selections []selectionNode
}

func tokenize(src string) []string {
	var tokens []string
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+3 <= len(src) && src[i:i+3] == "..." {
			tokens = append(tokens, "...")
			i += 3
			continue
		}
		if strings.IndexByte("{}()[]:!=$@=", c) >= 0 {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					n := src[j+1]
					switch n {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(n)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, strconv.Quote(out.String()))
			i = j + 1
			continue
		}
		j := i
		for j < len(src) {
			ch := src[j]
			if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '.' || ch == '-' {
				j++
			} else {
				break
			}
		}
		if j == i {
			i++
			continue
		}
		tokens = append(tokens, src[i:j])
		i = j
	}
	return tokens
}

type gqlParser struct {
	tokens []string
	pos    int
}

func (p *gqlParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *gqlParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *gqlParser) expect(tok string) error {
	if p.next() != tok {
		return &gqlError{"expected " + tok}
	}
	return nil
}

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

func (p *gqlParser) parseDocument() ([]*operationNode, map[string]*fragmentNode, error) {
	var operations []*operationNode
	fragments := map[string]*fragmentNode{}
	for p.pos < len(p.tokens) {
		tok := p.peek()
		if tok == "query" || tok == "mutation" {
			p.next()
			var name string
			if p.peek() != "" && p.peek() != "(" && p.peek() != "{" {
				name = p.next()
			}
			var variables []string
			if p.peek() == "(" {
				p.next()
				for p.peek() != ")" && p.pos < len(p.tokens) {
					t := p.next()
					if t == "$" {
						variables = append(variables, p.next())
					}
				}
				if err := p.expect(")"); err != nil {
					return nil, nil, err
				}
			}
			sels, err := p.parseSelections()
			if err != nil {
				return nil, nil, err
			}
			operations = append(operations, &operationNode{typ: tok, name: name, variables: variables, selections: sels})
		} else if tok == "fragment" {
			p.next()
			name := p.next()
			if err := p.expect("on"); err != nil {
				return nil, nil, err
			}
			typeCondition := p.next()
			sels, err := p.parseSelections()
			if err != nil {
				return nil, nil, err
			}
			fragments[name] = &fragmentNode{typeCondition: typeCondition, hasCondition: true, selections: sels}
		} else if tok == "{" {
			sels, err := p.parseSelections()
			if err != nil {
				return nil, nil, err
			}
			operations = append(operations, &operationNode{typ: "query", selections: sels})
		} else {
			p.next()
		}
	}
	return operations, fragments, nil
}

func (p *gqlParser) parseSelections() ([]selectionNode, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var out []selectionNode
	for p.peek() != "}" && p.pos < len(p.tokens) {
		if p.peek() == "..." {
			p.next()
			next := p.peek()
			if next == "on" {
				p.next()
				typeCondition := p.next()
				sels, err := p.parseSelections()
				if err != nil {
					return nil, err
				}
				out = append(out, selectionNode{isFragment: true, fragment: &fragmentNode{typeCondition: typeCondition, hasCondition: true, selections: sels}})
			} else {
				p.next()
				out = append(out, selectionNode{isFragment: true, fragment: &fragmentNode{selections: []selectionNode{}}})
			}
			continue
		}
		name := p.next()
		var alias string
		hasAlias := false
		if p.peek() == ":" {
			p.next()
			alias = name
			hasAlias = true
			name = p.next()
		}
		var args []struct {
			name  string
			value *valueNode
		}
		if p.peek() == "(" {
			p.next()
			for p.peek() != ")" && p.pos < len(p.tokens) {
				argName := p.next()
				if err := p.expect(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				args = append(args, struct {
					name  string
					value *valueNode
				}{argName, v})
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
		}
		var selections []selectionNode
		if p.peek() == "{" {
			sels, err := p.parseSelections()
			if err != nil {
				return nil, err
			}
			selections = sels
		}
		out = append(out, selectionNode{field: &fieldNode{alias: alias, hasAlias: hasAlias, name: name, args: args, selections: selections}})
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gqlParser) parseValue() (*valueNode, error) {
	tok := p.next()
	switch {
	case tok == "$":
		return &valueNode{kind: "variable", name: p.next()}, nil
	case tok == "[":
		vn := &valueNode{kind: "list"}
		for p.peek() != "]" && p.pos < len(p.tokens) {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			vn.values = append(vn.values, v)
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		return vn, nil
	case tok == "{":
		vn := &valueNode{kind: "object"}
		for p.peek() != "}" && p.pos < len(p.tokens) {
			name := p.next()
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			vn.fields = append(vn.fields, struct {
				name  string
				value *valueNode
			}{name, v})
		}
		if err := p.expect("}"); err != nil {
			return nil, err
		}
		return vn, nil
	case tok == "true":
		return &valueNode{kind: "boolean", b: true}, nil
	case tok == "false":
		return &valueNode{kind: "boolean", b: false}, nil
	case tok == "null":
		return &valueNode{kind: "null"}, nil
	case strings.HasPrefix(tok, "\""):
		var s string
		if err := unquote(tok, &s); err != nil {
			return nil, err
		}
		return &valueNode{kind: "string", str: s}, nil
	case isIntTok(tok):
		n, _ := strconv.ParseFloat(tok, 64)
		return &valueNode{kind: "int", num: n}, nil
	case isFloatTok(tok):
		n, _ := strconv.ParseFloat(tok, 64)
		return &valueNode{kind: "float", num: n}, nil
	default:
		return &valueNode{kind: "enum", str: tok}, nil
	}
}

func unquote(tok string, out *string) error {
	s, err := strconv.Unquote(tok)
	*out = s
	return err
}

func isIntTok(tok string) bool {
	if tok == "" {
		return false
	}
	i := 0
	if tok[0] == '-' {
		i = 1
	}
	if i >= len(tok) {
		return false
	}
	for ; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

func isFloatTok(tok string) bool {
	if tok == "" {
		return false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 2 {
		return false
	}
	return isIntTok(parts[0]) && parts[1] != "" && isDigits(parts[1])
}

func valueOf(node *valueNode, variables map[string]interface{}) interface{} {
	switch node.kind {
	case "variable":
		if v, ok := variables[node.name]; ok {
			return v
		}
		return nil
	case "string":
		return node.str
	case "int", "float":
		return node.num
	case "boolean":
		return node.b
	case "null":
		return nil
	case "enum":
		return node.str
	case "list":
		var out []interface{}
		for _, v := range node.values {
			out = append(out, valueOf(v, variables))
		}
		return out
	case "object":
		out := map[string]interface{}{}
		for _, f := range node.fields {
			out[f.name] = valueOf(f.value, variables)
		}
		return out
	}
	return nil
}

func argsOf(field *fieldNode, variables map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, a := range field.args {
		out[a.name] = valueOf(a.value, variables)
	}
	return out
}

var typeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "createdAt", "updatedAt", "labels", "user", "comments", "url"},
	"PullRequest": {"id", "number", "title", "body", "state", "merged", "mergedAt", "mergeCommitSha", "headRefName", "baseRefName", "headRef", "baseRef", "mergeable", "files", "createdAt", "updatedAt"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "createdAt", "updatedAt"},
	"User":        {"id", "login", "name", "email"},
	"SearchType":  {},
}

var typeEnumValues = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"OrderDirection":         {"ASC", "DESC"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
}

// nodeValue is one resolved GraphQL object.
type nodeValue struct {
	kind         string
	login        string
	repo         *Repo
	name         string // Ref name
	issue        *Issue
	issues       []*Issue
	endCursor    string
	hasEndCursor bool
	hasNextPage  bool
	pr           *Pull
	prs          []*Pull
	files        []string
	typeName     string
	commentID    string
	commentBody  string
}

func issueGid(repo *Repo, issue *Issue) string { return gid("I", repo.ID, issue.Number) }
func prGid(repo *Repo, pr *Pull) string       { return gid("P", repo.ID, pr.Number) }
func repoGid(repo *Repo) string               { return gid("R", repo.ID) }

func repoByID(s *Store, id int) *Repo {
	for _, repo := range s.Repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func cursorFor(number int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("c:" + itoa(number)))
}

func cursorNumber(cursor interface{}) (int, bool) {
	s, ok := cursor.(string)
	if !ok || s == "" {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "c:") {
		return 0, false
	}
	n, err := strconv.Atoi(text[2:])
	if err != nil {
		return 0, false
	}
	return n, true
}

type executor struct {
	store     *Store
	errors    []map[string]interface{}
	variables map[string]interface{}
}

func (e *executor) unknownField(typeName, field string) {
	e.errors = append(e.errors, map[string]interface{}{"message": "Field '" + field + "' doesn't exist on type '" + typeName + "'"})
}

func (e *executor) resolveSelections(node *nodeValue, selections []selectionNode) map[string]interface{} {
	out := map[string]interface{}{}
	if selections == nil {
		return out
	}
	for _, sel := range selections {
		if sel.isFragment {
			if sel.fragment.hasCondition && sel.fragment.typeCondition != node.kind {
				continue
			}
			for k, v := range e.resolveSelections(node, sel.fragment.selections) {
				out[k] = v
			}
			continue
		}
		key := sel.field.name
		if sel.field.hasAlias {
			key = sel.field.alias
		}
		out[key] = e.resolveField(node, sel.field)
	}
	return out
}

func (e *executor) resolveField(node *nodeValue, field *fieldNode) interface{} {
	args := argsOf(field, e.variables)
	switch node.kind {
	case "Query":
		return e.queryField(field, args)
	case "User":
		if field.name == "login" {
			return node.login
		}
		if field.name == "id" {
			return gid("U", node.login)
		}
		e.unknownField("User", field.name)
		return nil
	case "Repository":
		return e.repositoryField(node, field, args)
	case "Ref":
		if field.name == "name" {
			return node.name
		}
		e.unknownField("Ref", field.name)
		return nil
	case "Issue":
		return e.issueField(node, field)
	case "IssueConnection":
		if field.name == "nodes" {
			var out []interface{}
			for _, issue := range node.issues {
				out = append(out, e.resolveSelections(&nodeValue{kind: "Issue", repo: node.repo, issue: issue}, field.selections))
			}
			if out == nil {
				out = []interface{}{}
			}
			return out
		}
		if field.name == "pageInfo" {
			return e.resolveSelections(&nodeValue{kind: "PageInfo", hasNextPage: node.hasNextPage, endCursor: node.endCursor, hasEndCursor: node.hasEndCursor}, field.selections)
		}
		if field.name == "totalCount" {
			return len(node.issues)
		}
		e.unknownField("IssueConnection", field.name)
		return nil
	case "PageInfo":
		if field.name == "hasNextPage" {
			return node.hasNextPage
		}
		if field.name == "endCursor" {
			if node.hasEndCursor {
				return node.endCursor
			}
			return nil
		}
		e.unknownField("PageInfo", field.name)
		return nil
	case "PullRequest":
		return e.pullRequestField(node, field, args)
	case "PullRequestConnection":
		if field.name == "nodes" {
			var out []interface{}
			for _, pr := range node.prs {
				out = append(out, e.resolveSelections(&nodeValue{kind: "PullRequest", repo: node.repo, pr: pr}, field.selections))
			}
			if out == nil {
				out = []interface{}{}
			}
			return out
		}
		if field.name == "totalCount" {
			return len(node.prs)
		}
		e.unknownField("PullRequestConnection", field.name)
		return nil
	case "PullRequestChangedFileConnection":
		if field.name == "nodes" {
			var out []interface{}
			for _, f := range node.files {
				out = append(out, e.resolveSelections(&nodeValue{kind: "PullRequestChangedFile", name: f}, field.selections))
			}
			if out == nil {
				out = []interface{}{}
			}
			return out
		}
		e.unknownField("PullRequestChangedFileConnection", field.name)
		return nil
	case "PullRequestChangedFile":
		if field.name == "path" {
			return node.name
		}
		e.unknownField("PullRequestChangedFile", field.name)
		return nil
	case "Type":
		if field.name == "fields" {
			fields, ok := typeFields[node.typeName]
			if !ok {
				fields = []string{}
			}
			var out []interface{}
			for _, n := range fields {
				out = append(out, map[string]interface{}{"name": n})
			}
			if out == nil {
				out = []interface{}{}
			}
			return out
		}
		if field.name == "enumValues" {
			vals, ok := typeEnumValues[node.typeName]
			if !ok {
				vals = []string{}
			}
			var out []interface{}
			for _, n := range vals {
				out = append(out, map[string]interface{}{"name": n})
			}
			if out == nil {
				out = []interface{}{}
			}
			return out
		}
		if field.name == "name" {
			return node.typeName
		}
		e.unknownField("__Type", field.name)
		return nil
	case "CommentEdge":
		if field.name == "node" {
			return e.resolveSelections(&nodeValue{kind: "AddCommentPayload", commentID: node.commentID, commentBody: node.commentBody}, field.selections)
		}
		e.unknownField("IssueCommentEdge", field.name)
		return nil
	case "AddCommentPayload":
		if field.name == "commentEdge" {
			return e.resolveSelections(&nodeValue{kind: "CommentEdge", commentID: node.commentID, commentBody: node.commentBody}, field.selections)
		}
		if field.name == "id" {
			return node.commentID
		}
		if field.name == "body" {
			return node.commentBody
		}
		e.unknownField("AddCommentPayload", field.name)
		return nil
	case "CreateIssuePayload":
		if field.name == "issue" {
			return e.resolveSelections(&nodeValue{kind: "Issue", repo: node.repo, issue: node.issue}, field.selections)
		}
		e.unknownField("CreateIssuePayload", field.name)
		return nil
	case "CloseIssuePayload":
		if field.name == "issue" {
			return e.resolveSelections(&nodeValue{kind: "Issue", repo: node.repo, issue: node.issue}, field.selections)
		}
		e.unknownField("CloseIssuePayload", field.name)
		return nil
	case "CreatePullRequestPayload":
		if field.name == "pullRequest" {
			return e.resolveSelections(&nodeValue{kind: "PullRequest", repo: node.repo, pr: node.pr}, field.selections)
		}
		e.unknownField("CreatePullRequestPayload", field.name)
		return nil
	case "MergePullRequestPayload":
		if field.name == "pullRequest" {
			return e.resolveSelections(&nodeValue{kind: "PullRequest", repo: node.repo, pr: node.pr}, field.selections)
		}
		e.unknownField("MergePullRequestPayload", field.name)
		return nil
	}
	return nil
}

func (e *executor) queryField(field *fieldNode, args map[string]interface{}) interface{} {
	if field.name == "viewer" {
		return e.resolveSelections(&nodeValue{kind: "User", login: ViewerLogin}, field.selections)
	}
	if field.name == "repository" {
		owner, _ := args["owner"].(string)
		name, _ := args["name"].(string)
		repo := e.store.getRepo(owner, name)
		if repo == nil {
			return nil
		}
		return e.resolveSelections(&nodeValue{kind: "Repository", repo: repo}, field.selections)
	}
	if field.name == "node" {
		idStr, _ := args["id"].(string)
		decoded := ungid(idStr)
		if decoded == nil {
			return nil
		}
		switch decoded.Kind {
		case "R":
			repo := repoByID(e.store, atoi(decoded.Parts[0]))
			if repo == nil {
				return nil
			}
			return e.resolveSelections(&nodeValue{kind: "Repository", repo: repo}, field.selections)
		case "I":
			repo := repoByID(e.store, atoi(decoded.Parts[0]))
			if repo == nil {
				return nil
			}
			issue := repo.Issues[atoi(decoded.Parts[1])]
			if issue == nil {
				return nil
			}
			return e.resolveSelections(&nodeValue{kind: "Issue", repo: repo, issue: issue}, field.selections)
		case "P":
			repo := repoByID(e.store, atoi(decoded.Parts[0]))
			if repo == nil {
				return nil
			}
			pr := repo.Pulls[atoi(decoded.Parts[1])]
			if pr == nil {
				return nil
			}
			return e.resolveSelections(&nodeValue{kind: "PullRequest", repo: repo, pr: pr}, field.selections)
		}
		return nil
	}
	if field.name == "__type" {
		name, _ := args["name"].(string)
		_, inFields := typeFields[name]
		_, inEnums := typeEnumValues[name]
		if name == "" || (!inFields && !inEnums) {
			return nil
		}
		return e.resolveSelections(&nodeValue{kind: "Type", typeName: name}, field.selections)
	}
	e.unknownField("Query", field.name)
	return nil
}

func (e *executor) repositoryField(node *nodeValue, field *fieldNode, args map[string]interface{}) interface{} {
	repo := node.repo
	switch field.name {
	case "id":
		return repoGid(repo)
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name
	case "owner":
		return e.resolveSelections(&nodeValue{kind: "User", login: repo.Owner}, field.selections)
	case "defaultBranchRef":
		if _, ok := e.store.defaultTip(repo); !ok {
			return nil
		}
		return e.resolveSelections(&nodeValue{kind: "Ref", name: repo.DefaultBranch}, field.selections)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "description":
		return repo.Description
	case "private":
		return repo.Private
	case "issue":
		issue := repo.Issues[int(toFloat(args["number"]))]
		if issue == nil {
			return nil
		}
		return e.resolveSelections(&nodeValue{kind: "Issue", repo: repo, issue: issue}, field.selections)
	case "issues":
		var list []*Issue
		for _, i := range repo.Issues {
			list = append(list, i)
		}
		if states, ok := args["states"].([]interface{}); ok && len(states) > 0 {
			wanted := map[string]bool{}
			for _, st := range states {
				if s, ok := st.(string); ok {
					wanted[strings.ToLower(s)] = true
				}
			}
			var filtered []*Issue
			for _, i := range list {
				if wanted[i.State] {
					filtered = append(filtered, i)
				}
			}
			list = filtered
		}
		desc := false
		if ob, ok := args["orderBy"].(map[string]interface{}); ok {
			if d, ok := ob["direction"].(string); ok && d == "DESC" {
				desc = true
			}
		}
		sort.Slice(list, func(a, b int) bool {
			if desc {
				return list[a].Number > list[b].Number
			}
			return list[a].Number < list[b].Number
		})
		if afterNum, ok := cursorNumber(args["after"]); ok {
			idx := -1
			for i, it := range list {
				if it.Number == afterNum {
					idx = i
					break
				}
			}
			if idx >= 0 {
				list = list[idx+1:]
			}
		}
		first := len(list)
		if f, ok := args["first"].(float64); ok {
			first = int(f)
		}
		if first > len(list) {
			first = len(list)
		}
		if first < 0 {
			first = 0
		}
		page := list[:first]
		hasNext := len(list) > len(page)
		var endCursor string
		hasEndCursor := false
		if len(page) > 0 {
			endCursor = cursorFor(page[len(page)-1].Number)
			hasEndCursor = true
		}
		return e.resolveSelections(&nodeValue{kind: "IssueConnection", repo: repo, issues: page, endCursor: endCursor, hasEndCursor: hasEndCursor, hasNextPage: hasNext}, field.selections)
	case "pullRequests":
		var list []*Pull
		for _, p := range repo.Pulls {
			if p.State == "open" {
				list = append(list, p)
			}
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Number < list[b].Number })
		first := len(list)
		if f, ok := args["first"].(float64); ok {
			first = int(f)
		}
		if first > len(list) {
			first = len(list)
		}
		if first < 0 {
			first = 0
		}
		list = list[:first]
		return e.resolveSelections(&nodeValue{kind: "PullRequestConnection", repo: repo, prs: list}, field.selections)
	case "pullRequest":
		pr := repo.Pulls[int(toFloat(args["number"]))]
		if pr == nil {
			return nil
		}
		return e.resolveSelections(&nodeValue{kind: "PullRequest", repo: repo, pr: pr}, field.selections)
	default:
		e.unknownField("Repository", field.name)
		return nil
	}
}

func toFloat(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func (e *executor) issueField(node *nodeValue, field *fieldNode) interface{} {
	repo, issue := node.repo, node.issue
	switch field.name {
	case "id":
		return issueGid(repo, issue)
	case "number":
		return issue.Number
	case "title":
		return issue.Title
	case "state":
		if issue.State == "open" {
			return "OPEN"
		}
		return "CLOSED"
	case "body":
		return issue.Body
	default:
		e.unknownField("Issue", field.name)
		return nil
	}
}

func (e *executor) pullRequestField(node *nodeValue, field *fieldNode, args map[string]interface{}) interface{} {
	repo, pr := node.repo, node.pr
	switch field.name {
	case "id":
		return prGid(repo, pr)
	case "number":
		return pr.Number
	case "title":
		return pr.Title
	case "body":
		return pr.Body
	case "state":
		if pr.Merged {
			return "MERGED"
		}
		if pr.State == "open" {
			return "OPEN"
		}
		return "CLOSED"
	case "merged":
		return pr.Merged
	case "headRefName":
		return pr.HeadRef
	case "baseRefName":
		return pr.BaseRef
	case "mergeable":
		return e.store.pullMergeable(repo, pr)
	case "files":
		var files []string
		for _, f := range e.store.pullFiles(repo, pr) {
			files = append(files, f.Path)
		}
		first := len(files)
		if f, ok := args["first"].(float64); ok {
			first = int(f)
		}
		if first > len(files) {
			first = len(files)
		}
		if first < 0 {
			first = 0
		}
		files = files[:first]
		return e.resolveSelections(&nodeValue{kind: "PullRequestChangedFileConnection", files: files}, field.selections)
	default:
		e.unknownField("PullRequest", field.name)
		return nil
	}
}

func (e *executor) runMutation(field *fieldNode, args map[string]interface{}) interface{} {
	input, _ := args["input"].(map[string]interface{})
	if input == nil {
		input = map[string]interface{}{}
	}
	if field.name == "createIssue" {
		repoID, _ := input["repositoryId"].(string)
		decoded := ungid(repoID)
		var repo *Repo
		if decoded != nil && decoded.Kind == "R" {
			repo = repoByID(e.store, atoi(decoded.Parts[0]))
		}
		if repo == nil {
			e.errors = append(e.errors, map[string]interface{}{"message": "createIssue: unknown repository"})
			return nil
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		made, errStr := e.store.createIssue(repo, title, body, []string{})
		if errStr != "" {
			e.errors = append(e.errors, map[string]interface{}{"message": "createIssue: " + errStr})
			return nil
		}
		return e.resolveSelections(&nodeValue{kind: "CreateIssuePayload", repo: repo, issue: made}, field.selections)
	}
	if field.name == "closeIssue" {
		issueID, _ := input["issueId"].(string)
		decoded := ungid(issueID)
		var repo *Repo
		var issue *Issue
		if decoded != nil && decoded.Kind == "I" {
			repo = repoByID(e.store, atoi(decoded.Parts[0]))
			if repo != nil {
				issue = repo.Issues[atoi(decoded.Parts[1])]
			}
		}
		if repo == nil || issue == nil {
			e.errors = append(e.errors, map[string]interface{}{"message": "closeIssue: unknown issue"})
			return nil
		}
		issue.State = "closed"
		issue.UpdatedAt = isoNow()
		issue.JSONBuf = nil
		return e.resolveSelections(&nodeValue{kind: "CloseIssuePayload", repo: repo, issue: issue}, field.selections)
	}
	if field.name == "createPullRequest" {
		repoID, _ := input["repositoryId"].(string)
		decoded := ungid(repoID)
		var repo *Repo
		if decoded != nil && decoded.Kind == "R" {
			repo = repoByID(e.store, atoi(decoded.Parts[0]))
		}
		if repo == nil {
			e.errors = append(e.errors, map[string]interface{}{"message": "createPullRequest: unknown repository"})
			return nil
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		head, _ := input["headRefName"].(string)
		base, _ := input["baseRefName"].(string)
		made, errStr := e.store.createPull(repo, title, body, head, base)
		if errStr != "" {
			e.errors = append(e.errors, map[string]interface{}{"message": "createPullRequest: " + errStr})
			return nil
		}
		return e.resolveSelections(&nodeValue{kind: "CreatePullRequestPayload", repo: repo, pr: made}, field.selections)
	}
	if field.name == "mergePullRequest" {
		prID, _ := input["pullRequestId"].(string)
		decoded := ungid(prID)
		var repo *Repo
		var pr *Pull
		if decoded != nil && decoded.Kind == "P" {
			repo = repoByID(e.store, atoi(decoded.Parts[0]))
			if repo != nil {
				pr = repo.Pulls[atoi(decoded.Parts[1])]
			}
		}
		if repo == nil || pr == nil {
			e.errors = append(e.errors, map[string]interface{}{"message": "mergePullRequest: unknown pull request"})
			return nil
		}
		ok, _, reason := e.store.mergePull(repo, pr)
		if !ok {
			e.errors = append(e.errors, map[string]interface{}{"message": "mergePullRequest: " + reason})
			return nil
		}
		return e.resolveSelections(&nodeValue{kind: "MergePullRequestPayload", repo: repo, pr: pr}, field.selections)
	}
	if field.name == "addComment" {
		subjectID, _ := input["subjectId"].(string)
		decoded := ungid(subjectID)
		var repo *Repo
		var issue *Issue
		if decoded != nil && decoded.Kind == "I" {
			repo = repoByID(e.store, atoi(decoded.Parts[0]))
			if repo != nil {
				issue = repo.Issues[atoi(decoded.Parts[1])]
			}
		}
		if repo == nil || issue == nil {
			e.errors = append(e.errors, map[string]interface{}{"message": "addComment: unknown subject"})
			return nil
		}
		body, _ := input["body"].(string)
		comment := &Comment{
			ID: e.store.NextItem, Body: body, User: ViewerLogin,
			CreatedAt: isoNow(), Seq: e.store.NextSeq,
		}
		e.store.NextItem++
		e.store.NextSeq++
		issue.Comments = append(issue.Comments, comment)
		issue.JSONBuf = nil
		nodeID := gid("C", comment.ID)
		return e.resolveSelections(&nodeValue{kind: "AddCommentPayload", commentID: nodeID, commentBody: body}, field.selections)
	}
	e.unknownField("Mutation", field.name)
	return nil
}

type gqlResult struct {
	Status int
	JSON   interface{}
}

func executeGraphQL(s *Store, body interface{}) gqlResult {
	if arr, ok := body.([]interface{}); ok {
		var out []interface{}
		for _, item := range arr {
			out = append(out, executeOne(s, item))
		}
		return gqlResult{200, out}
	}
	return gqlResult{200, executeOne(s, body)}
}

func executeOne(s *Store, body interface{}) map[string]interface{} {
	exec := &executor{store: s}
	m, ok := body.(map[string]interface{})
	if !ok {
		return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": "GraphQL body must be an object with query and variables"}}}
	}
	query, _ := m["query"].(string)
	if query == "" || strings.TrimSpace(query) == "" {
		return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": "GraphQL query must be a non-empty string"}}}
	}
	if vars, ok := m["variables"].(map[string]interface{}); ok {
		exec.variables = vars
	} else {
		exec.variables = map[string]interface{}{}
	}
	p := &gqlParser{tokens: tokenize(query)}
	ops, _, err := p.parseDocument()
	if err != nil {
		return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": "GraphQL parse error: " + err.Error()}}}
	}
	opName, _ := m["operationName"].(string)
	var op *operationNode
	if len(ops) > 0 {
		op = ops[0]
	}
	if opName != "" {
		op = nil
		for _, o := range ops {
			if o.name == opName {
				op = o
				break
			}
		}
	}
	if op == nil {
		return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": "no operation to execute"}}}
	}
	data := map[string]interface{}{}
	if op.typ == "mutation" {
		for _, sel := range op.selections {
			if sel.isFragment {
				continue
			}
			key := sel.field.name
			if sel.field.hasAlias {
				key = sel.field.alias
			}
			data[key] = exec.runMutation(sel.field, argsOf(sel.field, exec.variables))
		}
	} else {
		for k, v := range exec.resolveSelections(&nodeValue{kind: "Query"}, op.selections) {
			data[k] = v
		}
	}
	out := map[string]interface{}{"data": data}
	if len(exec.errors) > 0 {
		var errs []interface{}
		for _, e := range exec.errors {
			errs = append(errs, e)
		}
		out["errors"] = errs
	}
	return out
}
