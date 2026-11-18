// graphql.go — the GraphQL v4 surface at POST /api/graphql: a small
// tokenizer, parser and resolver for the documents gh and github-mcp-server
// send (viewer, repository reads, issue/pull connections, the create/close/
// merge/comment mutations, __type introspection, node(id:) and operation
// batching). Global ids are opaque, stable encodings of the object's kind
// and key — never the plain database id.

package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// global ids

func encodeId(kind string, parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(append([]string{kind}, parts...), ":")))
}

func decodeId(id string) (string, []string, bool) {
	filtered := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			filtered = append(filtered, c)
		}
	}
	for len(filtered)%4 != 0 {
		filtered = append(filtered, '=')
	}
	raw, err := base64.URLEncoding.DecodeString(string(filtered))
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}

func repoGlobalId(repo *RepoRec) string { return encodeId("repo", strconv.Itoa(repo.ID)) }
func issueGlobalId(repo *RepoRec, issue *IssueRec) string {
	return encodeId("issue", strconv.Itoa(repo.ID), strconv.Itoa(issue.Number))
}
func pullGlobalId(repo *RepoRec, pull *PullRec) string {
	return encodeId("pull", strconv.Itoa(repo.ID), strconv.Itoa(pull.Number))
}
func commentGlobalId(repo *RepoRec, issue *IssueRec, commentID int) string {
	return encodeId("comment", strconv.Itoa(repo.ID), strconv.Itoa(issue.Number), strconv.Itoa(commentID))
}

func cursorFor(number int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(number)))
}

var cursorPattern = regexp.MustCompile(`^cursor:(\d+)$`)

func numberFromCursor(cursor string) (int, bool) {
	filtered := make([]byte, 0, len(cursor))
	for i := 0; i < len(cursor); i++ {
		c := cursor[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			filtered = append(filtered, c)
		}
	}
	for len(filtered)%4 != 0 {
		filtered = append(filtered, '=')
	}
	raw, err := base64.URLEncoding.DecodeString(string(filtered))
	if err != nil {
		return 0, false
	}
	match := cursorPattern.FindStringSubmatch(string(raw))
	if match == nil {
		return 0, false
	}
	number, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	return number, true
}

// ---------------------------------------------------------------------------
// tokenizer

type gqlParseError struct{ message string }

func (e *gqlParseError) Error() string { return e.message }

func parseFail(format string, args ...any) {
	panic(&gqlParseError{message: fmt.Sprintf(format, args...)})
}

type gqlToken struct {
	kind string // "name" | "string" | "int" | "float" | "punct" | "eof"
	str  string
	num  float64
}

func isNameStart(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9'
}

func tokenizeGraphQL(source string) []gqlToken {
	tokens := []gqlToken{}
	index := 0
	for index < len(source) {
		ch := source[index]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			index++
			continue
		}
		if ch == 0xEF && strings.HasPrefix(source[index:], "\xef\xbb\xbf") {
			// A byte-order mark is one skipped character.
			index += 3
			continue
		}
		if ch == '#' {
			for index < len(source) && source[index] != '\n' {
				index++
			}
			continue
		}
		if strings.HasPrefix(source[index:], "...") {
			tokens = append(tokens, gqlToken{kind: "punct", str: "..."})
			index += 3
			continue
		}
		if strings.ContainsRune("{}()[]:=@$!|&", rune(ch)) {
			tokens = append(tokens, gqlToken{kind: "punct", str: string(ch)})
			index++
			continue
		}
		if ch == '"' {
			if strings.HasPrefix(source[index:], `"""`) {
				end := strings.Index(source[index+3:], `"""`)
				var value string
				if end < 0 {
					value = source[index+3:]
					index = len(source)
				} else {
					value = source[index+3 : index+3+end]
					index = index + 3 + end + 3
				}
				tokens = append(tokens, gqlToken{kind: "string", str: value})
				continue
			}
			var value strings.Builder
			index++
			for index < len(source) && source[index] != '"' {
				if source[index] == '\\' && index+1 < len(source) {
					esc := source[index+1]
					switch esc {
					case '"':
						value.WriteByte('"')
						index += 2
					case '\\':
						value.WriteByte('\\')
						index += 2
					case '/':
						value.WriteByte('/')
						index += 2
					case 'b':
						value.WriteByte('\b')
						index += 2
					case 'f':
						value.WriteByte('\f')
						index += 2
					case 'n':
						value.WriteByte('\n')
						index += 2
					case 'r':
						value.WriteByte('\r')
						index += 2
					case 't':
						value.WriteByte('\t')
						index += 2
					case 'u':
						code := 0
						if index+6 <= len(source) {
							if parsed, err := strconv.ParseInt(source[index+2:index+6], 16, 32); err == nil {
								code = int(parsed)
							}
						}
						value.WriteRune(rune(code))
						index += 6
					default:
						value.WriteByte(esc)
						index += 2
					}
				} else {
					value.WriteByte(source[index])
					index++
				}
			}
			index++
			tokens = append(tokens, gqlToken{kind: "string", str: value.String()})
			continue
		}
		if ch >= '0' && ch <= '9' || ch == '-' {
			start := index
			for index < len(source) && strings.ContainsRune("0123456789.eE+-", rune(source[index])) {
				index++
			}
			text := source[start:index]
			if strings.ContainsAny(text, ".eE") {
				value, _ := strconv.ParseFloat(text, 64)
				tokens = append(tokens, gqlToken{kind: "float", num: value})
			} else {
				value := jsParseInt(text)
				tokens = append(tokens, gqlToken{kind: "int", num: value})
			}
			continue
		}
		if isNameStart(ch) {
			start := index
			for index < len(source) && isNameChar(source[index]) {
				index++
			}
			tokens = append(tokens, gqlToken{kind: "name", str: source[start:index]})
			continue
		}
		parseFail("unexpected character %s in the document", strconv.Quote(string(ch)))
	}
	tokens = append(tokens, gqlToken{kind: "eof"})
	return tokens
}

