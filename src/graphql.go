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

// ---------------------------------------------------------------------------
// A small GraphQL engine covering the document shapes gh and
// github-mcp-server send: operations with variables, aliases, arguments
// (enums, lists, input objects), inline fragments, connections with
// pageInfo, introspection of the types the suite probes, Relay node(id:),
// and operation batching (an array body answers an array of results).
// ---------------------------------------------------------------------------

// ---- lexer ----

type gqlTok struct {
	kind string // name | string | int | float | punct | spread
	val  string
}

func gqlLex(src string) ([]gqlTok, error) {
	var toks []gqlTok
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
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlTok{"spread", "..."})
			i += 3
		case strings.IndexByte("{}()[]:@$!=|", c) >= 0:
			toks = append(toks, gqlTok{"punct", string(c)})
			i++
		case c == '"':
			if i+2 < len(src) && src[i+1] == '"' && src[i+2] == '"' {
				end := strings.Index(src[i+3:], "\"\"\"")
				if end < 0 {
					return nil, fmt.Errorf("unterminated block string")
				}
				toks = append(toks, gqlTok{"string", src[i+3 : i+3+end]})
				i += 3 + end + 3
			} else {
				var sb strings.Builder
				i++
				for i < len(src) && src[i] != '"' {
					if src[i] == '\\' && i+1 < len(src) {
						i++
						switch src[i] {
						case 'n':
							sb.WriteByte('\n')
						case 't':
							sb.WriteByte('\t')
						case 'r':
							sb.WriteByte('\r')
						case 'u':
							if i+4 < len(src) {
								if n, err := strconv.ParseInt(src[i+1:i+5], 16, 32); err == nil {
									sb.WriteRune(rune(n))
									i += 4
								}
							}
						default:
							sb.WriteByte(src[i])
						}
						i++
					} else {
						sb.WriteByte(src[i])
						i++
					}
				}
				i++ // closing quote
				toks = append(toks, gqlTok{"string", sb.String()})
			}
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			toks = append(toks, gqlTok{kind, src[i:j]})
			i = j
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j := i
			for j < len(src) && (src[j] == '_' || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= '0' && src[j] <= '9')) {
				j++
			}
			toks = append(toks, gqlTok{"name", src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", string(c))
		}
	}
	return toks, nil
}

// ---- AST ----