// jsParseInt is Number.parseInt(text, 10): leading sign + digits, NaN when
// there are none.
func jsParseInt(text string) float64 {
	index := 0
	negative := false
	if index < len(text) && (text[index] == '+' || text[index] == '-') {
		negative = text[index] == '-'
		index++
	}
	start := index
	for index < len(text) && text[index] >= '0' && text[index] <= '9' {
		index++
	}
	if index == start {
		return nanFloat()
	}
	value, _ := strconv.ParseFloat(text[start:index], 64)
	if negative {
		value = -value
	}
	return value
}

// ---------------------------------------------------------------------------
// parser

type gqlValue struct {
	kind   string // variable|string|int|float|boolean|null|enum|list|object
	str    string
	num    float64
	b      bool
	list   []*gqlValue
	fields []gqlObjectField
}

type gqlObjectField struct {
	name  string
	value *gqlValue
}

type gqlArg struct {
	name  string
	value *gqlValue
}

type gqlFieldNode struct {
	alias        string
	hasAlias     bool
	name         string
	args         []gqlArg
	selectionSet []*gqlSelection
}

type gqlSelection struct {
	kind          string // "field" | "inline" | "spread"
	field         *gqlFieldNode
	typeCondition string
	selectionSet  []*gqlSelection
	spreadName    string
}

type gqlOperation struct {
	opType       string // "query" | "mutation"
	name         string
	hasName      bool
	selectionSet []*gqlSelection
}

type gqlFragment struct {
	typeCondition string
	selectionSet  []*gqlSelection
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string]*gqlFragment
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.pos] }
func (p *gqlParser) next() gqlToken { token := p.tokens[p.pos]; p.pos++; return token }
func (p *gqlParser) atEOF() bool    { return p.pos >= len(p.tokens) || p.tokens[p.pos].kind == "eof" }

func (p *gqlParser) expectPunct(value string) {
	token := p.next()
	if token.kind != "punct" || token.str != value {
		parseFail("expected %q in the document", value)
	}
}

func (p *gqlParser) skipPunct(value string) bool {
	token := p.peek()
	if token.kind == "punct" && token.str == value {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) expectName() string {
	token := p.next()
	if token.kind != "name" {
		parseFail("expected a name in the document")
	}
	return token.str
}

func (p *gqlParser) parseDocument() *gqlDocument {
	document := &gqlDocument{fragments: make(map[string]*gqlFragment)}
	for !p.atEOF() {
		token := p.peek()
		if token.kind == "punct" && token.str == "{" {
			document.operations = append(document.operations, &gqlOperation{opType: "query", selectionSet: p.parseSelectionSet()})
			continue
		}
		if token.kind != "name" {
			parseFail("expected an operation in the document")
		}
		if token.str == "query" || token.str == "mutation" {
			opType := token.str
			p.next()
			operation := &gqlOperation{opType: opType}
			if p.peek().kind == "name" {
				operation.name = p.expectName()
				operation.hasName = true
			}
			if p.peek().kind == "punct" && p.peek().str == "(" {
				p.parseVariableDefinitions()
			}
			p.skipDirectives()
			operation.selectionSet = p.parseSelectionSet()
			document.operations = append(document.operations, operation)
			continue
		}
		if token.str == "fragment" {
			p.next()
			fragName := p.expectName()
			onToken := p.next()
			if onToken.kind != "name" || onToken.str != "on" {
				parseFail("expected \"on\" in a fragment definition")
			}
			typeCondition := p.expectName()
			p.skipDirectives()
			document.fragments[fragName] = &gqlFragment{typeCondition: typeCondition, selectionSet: p.parseSelectionSet()}
			continue
		}
		parseFail("unexpected definition %s in the document", token.str)
	}
	return document
}

func (p *gqlParser) parseVariableDefinitions() {
	p.expectPunct("(")
	for !p.skipPunct(")") {
		p.expectPunct("$")
		p.expectName()
		p.expectPunct(":")
		p.parseType()
		if p.skipPunct("=") {
			p.parseValue()
		}
		p.skipDirectives()
	}
}

func (p *gqlParser) parseType() {
	if p.skipPunct("[") {
		p.parseType()
		p.expectPunct("]")
	} else {
		p.expectName()
	}
	p.skipPunct("!")
}

func (p *gqlParser) skipDirectives() {
	for p.skipPunct("@") {
		p.expectName()
		if p.peek().kind == "punct" && p.peek().str == "(" {
			p.expectPunct("(")
			for !p.skipPunct(")") {
				p.expectName()
				p.expectPunct(":")
				p.parseValue()
			}
		}
	}
}

func (p *gqlParser) parseSelectionSet() []*gqlSelection {
	p.expectPunct("{")
	selections := []*gqlSelection{}
	for !p.skipPunct("}") {
		selections = append(selections, p.parseSelection())
	}
	return selections
}

func (p *gqlParser) parseSelection() *gqlSelection {
	if p.skipPunct("...") {
		token := p.peek()
		if token.kind == "name" && token.str == "on" {
			p.next()
			typeCondition := p.expectName()
			p.skipDirectives()
			return &gqlSelection{kind: "inline", typeCondition: typeCondition, selectionSet: p.parseSelectionSet()}
		}
		name := p.expectName()
		p.skipDirectives()
		return &gqlSelection{kind: "spread", spreadName: name}
	}
	field := &gqlFieldNode{}
	name := p.expectName()
	if p.skipPunct(":") {
		field.alias = name
		field.hasAlias = true
		name = p.expectName()
	}
	field.name = name
	if p.skipPunct("(") {
		for !p.skipPunct(")") {
			argName := p.expectName()
			p.expectPunct(":")
			field.args = append(field.args, gqlArg{name: argName, value: p.parseValue()})
		}
	}
	p.skipDirectives()
	if p.peek().kind == "punct" && p.peek().str == "{" {
		field.selectionSet = p.parseSelectionSet()
	}
	return &gqlSelection{kind: "field", field: field}
}

func (p *gqlParser) parseValue() *gqlValue {
	token := p.next()
	switch token.kind {
	case "string":
		return &gqlValue{kind: "string", str: token.str}
	case "int":
		return &gqlValue{kind: "int", num: token.num}
	case "float":
		return &gqlValue{kind: "float", num: token.num}
	case "name":
		switch token.str {
		case "true":
			return &gqlValue{kind: "boolean", b: true}
		case "false":
			return &gqlValue{kind: "boolean", b: false}
		case "null":
			return &gqlValue{kind: "null"}
		}
		return &gqlValue{kind: "enum", str: token.str}
	case "punct":
		switch token.str {
		case "$":
			return &gqlValue{kind: "variable", str: p.expectName()}
		case "[":
			values := []*gqlValue{}
			for !p.skipPunct("]") {
				values = append(values, p.parseValue())
			}
			return &gqlValue{kind: "list", list: values}
		case "{":
			fields := []gqlObjectField{}
			for !p.skipPunct("}") {
				name := p.expectName()
				p.expectPunct(":")
				fields = append(fields, gqlObjectField{name: name, value: p.parseValue()})
			}
			return &gqlValue{kind: "object", fields: fields}
		}
	}
	parseFail("unexpected value in the document")
	return nil
}

// ---------------------------------------------------------------------------
// schema tables (introspection answers, and the field lists per type)

var gqlObjectFields = map[string][]string{
	"Query":                    {"viewer", "repository", "node"},
	"Mutation":                 {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":                     {"login", "id", "name", "email", "url", "databaseId"},
	"Repository":               {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests", "isPrivate", "description", "url", "createdAt", "updatedAt"},
	"BranchRef":                {"name", "prefix", "target"},
	"Issue":                    {"id", "number", "title", "state", "body", "url", "createdAt", "updatedAt", "author"},
	"IssueConnection":          {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueEdge":                {"node", "cursor"},
	"PullRequest":              {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "url", "createdAt", "updatedAt"},
	"PullRequestConnection":    {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestEdge":          {"node", "cursor"},
	"PullRequestChangedFile":   {"path", "additions", "deletions", "changeType"},
	"PageInfo":                 {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"Comment":                  {"id", "body", "createdAt", "author"},
	"CreateIssuePayload":       {"issue", "clientMutationId"},
	"CloseIssuePayload":        {"issue", "clientMutationId"},
	"CreatePullRequestPayload": {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":  {"pullRequest", "clientMutationId"},
	"AddCommentPayload":        {"commentEdge", "subject", "clientMutationId"},
	"CommentEdge":              {"node", "cursor"},
	"__Type":                   {"name", "kind", "description", "fields", "enumValues", "possibleTypes", "inputFields", "ofType"},
	"__Field":                  {"name", "description", "type", "isDeprecated", "deprecationReason", "args"},
	"__EnumValue":              {"name", "description", "isDeprecated", "deprecationReason"},
	"__Schema":                 {"types", "queryType", "mutationType", "directives"},
}

var gqlEnumValues = map[string][]string{
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "PULL_REQUEST", "COMMIT", "CODE", "WIKI", "PROJECT", "REGISTRY_PACKAGE", "TOPIC", "MARKETPLACE"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
	"State":                  {"OPEN", "CLOSED"},
}

// ---------------------------------------------------------------------------
// execution

type gqlPageInfo struct {
	hasNext     bool
	hasPrevious bool
	startCursor *string
	endCursor   *string
}

type gqlSource struct {
	kind string

	repo  *RepoRec
	issue *IssueRec
	pull  *PullRec

	branchName  string // BranchRef
	commentID   string // Comment
	commentBody string

	issues []*IssueRec  // IssueConnection page items
	pulls  []*PullRec   // PullRequestConnection page items
	info   *gqlPageInfo // connections and PageInfo

	files      []*gqlSource // FileConnection
	totalCount int

	filePath      string // PullRequestChangedFile
	fileAdditions int
	fileDeletions int
	changeType    string

	typeName      string // __Type
	payloadType   string // Payload
	payloadValues map[string]*gqlSource
	edgeNode      *gqlSource // CommentEdge

	raw any // Raw
}

type gqlExecCtx struct {
	variables *JObj
	fragments map[string]*gqlFragment
	errors    []string
}

func (ctx *gqlExecCtx) unknownField(typeName string, field *gqlFieldNode) any {
	ctx.errors = append(ctx.errors, fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field.name, typeName))
	return nil
}

func gqlValueOf(value *gqlValue, ctx *gqlExecCtx) any {
	switch value.kind {
	case "variable":
		if ctx.variables != nil {
			if resolved, present := ctx.variables.Lookup(value.str); present {
				return resolved
			}
		}
		return nil
	case "string", "enum":
		return value.str
	case "int", "float":
		return value.num
	case "boolean":
		return value.b
	case "null":
		return nil
	case "list":
		out := JArr{}
		for _, entry := range value.list {
			out = append(out, gqlValueOf(entry, ctx))
		}
		return out
	case "object":
		out := NewJObj()
		for _, field := range value.fields {
			out.Set(field.name, gqlValueOf(field.value, ctx))
		}
		return out
	}
	return nil
}

func gqlArgsOf(field *gqlFieldNode, ctx *gqlExecCtx) map[string]any {
	out := make(map[string]any, len(field.args))
	for _, arg := range field.args {
		out[arg.name] = gqlValueOf(arg.value, ctx)
	}
	return out
}

// gqlStringArg reads an arg the way `typeof args.x === 'string' ? args.x : ”` does.
func gqlStringArg(args map[string]any, name string) string {
	if text, ok := args[name].(string); ok {
		return text
	}
	return ""
}

// gqlNumber is JS Number(value) for the values arguments can hold.
func gqlNumber(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case string:
		if parsed, ok := jsNumberParse(v); ok {
			return parsed
		}
		return nanFloat()
	case bool:
		if v {
			return 1
		}
		return 0
	case nil:
		return 0
	}
	return nanFloat()
}

// gqlIntKey converts a JS number to a map key when it is integral.
func gqlIntKey(value float64) (int, bool) {
	if value != value || value > 9.007199254740991e15 || value < -9.007199254740991e15 {
		return 0, false
	}
	if value != float64(int64(value)) {
		return 0, false
	}
	return int(value), true
}

// jsStringValue is JS String(value) for argument coercion (states filters).
func jsStringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return jsStringNumber(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case nil:
		return "null"
	case JArr:
		parts := make([]string, len(v))
		for i, entry := range v {
			if entry == nil {
				parts[i] = ""
			} else {
				parts[i] = jsStringValue(entry)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// pageOfNumbers pages an ordered list of item numbers by first/after.
func pageOfNumbers(numbers []int, first any, after any) ([]int, *gqlPageInfo) {
	start := 0
	if text, ok := after.(string); ok && text != "" {
		if afterNumber, found := numberFromCursor(text); found {
			for index, number := range numbers {
				if number == afterNumber {
					start = index + 1
					break
				}
			}
		}
	}
	limit := len(numbers)
	if value, ok := first.(float64); ok && value > 0 {
		limit = int(value)
	}
	end := start + limit
	if end > len(numbers) {
		end = len(numbers)
	}
	page := numbers[start:end]
	info := &gqlPageInfo{
		hasNext:     start+len(page) < len(numbers),
		hasPrevious: start > 0,
	}
	if len(page) > 0 {
		firstCursor := cursorFor(page[0])
		lastCursor := cursorFor(page[len(page)-1])
		info.startCursor = &firstCursor
		info.endCursor = &lastCursor
	}
	return page, info
}

func pullStateString(pull *PullRec) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

// ---------------------------------------------------------------------------
// field resolution

func (ctx *gqlExecCtx) resolveField(source *gqlSource, field *gqlFieldNode) any {
	args := gqlArgsOf(field, ctx)
	if field.name == "__typename" {
		if source.kind == "Raw" {
			return nil
		}
		return source.kind
	}
	switch source.kind {
	case "Query":
		return ctx.resolveQuery(field, args)
	case "Mutation":
		return ctx.resolveMutation(field, args)
	case "User":
		switch field.name {
		case "login":
			return VIEWER.Login
		case "id":
			return encodeId("user", strconv.Itoa(VIEWER.ID))
		case "databaseId":
			return VIEWER.ID
		case "name":
			return VIEWER.Name
		case "email":
			return VIEWER.Email
		case "url":
			return "/" + VIEWER.Login
		}
		return ctx.unknownField("User", field)
	case "Repository":
		return ctx.resolveRepository(source.repo, field, args)
	case "BranchRef":
		if field.name == "name" {
			return source.branchName
		}
		if field.name == "prefix" {
			return "refs/heads/"
		}
		return nil
	case "Issue":
		issue := source.issue
		switch field.name {
		case "id":
			return issueGlobalId(source.repo, issue)
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "body":
			return issue.Body
		case "state":
			if issue.State == "open" {
				return "OPEN"
			}
			return "CLOSED"
		case "url":
			return fmt.Sprintf("/%s/%s/issues/%d", source.repo.Owner, source.repo.Name, issue.Number)
		case "createdAt":
			return iso(issue.CreatedEpoch)
		case "updatedAt":
			return iso(issue.UpdatedEpoch)
		case "author":
			return &gqlSource{kind: "User"}
		}
		return ctx.unknownField("Issue", field)
	case "PullRequest":
		return ctx.resolvePullRequest(source, field, args)
	case "Comment":
		switch field.name {
		case "id":
			return source.commentID
		case "body":
			return source.commentBody
		case "author":
			return &gqlSource{kind: "User"}
		}
		return ctx.unknownField("Comment", field)
	case "IssueConnection":
		switch field.name {
		case "nodes":
			out := []*gqlSource{}
			for _, issue := range source.issues {
				out = append(out, &gqlSource{kind: "Issue", repo: source.repo, issue: issue})
			}
			return out
		case "pageInfo":
			return &gqlSource{kind: "PageInfo", info: source.info}
		case "totalCount":
			return len(source.issues)
		case "edges":
			out := []*gqlSource{}
			for _, issue := range source.issues {
				raw := NewJObj().
					Set("node", &gqlSource{kind: "Issue", repo: source.repo, issue: issue}).
					Set("cursor", cursorFor(issue.Number))
				out = append(out, &gqlSource{kind: "Raw", raw: raw})
			}
			return out
		}
		return ctx.unknownField("IssueConnection", field)
	case "PullRequestConnection":
		switch field.name {
		case "nodes":
			out := []*gqlSource{}
			for _, pull := range source.pulls {
				out = append(out, &gqlSource{kind: "PullRequest", repo: source.repo, pull: pull})
			}
			return out
		case "pageInfo":
			return &gqlSource{kind: "PageInfo", info: source.info}
		case "totalCount":
			return len(source.pulls)
		}
		return ctx.unknownField("PullRequestConnection", field)
	case "FileConnection":
		switch field.name {
		case "nodes":
			return source.files
		case "totalCount":
			return source.totalCount
		case "pageInfo":
			return &gqlSource{kind: "PageInfo", info: &gqlPageInfo{}}
		}
		return ctx.unknownField("FileConnection", field)
	case "PullRequestChangedFile":
		switch field.name {
		case "path":
			return source.filePath
		case "additions":
			return source.fileAdditions
		case "deletions":
			return source.fileDeletions
		case "changeType":
			return source.changeType
		}
		return ctx.unknownField("PullRequestChangedFile", field)
	case "PageInfo":
		switch field.name {
		case "hasNextPage":
			return source.info.hasNext
		case "hasPreviousPage":
			return source.info.hasPrevious
		case "startCursor":
			if source.info.startCursor == nil {
				return nil
			}
			return *source.info.startCursor
		case "endCursor":
			if source.info.endCursor == nil {
				return nil
			}
			return *source.info.endCursor
		}
		return ctx.unknownField("PageInfo", field)
	case "__Type":
		return ctx.resolveType(source.typeName, field)
	case "Payload":
		if value, present := source.payloadValues[field.name]; present {
			if value == nil {
				return nil
			}
			return value
		}
		return ctx.unknownField(source.payloadType, field)
	case "CommentEdge":
		if field.name == "node" {
			return source.edgeNode
		}
		if field.name == "cursor" {
			return cursorFor(0)
		}
		return ctx.unknownField("CommentEdge", field)
	case "Raw":
		if record, ok := source.raw.(*JObj); ok {
			if value, present := record.Lookup(field.name); present {
				return value
			}
		}
		return nil
	}
	return nil
}

func (ctx *gqlExecCtx) resolveQuery(field *gqlFieldNode, args map[string]any) any {
	switch field.name {
	case "viewer":
		return &gqlSource{kind: "User"}
	case "repository":
		repo, ok := theForge.GetRepo(gqlStringArg(args, "owner"), gqlStringArg(args, "name"))
		if !ok {
			return nil
		}
		return &gqlSource{kind: "Repository", repo: repo}
	case "node":
		return ctx.nodeById(gqlStringArg(args, "id"))
	case "__type":
		name := gqlStringArg(args, "name")
		if _, ok := gqlObjectFields[name]; ok {
			return &gqlSource{kind: "__Type", typeName: name}
		}
		if _, ok := gqlEnumValues[name]; ok {
			return &gqlSource{kind: "__Type", typeName: name}
		}
		if name == "String" || name == "Int" || name == "Boolean" || name == "ID" {
			return &gqlSource{kind: "__Type", typeName: name}
		}
		return nil
	case "__schema":
		raw := NewJObj().
			Set("types", JArr{}).
			Set("queryType", NewJObj().Set("name", "Query")).
			Set("mutationType", NewJObj().Set("name", "Mutation")).
			Set("directives", JArr{})
		return &gqlSource{kind: "Raw", raw: raw}
	}
	return ctx.unknownField("Query", field)
}

func (ctx *gqlExecCtx) nodeById(id string) *gqlSource {
	kind, parts, ok := decodeId(id)
	if !ok {
		return nil
	}
	switch kind {
	case "repo":
		if repo := repoByIdPart(parts, 0); repo != nil {
			return &gqlSource{kind: "Repository", repo: repo}
		}
	case "issue":
		repo := repoByIdPart(parts, 0)
		if repo == nil || len(parts) < 2 {
			return nil
		}
		if number, ok := gqlIntKey(gqlNumber(parts[1])); ok {
			if issue, exists := repo.Issues[number]; exists {
				return &gqlSource{kind: "Issue", repo: repo, issue: issue}
			}
		}
	case "pull":
		repo := repoByIdPart(parts, 0)
		if repo == nil || len(parts) < 2 {
			return nil
		}
		if number, ok := gqlIntKey(gqlNumber(parts[1])); ok {
			if pull, exists := repo.Pulls[number]; exists {
				return &gqlSource{kind: "PullRequest", repo: repo, pull: pull}
			}
		}
	case "user":
		return &gqlSource{kind: "User"}
	}
	return nil
}

// repoByIdPart resolves parts[index] as a repository id (JS Number(part)).
func repoByIdPart(parts []string, index int) *RepoRec {
	if index >= len(parts) {
		return nil
	}
	id, ok := gqlIntKey(gqlNumber(parts[index]))
	if !ok {
		return nil
	}
	return theForge.FindRepoByID(id)
}

func (ctx *gqlExecCtx) resolveRepository(repo *RepoRec, field *gqlFieldNode, args map[string]any) any {
	switch field.name {
	case "id":
		return repoGlobalId(repo)
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name
	case "owner":
		return &gqlSource{kind: "User"}
	case "defaultBranchRef":
		if _, ok := repo.Refs.Get("refs/heads/" + repo.DefaultBranch); ok {
			return &gqlSource{kind: "BranchRef", branchName: repo.DefaultBranch}
		}
		return nil
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "isPrivate":
		return repo.IsPrivate
	case "description":
		return repo.Description
	case "url":
		return "/" + repo.Owner + "/" + repo.Name
	case "createdAt":
		return iso(repo.CreatedEpoch)
	case "updatedAt":
		return iso(repo.UpdatedEpoch)
	case "issue":
		if number, ok := gqlIntKey(gqlNumber(args["number"])); ok {
			if issue, exists := repo.Issues[number]; exists {
				return &gqlSource{kind: "Issue", repo: repo, issue: issue}
			}
		}
		return nil
	case "issues":
		numbers := sortedIssueNumbers(repo)
		if rawStates, isList := args["states"].(JArr); isList {
			states := make([]string, 0, len(rawStates))
			for _, entry := range rawStates {
				states = append(states, jsStringValue(entry))
			}
			filtered := []int{}
			for _, number := range numbers {
				state := "CLOSED"
				if repo.Issues[number].State == "open" {
					state = "OPEN"
				}
				for _, candidate := range states {
					if candidate == state {
						filtered = append(filtered, number)
						break
					}
				}
			}
			numbers = filtered
		}
		direction := ""
		if orderBy, isRecord := args["orderBy"].(*JObj); isRecord {
			direction = stringOr(orderBy.Get("direction"), "")
		}
		if direction == "DESC" {
			reverseInts(numbers)
		}
		page, info := pageOfNumbers(numbers, args["first"], args["after"])
		issues := []*IssueRec{}
		for _, number := range page {
			issues = append(issues, repo.Issues[number])
		}
		return &gqlSource{kind: "IssueConnection", repo: repo, issues: issues, info: info}
	case "pullRequest":
		if number, ok := gqlIntKey(gqlNumber(args["number"])); ok {
			if pull, exists := repo.Pulls[number]; exists {
				return &gqlSource{kind: "PullRequest", repo: repo, pull: pull}
			}
		}
		return nil
	case "pullRequests":
		numbers := sortedPullNumbers(repo)
		if rawStates, isList := args["states"].(JArr); isList {
			states := make([]string, 0, len(rawStates))
			for _, entry := range rawStates {
				states = append(states, jsStringValue(entry))
			}
			filtered := []int{}
			for _, number := range numbers {
				state := pullStateString(repo.Pulls[number])
				for _, candidate := range states {
					if candidate == state {
						filtered = append(filtered, number)
						break
					}
				}
			}
			numbers = filtered
		}
		reverseInts(numbers) // pullRequests sort newest first
		page, info := pageOfNumbers(numbers, args["first"], args["after"])
		pulls := []*PullRec{}
		for _, number := range page {
			pulls = append(pulls, repo.Pulls[number])
		}
		return &gqlSource{kind: "PullRequestConnection", repo: repo, pulls: pulls, info: info}
	}
	return ctx.unknownField("Repository", field)
}

func reverseInts(values []int) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}

func (ctx *gqlExecCtx) resolvePullRequest(source *gqlSource, field *gqlFieldNode, args map[string]any) any {
	repo, pull := source.repo, source.pull
	switch field.name {
	case "id":
		return pullGlobalId(repo, pull)
	case "number":
		return pull.Number
	case "title":
		return pull.Title
	case "body":
		return pull.Body
	case "state":
		return pullStateString(pull)
	case "merged":
		return pull.Merged
	case "mergeable":
		if pull.State != "open" {
			return "UNKNOWN"
		}
		if AnalyzePull(repo, pull).Conflict {
			return "CONFLICTING"
		}
		return "MERGEABLE"
	case "headRefName":
		return pull.HeadRef
	case "baseRefName":
		return pull.BaseRef
	case "url":
		return fmt.Sprintf("/%s/%s/pull/%d", repo.Owner, repo.Name, pull.Number)
	case "createdAt":
		return iso(pull.CreatedEpoch)
	case "updatedAt":
		return iso(pull.UpdatedEpoch)
	case "files":
		analysis := AnalyzePull(repo, pull)
		limit := len(analysis.Files)
		if value, ok := args["first"].(float64); ok && value > 0 {
			limit = int(value)
		}
		if limit > len(analysis.Files) {
			limit = len(analysis.Files)
		}
		files := []*gqlSource{}
		for _, file := range analysis.Files[:limit] {
			changeType := "MODIFIED"
			if file.Status == "added" {
				changeType = "ADDED"
			} else if file.Status == "removed" {
				changeType = "DELETED"
			}
			files = append(files, &gqlSource{
				kind:          "PullRequestChangedFile",
				filePath:      file.Filename,
				fileAdditions: file.Additions,
				fileDeletions: file.Deletions,
				changeType:    changeType,
			})
		}
		return &gqlSource{kind: "FileConnection", files: files, totalCount: len(analysis.Files)}
	}
	return ctx.unknownField("PullRequest", field)
}

func (ctx *gqlExecCtx) resolveType(name string, field *gqlFieldNode) any {
	switch field.name {
	case "name":
		return name
	case "kind":
		if _, isEnum := gqlEnumValues[name]; isEnum {
			return "ENUM"
		}
		return "OBJECT"
	case "description":
		return nil
	case "fields":
		fields, ok := gqlObjectFields[name]
		if !ok {
			return nil
		}
		out := []*gqlSource{}
		for _, fieldName := range fields {
			raw := NewJObj().
				Set("name", fieldName).
				Set("description", nil).
				Set("isDeprecated", false).
				Set("deprecationReason", nil)
			out = append(out, &gqlSource{kind: "Raw", raw: raw})
		}
		return out
	case "enumValues":
		values, ok := gqlEnumValues[name]
		if !ok {
			return nil
		}
		out := []*gqlSource{}
		for _, valueName := range values {
			raw := NewJObj().
				Set("name", valueName).
				Set("description", nil).
				Set("isDeprecated", false).
				Set("deprecationReason", nil)
			out = append(out, &gqlSource{kind: "Raw", raw: raw})
		}
		return out
	case "possibleTypes", "inputFields", "ofType":
		return nil
	}
	return ctx.unknownField("__Type", field)
}

// ---------------------------------------------------------------------------
// mutations

func (ctx *gqlExecCtx) resolveMutation(field *gqlFieldNode, args map[string]any) (result any) {
	input, _ := args["input"].(*JObj)
	if input == nil {
		input = NewJObj()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if apiErr, ok := recovered.(*ApiError); ok {
				ctx.errors = append(ctx.errors, apiErr.Message)
				result = nil
				return
			}
			panic(recovered)
		}
	}()
	switch field.name {
	case "createIssue":
		repo := repoByGlobalId(input.Get("repositoryId"))
		if repo == nil {
			panic(ApiErrorf(404, "repository not found"))
		}
		stamp := tickEpochSec()
		issue := &IssueRec{
			ID:           theForge.AllocObjectID(),
			Number:       repo.NextNumber,
			Title:        stringOr(input.Get("title"), ""),
			Body:         stringOr(input.Get("body"), ""),
			State:        "open",
			Labels:       []string{},
			CreatedEpoch: stamp,
			UpdatedEpoch: stamp,
			Comments:     []*CommentRec{},
		}
		repo.NextNumber++
		repo.Issues[issue.Number] = issue
		return &gqlSource{kind: "Payload", payloadType: "CreateIssuePayload", payloadValues: map[string]*gqlSource{
			"issue":            {kind: "Issue", repo: repo, issue: issue},
			"clientMutationId": nil,
		}}
	case "closeIssue":
		repo, issue := issueByGlobalId(input.Get("issueId"))
		if issue == nil {
			panic(ApiErrorf(404, "issue not found"))
		}
		issue.State = "closed"
		issue.UpdatedEpoch = tickEpochSec()
		return &gqlSource{kind: "Payload", payloadType: "CloseIssuePayload", payloadValues: map[string]*gqlSource{
			"issue":            {kind: "Issue", repo: repo, issue: issue},
			"clientMutationId": nil,
		}}
	case "createPullRequest":
		repo := repoByGlobalId(input.Get("repositoryId"))
		if repo == nil {
			panic(ApiErrorf(404, "repository not found"))
		}
		pull := OpenPull(repo,
			stringOr(input.Get("title"), ""),
			stringOr(input.Get("body"), ""),
			stringOr(input.Get("headRefName"), ""),
			stringOr(input.Get("baseRefName"), ""))
		return &gqlSource{kind: "Payload", payloadType: "CreatePullRequestPayload", payloadValues: map[string]*gqlSource{
			"pullRequest":      {kind: "PullRequest", repo: repo, pull: pull},
			"clientMutationId": nil,
		}}
	case "mergePullRequest":
		repo, pull := pullByGlobalId(input.Get("pullRequestId"))
		if pull == nil {
			panic(ApiErrorf(404, "pull request not found"))
		}
		if pull.State == "open" {
			SquashMerge(repo, pull)
		}
		return &gqlSource{kind: "Payload", payloadType: "MergePullRequestPayload", payloadValues: map[string]*gqlSource{
			"pullRequest":      {kind: "PullRequest", repo: repo, pull: pull},
			"clientMutationId": nil,
		}}
	case "addComment":
		repo, issue := issueByGlobalId(input.Get("subjectId"))
		if issue == nil {
			panic(ApiErrorf(404, "subject not found"))
		}
		body := stringOr(input.Get("body"), "")
		comment := &CommentRec{ID: theForge.AllocCommentID(), Body: body, CreatedEpoch: tickEpochSec()}
		issue.Comments = append(issue.Comments, comment)
		node := &gqlSource{kind: "Comment", commentID: commentGlobalId(repo, issue, comment.ID), commentBody: body}
		return &gqlSource{kind: "Payload", payloadType: "AddCommentPayload", payloadValues: map[string]*gqlSource{
			"commentEdge":      {kind: "CommentEdge", edgeNode: node},
			"subject":          {kind: "Issue", repo: repo, issue: issue},
			"clientMutationId": nil,
		}}
	}
	return ctx.unknownField("Mutation", field)
}

func repoByGlobalId(id any) *RepoRec {
	text, ok := id.(string)
	if !ok {
		return nil
	}
	kind, parts, decoded := decodeId(text)
	if !decoded || kind != "repo" {
		return nil
	}
	return repoByIdPart(parts, 0)
}

func issueByGlobalId(id any) (*RepoRec, *IssueRec) {
	text, ok := id.(string)
	if !ok {
		return nil, nil
	}
	kind, parts, decoded := decodeId(text)
	if !decoded || kind != "issue" {
		return nil, nil
	}
	repo := repoByIdPart(parts, 0)
	if repo == nil || len(parts) < 2 {
		return nil, nil
	}
	number, ok := gqlIntKey(gqlNumber(parts[1]))
	if !ok {
		return nil, nil
	}
	issue, exists := repo.Issues[number]
	if !exists {
		return nil, nil
	}
	return repo, issue
}

func pullByGlobalId(id any) (*RepoRec, *PullRec) {
	text, ok := id.(string)
	if !ok {
		return nil, nil
	}
	kind, parts, decoded := decodeId(text)
	if !decoded || kind != "pull" {
		return nil, nil
	}
	repo := repoByIdPart(parts, 0)
	if repo == nil || len(parts) < 2 {
		return nil, nil
	}
	number, ok := gqlIntKey(gqlNumber(parts[1]))
	if !ok {
		return nil, nil
	}
	pull, exists := repo.Pulls[number]
	if !exists {
		return nil, nil
	}
	return repo, pull
}

// ---------------------------------------------------------------------------
// selection execution

func (ctx *gqlExecCtx) executeSelectionSet(source *gqlSource, selections []*gqlSelection) *JObj {
	out := NewJObj()
	for _, selection := range selections {
		switch selection.kind {
		case "field":
			field := selection.field
			key := field.name
			if field.hasAlias {
				key = field.alias
			}
			value := ctx.resolveField(source, field)
			out.Set(key, ctx.completeValue(value, field))
		case "inline":
			if selection.typeCondition == source.kind || selection.typeCondition == "Node" {
				merged := ctx.executeSelectionSet(source, selection.selectionSet)
				for i, key := range merged.keys {
					out.Set(key, merged.vals[i])
				}
			}
		case "spread":
			fragment, ok := ctx.fragments[selection.spreadName]
			if ok && (fragment.typeCondition == source.kind || fragment.typeCondition == "Node") {
				merged := ctx.executeSelectionSet(source, fragment.selectionSet)
				for i, key := range merged.keys {
					out.Set(key, merged.vals[i])
				}
			}
		}
	}
	return out
}

func (ctx *gqlExecCtx) completeValue(value any, field *gqlFieldNode) any {
	switch v := value.(type) {
	case nil:
		return nil
	case []*gqlSource:
		out := make(JArr, 0, len(v))
		for _, entry := range v {
			out = append(out, ctx.completeValue(entry, field))
		}
		return out
	case JArr:
		out := make(JArr, 0, len(v))
		for _, entry := range v {
			out = append(out, ctx.completeValue(entry, field))
		}
		return out
	case *gqlSource:
		if field.selectionSet == nil {
			if v.kind == "Raw" {
				return v.raw
			}
			return nil
		}
		if v.kind == "Raw" {
			if record, ok := v.raw.(*JObj); ok {
				shaped := NewJObj()
				for _, selection := range field.selectionSet {
					if selection.kind != "field" {
						continue
					}
					innerField := selection.field
					key := innerField.name
					if innerField.hasAlias {
						key = innerField.alias
					}
					inner, _ := record.Lookup(innerField.name)
					switch innerValue := inner.(type) {
					case JArr:
						mapped := make(JArr, 0, len(innerValue))
						for _, entry := range innerValue {
							if entrySource, isSource := entry.(*gqlSource); isSource {
								mapped = append(mapped, ctx.completeValue(entrySource, innerField))
							} else {
								mapped = append(mapped, entry)
							}
						}
						shaped.Set(key, mapped)
					case *gqlSource:
						shaped.Set(key, ctx.completeValue(innerValue, innerField))
					default:
						shaped.Set(key, inner)
					}
				}
				return shaped
			}
			return v.raw
		}
		return ctx.executeSelectionSet(v, field.selectionSet)
	}
	return value
}

// ---------------------------------------------------------------------------
// the route

func runGraphQLOperation(document *gqlDocument, operationName *string, variables *JObj) *JObj {
	var operation *gqlOperation
	if operationName != nil {
		for _, candidate := range document.operations {
			if candidate.hasName && candidate.name == *operationName {
				operation = candidate
				break
			}
		}
	}
	if operation == nil && len(document.operations) > 0 {
		operation = document.operations[0]
	}
	if operation == nil {
		return NewJObj().
			Set("data", nil).
			Set("errors", JArr{NewJObj().Set("message", "no operation in the document")})
	}
	ctx := &gqlExecCtx{variables: variables, fragments: document.fragments}
	root := &gqlSource{kind: "Query"}
	if operation.opType == "mutation" {
		root = &gqlSource{kind: "Mutation"}
	}
	data := ctx.executeSelectionSet(root, operation.selectionSet)
	result := NewJObj().Set("data", data)
	if len(ctx.errors) > 0 {
		errors := JArr{}
		for _, message := range ctx.errors {
			errors = append(errors, NewJObj().Set("message", message))
		}
		result.Set("errors", errors)
	}
	return result
}

func runGraphQLEntry(entry any) (result *JObj) {
	record, isRecord := entry.(*JObj)
	if !isRecord {
		return NewJObj().
			Set("data", nil).
			Set("errors", JArr{NewJObj().Set("message", "a query is required")})
	}
	query, isQuery := record.Get("query").(string)
	if !isQuery {
		return NewJObj().
			Set("data", nil).
			Set("errors", JArr{NewJObj().Set("message", "a query is required")})
	}
	variables, _ := record.Get("variables").(*JObj)
	var operationName *string
	if name, isName := record.Get("operationName").(string); isName {
		operationName = &name
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			message := "could not parse the document"
			if err, isErr := recovered.(error); isErr {
				message = err.Error()
			}
			result = NewJObj().
				Set("data", nil).
				Set("errors", JArr{NewJObj().Set("message", message)})
		}
	}()
	parser := &gqlParser{tokens: tokenizeGraphQL(query)}
	document := parser.parseDocument()
	return runGraphQLOperation(document, operationName, variables)
}

// HandleGraphQL is the /api/graphql entry point.
func HandleGraphQL(ctx *ReqCtx) *Reply {
	if !ctx.Authorized {
		return JsonReply(401, NewJObj().Set("message", "Bad credentials"))
	}
	if ctx.Method != "POST" {
		return JsonReply(404, NewJObj().Set("message", "Not Found"))
	}
	if len(strings.TrimSpace(string(ctx.Body))) == 0 {
		return JsonReply(400, NewJObj().Set("errors", JArr{NewJObj().Set("message", "Problems parsing JSON")}))
	}
	parsed, malformed := ctx.bodyJSON()
	if malformed {
		return JsonReply(400, NewJObj().Set("errors", JArr{NewJObj().Set("message", "Problems parsing JSON")}))
	}
	if batch, isBatch := parsed.(JArr); isBatch {
		out := make(JArr, 0, len(batch))
		for _, entry := range batch {
			out = append(out, runGraphQLEntry(entry))
		}
		return JsonReply(200, out)
	}
	return JsonReply(200, runGraphQLEntry(parsed))
}