type gqlValue struct {
	kind string // var | string | int | float | bool | enum | null | list | object
	s    string
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlField struct {
	name  string
	alias string
	args  map[string]*gqlValue
	sel   []*gqlField
	// inline fragment
	fragmentType string // non-empty => inline fragment node
	isFragment   bool
}

type gqlOperation struct {
	typ  string // query | mutation
	name string
	sel  []*gqlField
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() (gqlTok, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return gqlTok{}, false
}

func (p *gqlParser) next() (gqlTok, bool) {
	t, ok := p.peek()
	if ok {
		p.pos++
	}
	return t, ok
}

func (p *gqlParser) expectPunct(v string) error {
	t, ok := p.next()
	if !ok || t.kind != "punct" || t.val != v {
		return fmt.Errorf("expected %q", v)
	}
	return nil
}

func gqlParse(src string) ([]*gqlOperation, error) {
	toks, err := gqlLex(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	var ops []*gqlOperation
	for {
		t, ok := p.peek()
		if !ok {
			break
		}
		if t.kind == "punct" && t.val == "{" {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			ops = append(ops, &gqlOperation{typ: "query", sel: sel})
			continue
		}
		if t.kind == "name" && (t.val == "query" || t.val == "mutation" || t.val == "subscription") {
			p.next()
			op := &gqlOperation{typ: t.val}
			if nt, ok := p.peek(); ok && nt.kind == "name" {
				p.next()
				op.name = nt.val
			}
			// variable definitions
			if nt, ok := p.peek(); ok && nt.kind == "punct" && nt.val == "(" {
				if err := p.skipParens(); err != nil {
					return nil, err
				}
			}
			// directives
			for {
				if nt, ok := p.peek(); ok && nt.kind == "punct" && nt.val == "@" {
					p.next()
					p.next() // directive name
					if nt2, ok := p.peek(); ok && nt2.kind == "punct" && nt2.val == "(" {
						if err := p.skipParens(); err != nil {
							return nil, err
						}
					}
					continue
				}
				break
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			op.sel = sel
			ops = append(ops, op)
			continue
		}
		if t.kind == "name" && t.val == "fragment" {
			// named fragments are not used by the suite's documents; skip
			return nil, fmt.Errorf("named fragments are not supported")
		}
		return nil, fmt.Errorf("unexpected token %q", t.val)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("no operation found")
	}
	return ops, nil
}

func (p *gqlParser) skipParens() error {
	depth := 0
	for {
		t, ok := p.next()
		if !ok {
			return fmt.Errorf("unterminated parentheses")
		}
		if t.kind == "punct" && t.val == "(" {
			depth++
		}
		if t.kind == "punct" && t.val == ")" {
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []*gqlField
	for {
		t, ok := p.peek()
		if !ok {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if t.kind == "punct" && t.val == "}" {
			p.next()
			return out, nil
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
}

func (p *gqlParser) parseField() (*gqlField, error) {
	t, ok := p.next()
	if !ok {
		return nil, fmt.Errorf("unexpected end of document")
	}
	if t.kind == "spread" {
		nt, ok := p.next()
		if !ok || nt.kind != "name" {
			return nil, fmt.Errorf("bad fragment spread")
		}
		if nt.val == "on" {
			typeTok, ok := p.next()
			if !ok || typeTok.kind != "name" {
				return nil, fmt.Errorf("bad inline fragment")
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlField{isFragment: true, fragmentType: typeTok.val, sel: sel}, nil
		}
		return nil, fmt.Errorf("named fragment spreads are not supported")
	}
	if t.kind != "name" {
		return nil, fmt.Errorf("expected a field name, got %q", t.val)
	}
	f := &gqlField{name: t.val, args: map[string]*gqlValue{}}
	// alias
	if nt, ok := p.peek(); ok && nt.kind == "punct" && nt.val == ":" {
		p.next()
		nameTok, ok := p.next()
		if !ok || nameTok.kind != "name" {
			return nil, fmt.Errorf("bad alias")
		}
		f.alias = f.name
		f.name = nameTok.val
	}
	// arguments
	if nt, ok := p.peek(); ok && nt.kind == "punct" && nt.val == "(" {
		p.next()
		for {
			nt, ok := p.peek()
			if !ok {
				return nil, fmt.Errorf("unterminated arguments")
			}
			if nt.kind == "punct" && nt.val == ")" {
				p.next()
				break
			}
			argTok, ok := p.next()
			if !ok || argTok.kind != "name" {
				return nil, fmt.Errorf("bad argument name")
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			f.args[argTok.val] = v
		}
	}
	// directives (ignored)
	for {
		if nt, ok := p.peek(); ok && nt.kind == "punct" && nt.val == "@" {
			p.next()
			p.next()
			if nt2, ok := p.peek(); ok && nt2.kind == "punct" && nt2.val == "(" {
				if err := p.skipParens(); err != nil {
					return nil, err
				}
			}
			continue
		}
		break
	}
	// selection set
	if nt, ok := p.peek(); ok && nt.kind == "punct" && nt.val == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.sel = sel
	}
	return f, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t, ok := p.next()
	if !ok {
		return nil, fmt.Errorf("unexpected end in value")
	}
	switch t.kind {
	case "punct":
		switch t.val {
		case "$":
			nameTok, ok := p.next()
			if !ok || nameTok.kind != "name" {
				return nil, fmt.Errorf("bad variable")
			}
			return &gqlValue{kind: "var", s: nameTok.val}, nil
		case "[":
			v := &gqlValue{kind: "list"}
			for {
				nt, ok := p.peek()
				if !ok {
					return nil, fmt.Errorf("unterminated list")
				}
				if nt.kind == "punct" && nt.val == "]" {
					p.next()
					return v, nil
				}
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.list = append(v.list, item)
			}
		case "{":
			v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
			for {
				nt, ok := p.peek()
				if !ok {
					return nil, fmt.Errorf("unterminated object")
				}
				if nt.kind == "punct" && nt.val == "}" {
					p.next()
					return v, nil
				}
				keyTok, ok := p.next()
				if !ok || keyTok.kind != "name" {
					return nil, fmt.Errorf("bad object key")
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.obj[keyTok.val] = item
			}
		}
	case "string":
		return &gqlValue{kind: "string", s: t.val}, nil
	case "int":
		return &gqlValue{kind: "int", s: t.val}, nil
	case "float":
		return &gqlValue{kind: "float", s: t.val}, nil
	case "name":
		switch t.val {
		case "true", "false":
			return &gqlValue{kind: "bool", s: t.val}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", s: t.val}, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %q", t.val)
}

// eval turns a parsed value into plain Go data with variables substituted.
func (v *gqlValue) eval(vars map[string]interface{}) interface{} {
	switch v.kind {
	case "var":
		return vars[v.s]
	case "string", "enum":
		return v.s
	case "int":
		n, _ := strconv.ParseInt(v.s, 10, 64)
		return n
	case "float":
		f, _ := strconv.ParseFloat(v.s, 64)
		return f
	case "bool":
		return v.s == "true"
	case "null":
		return nil
	case "list":
		out := make([]interface{}, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, item.eval(vars))
		}
		return out
	case "object":
		out := map[string]interface{}{}
		for k, item := range v.obj {
			out[k] = item.eval(vars)
		}
		return out
	}
	return nil
}

func argStr(args map[string]interface{}, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argInt(args map[string]interface{}, key string) (int64, bool) {
	switch v := args[key].(type) {
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	}
	return 0, false
}

// ---- schema (field existence drives unknown-field errors + introspection) ----

var gqlSchema = map[string][]string{
	"Query":    {"viewer", "repository", "node", "search", "__type", "__schema", "__typename"},
	"Mutation": {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":     {"id", "login", "name", "email", "url"},
	"Repository": {"id", "databaseId", "name", "nameWithOwner", "owner", "description", "isPrivate",
		"defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues",
		"pullRequest", "pullRequests", "createdAt", "updatedAt", "url"},
	"Ref": {"name", "prefix", "target"},
	"Issue": {"id", "number", "title", "body", "state", "author", "labels", "comments",
		"createdAt", "updatedAt", "closedAt", "repository", "url"},
	"PullRequest": {"id", "number", "title", "body", "state", "merged", "mergeable",
		"headRefName", "baseRefName", "headRef", "baseRef", "files", "commits", "author",
		"repository", "createdAt", "updatedAt", "mergedAt", "mergeCommit", "url"},
	"IssueComment":              {"id", "body", "author", "createdAt", "updatedAt"},
	"Label":                     {"id", "name", "color"},
	"Commit":                    {"id", "oid", "message", "committedDate"},
	"PullRequestFile":           {"path", "additions", "deletions"},
	"IssueConnection":           {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueEdge":                 {"node", "cursor"},
	"PullRequestConnection":     {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestEdge":           {"node", "cursor"},
	"PullRequestFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PageInfo":                  {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"CreateIssuePayload":        {"issue", "clientMutationId"},
	"CloseIssuePayload":         {"issue", "clientMutationId"},
	"CreatePullRequestPayload":  {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":   {"pullRequest", "clientMutationId"},
	"AddCommentPayload":         {"commentEdge", "subject", "clientMutationId"},
	"IssueCommentEdge":          {"node", "cursor"},
	"__Type":                    {"name", "kind", "description", "fields", "enumValues", "possibleTypes", "inputFields", "ofType"},
	"__Field":                   {"name", "description", "type", "args"},
	"__EnumValue":               {"name", "description"},
	"__Schema":                  {"types", "queryType", "mutationType"},
}

var gqlEnumTypes = map[string][]string{
	"SearchType":       {"ISSUE", "REPOSITORY", "USER"},
	"IssueState":       {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":   {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
}

// ---- global ids ----

func gqlRepoID(r *Repo) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("Repository:%d", r.ID)))
}

func gqlIssueID(r *Repo, iss *Issue) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("Issue:%d:%d", r.ID, iss.Number)))
}

func gqlPullID(r *Repo, p *Pull) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("PullRequest:%d:%d", r.ID, p.Number)))
}

func gqlCommentID(c *Comment) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("IssueComment:%d", c.ID)))
}

func gqlDecodeID(id string) (kind string, a, b int64, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		// tolerate url-safe / unpadded encodings a client might hand back
		raw, err = base64.RawURLEncoding.DecodeString(id)
		if err != nil {
			return "", 0, 0, false
		}
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", 0, 0, false
	}
	kind = parts[0]
	a, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	if len(parts) >= 3 {
		b, _ = strconv.ParseInt(parts[2], 10, 64)
	}
	return kind, a, b, true
}

func (s *Store) repoByID(id int64) *Repo {
	for _, r := range s.repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// ---- execution ----

type gqlObj struct {
	typ  string
	repo *Repo
	iss  *Issue
	pull *Pull
	com  *Comment
	// connection payload
	items    []*gqlObj
	hasNext  bool
	endCur   string
	typeName string // for __Type
	// pull-request file payload
	filePath string
	fileAdd  int
	fileDel  int
}

type gqlError struct {
	Message string `json:"message"`
}

type gqlExec struct {
	s      *Server
	vars   map[string]interface{}
	errors []gqlError
}

func (e *gqlExec) fail(format string, args ...interface{}) {
	e.errors = append(e.errors, gqlError{Message: fmt.Sprintf(format, args...)})
}

// execSelection executes a selection set against an object of type typ,
// returning a JSON-ready map.
func (e *gqlExec) execSelection(typ string, obj *gqlObj, sel []*gqlField) map[string]interface{} {
	out := map[string]interface{}{}
	for _, f := range sel {
		if f.isFragment {
			if f.fragmentType == typ || f.fragmentType == "Node" {
				for k, v := range e.execSelection(typ, obj, f.sel) {
					out[k] = v
				}
			}
			continue
		}
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		if f.name == "__typename" {
			out[key] = typ
			continue
		}
		fields, known := gqlSchema[typ]
		if known && !contains(fields, f.name) {
			e.fail("Field '%s' doesn't exist on type '%s'", f.name, typ)
			out[key] = nil
			continue
		}
		args := map[string]interface{}{}
		for name, v := range f.args {
			args[name] = v.eval(e.vars)
		}
		out[key] = e.resolve(typ, obj, f, args)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// resolve returns a JSON-ready value for one field.
func (e *gqlExec) resolve(typ string, obj *gqlObj, f *gqlField, args map[string]interface{}) interface{} {
	s := e.s
	switch typ {
	case "Query":
		switch f.name {
		case "viewer":
			return e.execSelection("User", &gqlObj{typ: "User"}, f.sel)
		case "repository":
			owner, name := argStr(args, "owner"), argStr(args, "name")
			r := s.store.repo(owner, name)
			if r == nil {
				return nil
			}
			return e.execSelection("Repository", &gqlObj{typ: "Repository", repo: r}, f.sel)
		case "node":
			id := argStr(args, "id")
			kind, a, b, ok := gqlDecodeID(id)
			if !ok {
				return nil
			}
			switch kind {
			case "Repository":
				if r := s.store.repoByID(a); r != nil {
					return e.execSelection("Repository", &gqlObj{typ: "Repository", repo: r}, f.sel)
				}
			case "Issue":
				if r := s.store.repoByID(a); r != nil {
					if iss := r.Issues[int(b)]; iss != nil {
						return e.execSelection("Issue", &gqlObj{typ: "Issue", repo: r, iss: iss}, f.sel)
					}
				}
			case "PullRequest":
				if r := s.store.repoByID(a); r != nil {
					if p := r.Pulls[int(b)]; p != nil {
						return e.execSelection("PullRequest", &gqlObj{typ: "PullRequest", repo: r, pull: p}, f.sel)
					}
				}
			}
			return nil
		case "__type":
			name := argStr(args, "name")
			return e.execSelection("__Type", &gqlObj{typ: "__Type", typeName: name}, f.sel)
		case "__schema":
			return e.execSelection("__Schema", &gqlObj{typ: "__Schema"}, f.sel)
		}
	case "Mutation":
		return e.resolveMutation(obj, f, args)
	case "User":
		switch f.name {
		case "id":
			return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("User:%d", viewerID)))
		case "login":
			return viewerLogin
		case "name":
			return viewerName
		case "email":
			return viewerEmail
		}
	case "__Type":
		switch f.name {
		case "name":
			return obj.typeName
		case "kind":
			if _, isEnum := gqlEnumTypes[obj.typeName]; isEnum {
				return "ENUM"
			}
			return "OBJECT"
		case "fields":
			var out []interface{}
			for _, name := range gqlSchema[obj.typeName] {
				out = append(out, e.execSelection("__Field", &gqlObj{typ: "__Field", typeName: name}, f.sel))
			}
			if out == nil {
				out = []interface{}{}
			}
			return out
		case "enumValues":
			var out []interface{}
			for _, name := range gqlEnumTypes[obj.typeName] {
				out = append(out, e.execSelection("__EnumValue", &gqlObj{typ: "__EnumValue", typeName: name}, f.sel))
			}
			if out == nil {
				out = []interface{}{}
			}
			return out
		}
	case "__Field", "__EnumValue":
		if f.name == "name" {
			return obj.typeName
		}
		return nil
	case "Repository":
		return e.resolveRepository(obj, f, args)
	case "Issue":
		return e.resolveIssue(obj, f, args)
	case "PullRequest":
		return e.resolvePull(obj, f, args)
	case "IssueComment":
		switch f.name {
		case "id":
			return gqlCommentID(obj.com)
		case "body":
			return obj.com.Body
		case "createdAt", "updatedAt":
			return formatTime(obj.com.CreatedAt)
		case "author":
			return e.execSelection("User", &gqlObj{typ: "User"}, f.sel)
		}
	case "Ref":
		if f.name == "name" {
			return obj.typeName
		}
		return nil
	case "IssueConnection", "PullRequestConnection", "PullRequestFileConnection":
		switch f.name {
		case "nodes":
			out := []interface{}{}
			for _, item := range obj.items {
				out = append(out, e.execSelection(item.typ, item, f.sel))
			}
			return out
		case "edges":
			out := []interface{}{}
			for _, item := range obj.items {
				edge := map[string]interface{}{}
				for _, ef := range f.sel {
					if ef.name == "node" {
						edge["node"] = e.execSelection(item.typ, item, ef.sel)
					} else if ef.name == "cursor" {
						edge["cursor"] = obj.endCur
					}
				}
				out = append(out, edge)
			}
			return out
		case "pageInfo":
			return e.execSelection("PageInfo", obj, f.sel)
		case "totalCount":
			return len(obj.items)
		}
	case "PageInfo":
		switch f.name {
		case "hasNextPage":
			return obj.hasNext
		case "hasPreviousPage":
			return false
		case "endCursor":
			if obj.endCur == "" {
				return nil
			}
			return obj.endCur
		case "startCursor":
			if obj.endCur == "" {
				return nil
			}
			return obj.endCur
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		if f.name == "issue" && obj.iss != nil {
			return e.execSelection("Issue", obj, f.sel)
		}
		return nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if f.name == "pullRequest" && obj.pull != nil {
			return e.execSelection("PullRequest", obj, f.sel)
		}
		return nil
	case "AddCommentPayload":
		if f.name == "commentEdge" {
			edge := &gqlObj{typ: "IssueCommentEdge", repo: obj.repo, com: obj.com}
			return e.execSelection("IssueCommentEdge", edge, f.sel)
		}
		return nil
	case "IssueCommentEdge":
		if f.name == "node" && obj.com != nil {
			return e.execSelection("IssueComment", obj, f.sel)
		}
		if f.name == "cursor" {
			return gqlCommentID(obj.com)
		}
		return nil
	case "PullRequestFile":
		// items carry their data in typeName as JSON-free fields via iss slot reuse:
		return e.resolvePullFile(obj, f)
	}
	return nil
}

func (e *gqlExec) resolvePullFile(obj *gqlObj, f *gqlField) interface{} {
	switch f.name {
	case "path":
		return obj.filePath
	case "additions":
		return obj.fileAdd
	case "deletions":
		return obj.fileDel
	}
	return nil
}

func (e *gqlExec) resolveRepository(obj *gqlObj, f *gqlField, args map[string]interface{}) interface{} {
	r := obj.repo
	switch f.name {
	case "id":
		return gqlRepoID(r)
	case "databaseId":
		return r.ID
	case "name":
		return r.Name
	case "nameWithOwner":
		return r.FullName()
	case "owner":
		return e.execSelection("User", &gqlObj{typ: "User"}, f.sel)
	case "description":
		return r.Description
	case "isPrivate":
		return r.Private
	case "defaultBranchRef":
		return e.execSelection("Ref", &gqlObj{typ: "Ref", typeName: r.DefaultBranch}, f.sel)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "createdAt", "updatedAt":
		return formatTime(r.CreatedAt)
	case "issue":
		n, _ := argInt(args, "number")
		iss := r.Issues[int(n)]
		if iss == nil {
			return nil
		}
		return e.execSelection("Issue", &gqlObj{typ: "Issue", repo: r, iss: iss}, f.sel)
	case "issues":
		return e.execSelection("IssueConnection", e.issueConnection(r, args), f.sel)
	case "pullRequest":
		n, _ := argInt(args, "number")
		p := r.Pulls[int(n)]
		if p == nil {
			return nil
		}
		return e.execSelection("PullRequest", &gqlObj{typ: "PullRequest", repo: r, pull: p}, f.sel)
	case "pullRequests":
		return e.execSelection("PullRequestConnection", e.pullConnection(r, args), f.sel)
	}
	return nil
}

func issueCursor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", number)))
}

func decodeIssueCursor(cur string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(cur)
	if err != nil {
		return 0, false
	}
	if !strings.HasPrefix(string(raw), "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "cursor:"))
	return n, err == nil
}

// issueConnection builds the filtered, ordered, paginated issue connection.
func (e *gqlExec) issueConnection(r *Repo, args map[string]interface{}) *gqlObj {
	var states map[string]bool
	if raw, ok := args["states"].([]interface{}); ok {
		states = map[string]bool{}
		for _, v := range raw {
			if s, ok := v.(string); ok {
				states[s] = true
			}
		}
	}
	var list []*Issue
	for _, iss := range r.Issues {
		if states != nil {
			want := "OPEN"
			if iss.State == "closed" {
				want = "CLOSED"
			}
			if !states[want] {
				continue
			}
		}
		list = append(list, iss)
	}
	desc := false
	if ob, ok := args["orderBy"].(map[string]interface{}); ok {
		if d, ok := ob["direction"].(string); ok && d == "DESC" {
			desc = true
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if desc {
			if list[i].CreatedAt != list[j].CreatedAt {
				return list[i].CreatedAt > list[j].CreatedAt
			}
			return list[i].Number > list[j].Number
		}
		return list[i].Number < list[j].Number
	})
	return e.paginateIssues(r, list, args)
}

func (e *gqlExec) paginateIssues(r *Repo, list []*Issue, args map[string]interface{}) *gqlObj {
	start := 0
	if after, ok := args["after"].(string); ok && after != "" {
		if n, valid := decodeIssueCursor(after); valid {
			for i, iss := range list {
				if iss.Number == n {
					start = i + 1
					break
				}
			}
		}
	}
	first := 100
	if n, ok := argInt(args, "first"); ok {
		first = int(n)
	}
	end := start + first
	hasNext := false
	if end < len(list) {
		hasNext = true
	} else {
		end = len(list)
	}
	conn := &gqlObj{typ: "IssueConnection", repo: r, hasNext: hasNext}
	for _, iss := range list[start:end] {
		conn.items = append(conn.items, &gqlObj{typ: "Issue", repo: r, iss: iss})
	}
	if len(conn.items) > 0 {
		conn.endCur = issueCursor(list[end-1].Number)
	}
	return conn
}

func (e *gqlExec) pullConnection(r *Repo, args map[string]interface{}) *gqlObj {
	var list []*Pull
	for _, p := range r.Pulls {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	first := 100
	if n, ok := argInt(args, "first"); ok {
		first = int(n)
	}
	hasNext := len(list) > first
	if len(list) > first {
		list = list[:first]
	}
	conn := &gqlObj{typ: "PullRequestConnection", repo: r, hasNext: hasNext}
	for _, p := range list {
		conn.items = append(conn.items, &gqlObj{typ: "PullRequest", repo: r, pull: p})
	}
	return conn
}

func (e *gqlExec) resolveIssue(obj *gqlObj, f *gqlField, args map[string]interface{}) interface{} {
	iss := obj.iss
	switch f.name {
	case "id":
		return gqlIssueID(obj.repo, iss)
	case "number":
		return iss.Number
	case "title":
		return iss.Title
	case "body":
		return iss.Body
	case "state":
		if iss.State == "closed" {
			return "CLOSED"
		}
		return "OPEN"
	case "author":
		return e.execSelection("User", &gqlObj{typ: "User"}, f.sel)
	case "createdAt", "updatedAt":
		return formatTime(iss.CreatedAt)
	case "closedAt":
		if iss.ClosedAt > 0 {
			return formatTime(iss.ClosedAt)
		}
		return nil
	case "repository":
		return e.execSelection("Repository", &gqlObj{typ: "Repository", repo: obj.repo}, f.sel)
	}
	return nil
}

func (e *gqlExec) resolvePull(obj *gqlObj, f *gqlField, args map[string]interface{}) interface{} {
	p := obj.pull
	r := obj.repo
	switch f.name {
	case "id":
		return gqlPullID(r, p)
	case "number":
		return p.Number
	case "title":
		return p.Title
	case "body":
		return p.Body
	case "state":
		if p.Merged {
			return "MERGED"
		}
		if p.State == "closed" {
			return "CLOSED"
		}
		return "OPEN"
	case "merged":
		return p.Merged
	case "mergeable":
		if p.State != "open" {
			return "UNKNOWN"
		}
		return e.s.store.pullMergeable(r, p)
	case "headRefName":
		return p.Head
	case "baseRefName":
		return p.Base
	case "author":
		return e.execSelection("User", &gqlObj{typ: "User"}, f.sel)
	case "createdAt", "updatedAt":
		return formatTime(p.CreatedAt)
	case "mergedAt":
		if p.MergedAt > 0 {
			return formatTime(p.MergedAt)
		}
		return nil
	case "repository":
		return e.execSelection("Repository", &gqlObj{typ: "Repository", repo: r}, f.sel)
	case "files":
		changes := e.s.store.pullFiles(r, p)
		first := 100
		if n, ok := argInt(args, "first"); ok {
			first = int(n)
		}
		conn := &gqlObj{typ: "PullRequestFileConnection", repo: r}
		for i, c := range changes {
			if i >= first {
				conn.hasNext = true
				break
			}
			item := &gqlObj{typ: "PullRequestFile", repo: r, filePath: c.Path, fileAdd: c.Additions, fileDel: c.Deletions}
			conn.items = append(conn.items, item)
		}
		return e.execSelection("PullRequestFileConnection", conn, f.sel)
	}
	return nil
}

func (e *gqlExec) resolveMutation(obj *gqlObj, f *gqlField, args map[string]interface{}) interface{} {
	input, _ := args["input"].(map[string]interface{})
	s := e.s
	switch f.name {
	case "createIssue":
		repoID, _ := input["repositoryId"].(string)
		kind, a, _, ok := gqlDecodeID(repoID)
		if !ok || kind != "Repository" {
			e.fail("repository not found")
			return nil
		}
		r := s.store.repoByID(a)
		if r == nil {
			e.fail("repository not found")
			return nil
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		iss, errMsg := s.createIssue(r, title, body, nil)
		if errMsg != "" {
			e.fail("%s", errMsg)
			return nil
		}
		return e.execSelection("CreateIssuePayload", &gqlObj{typ: "CreateIssuePayload", repo: r, iss: iss}, f.sel)
	case "closeIssue":
		issueID, _ := input["issueId"].(string)
		kind, a, b, ok := gqlDecodeID(issueID)
		if !ok || kind != "Issue" {
			e.fail("issue not found")
			return nil
		}
		r := s.store.repoByID(a)
		if r == nil || r.Issues[int(b)] == nil {
			e.fail("issue not found")
			return nil
		}
		iss := r.Issues[int(b)]
		if iss.State != "closed" {
			iss.State = "closed"
			iss.ClosedAt = s.store.tick()
		}
		return e.execSelection("CloseIssuePayload", &gqlObj{typ: "CloseIssuePayload", repo: r, iss: iss}, f.sel)
	case "createPullRequest":
		repoID, _ := input["repositoryId"].(string)
		kind, a, _, ok := gqlDecodeID(repoID)
		if !ok || kind != "Repository" {
			e.fail("repository not found")
			return nil
		}
		r := s.store.repoByID(a)
		if r == nil {
			e.fail("repository not found")
			return nil
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		head, _ := input["headRefName"].(string)
		base, _ := input["baseRefName"].(string)
		p, errMsg, _ := s.createPull(r, title, body, head, base)
		if errMsg != "" {
			e.fail("%s", errMsg)
			return nil
		}
		return e.execSelection("CreatePullRequestPayload", &gqlObj{typ: "CreatePullRequestPayload", repo: r, pull: p}, f.sel)
	case "mergePullRequest":
		pullID, _ := input["pullRequestId"].(string)
		kind, a, b, ok := gqlDecodeID(pullID)
		if !ok || kind != "PullRequest" {
			e.fail("pull request not found")
			return nil
		}
		r := s.store.repoByID(a)
		if r == nil || r.Pulls[int(b)] == nil {
			e.fail("pull request not found")
			return nil
		}
		p := r.Pulls[int(b)]
		if _, status, msg := s.mergePull(r, p); status != 200 {
			e.fail("%s", msg)
			return nil
		}
		return e.execSelection("MergePullRequestPayload", &gqlObj{typ: "MergePullRequestPayload", repo: r, pull: p}, f.sel)
	case "addComment":
		subjectID, _ := input["subjectId"].(string)
		kind, a, b, ok := gqlDecodeID(subjectID)
		if !ok || kind != "Issue" {
			e.fail("subject not found")
			return nil
		}
		r := s.store.repoByID(a)
		if r == nil || r.Issues[int(b)] == nil {
			e.fail("subject not found")
			return nil
		}
		iss := r.Issues[int(b)]
		body, _ := input["body"].(string)
		c := &Comment{ID: s.store.nextCommentID(), Body: body, CreatedAt: s.store.tick()}
		iss.Comments = append(iss.Comments, c)
		return e.execSelection("AddCommentPayload", &gqlObj{typ: "AddCommentPayload", repo: r, iss: iss, com: c}, f.sel)
	}
	return nil
}

// ---- HTTP entry ----

type gqlRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

func (s *Server) executeGraphQL(single gqlRequest) map[string]interface{} {
	vars := single.Variables
	if vars == nil {
		vars = map[string]interface{}{}
	}
	ops, err := gqlParse(single.Query)
	if err != nil {
		return map[string]interface{}{
			"errors": []gqlError{{Message: err.Error()}},
		}
	}
	op := ops[0]
	exec := &gqlExec{s: s, vars: vars}
	rootType := "Query"
	if op.typ == "mutation" {
		rootType = "Mutation"
	}
	data := exec.execSelection(rootType, &gqlObj{typ: rootType}, op.sel)
	out := map[string]interface{}{"data": data}
	if len(exec.errors) > 0 {
		out["errors"] = exec.errors
	}
	return out
}

func (s *Server) handleGraphQL(w http.ResponseWriter, req *http.Request) {
	bodyBytes, err := readAllBody(req)
	if err != nil {
		s.writeErr(w, 400, "unreadable body")
		return
	}
	trimmed := strings.TrimSpace(string(bodyBytes))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", s.store.requestID())
	if strings.HasPrefix(trimmed, "[") {
		var batch []gqlRequest
		if err := json.Unmarshal(bodyBytes, &batch); err != nil {
			w.WriteHeader(400)
			w.Write([]byte(`{"message":"Problems parsing JSON"}`))
			return
		}
		results := make([]map[string]interface{}, 0, len(batch))
		for _, single := range batch {
			results = append(results, s.executeGraphQL(single))
		}
		data, _ := json.Marshal(results)
		w.Write(data)
		return
	}
	var single gqlRequest
	if err := json.Unmarshal(bodyBytes, &single); err != nil {
		w.WriteHeader(400)
		w.Write([]byte(`{"message":"Problems parsing JSON"}`))
		return
	}
	data, _ := json.Marshal(s.executeGraphQL(single))
	w.Write(data)
}
