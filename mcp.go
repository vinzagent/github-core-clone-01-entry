// mcp.go — the Model Context Protocol surface (mounted at /mcp): JSON-RPC
// 2.0 over POST, with initialize/discover/ping, tools/list and tools/call
// for the six forge tools. Tool payloads reuse the same forge operations as
// REST, so every surface agrees about state.

package main

import (
	"fmt"
	"math"
	"strings"
)

// jsStringNumber is JS String(number) for template interpolation.
func jsStringNumber(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	return jsNumber(f)
}

var mcpServerInfo = NewJObj().
	Set("name", "vinz-forge").
	Set("version", "1.0.0")

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// toolError is an expected tool failure: its message becomes an isError
// tool result, not a JSON-RPC error.
type toolError struct{ message string }

func (e *toolError) Error() string { return e.message }

func toolErrorf(format string, args ...any) *toolError {
	return &toolError{message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// summaries (the MCP shapes, which differ from the REST ones)

func mcpRepoSummary(repo *RepoRec) *JObj {
	return NewJObj().
		Set("id", repo.ID).
		Set("name", repo.Name).
		Set("full_name", repo.Owner+"/"+repo.Name).
		Set("owner", NewJObj().
			Set("login", repo.Owner).
			Set("id", VIEWER.ID).
			Set("type", "User")).
		Set("private", repo.IsPrivate).
		Set("description", repo.Description).
		Set("default_branch", repo.DefaultBranch).
		Set("html_url", "/"+repo.Owner+"/"+repo.Name)
}

func mcpIssueSummary(repo *RepoRec, issue *IssueRec) *JObj {
	labels := JArr{}
	for _, name := range issue.Labels {
		labels = append(labels, NewJObj().Set("name", name))
	}
	return NewJObj().
		Set("id", issue.ID).
		Set("number", issue.Number).
		Set("title", issue.Title).
		Set("body", issue.Body).
		Set("state", issue.State).
		Set("labels", labels).
		Set("html_url", fmt.Sprintf("/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number))
}

func mcpPullSummary(repo *RepoRec, pull *PullRec) *JObj {
	headSha, _ := repo.Refs.Get("refs/heads/" + pull.HeadRef)
	baseSha, _ := repo.Refs.Get("refs/heads/" + pull.BaseRef)
	var mergeCommitSha any
	if pull.MergeCommitSha != nil {
		mergeCommitSha = *pull.MergeCommitSha
	}
	return NewJObj().
		Set("id", pull.ID).
		Set("number", pull.Number).
		Set("title", pull.Title).
		Set("body", pull.Body).
		Set("state", pull.State).
		Set("merged", pull.Merged).
		Set("merge_commit_sha", mergeCommitSha).
		Set("head", NewJObj().Set("ref", pull.HeadRef).Set("sha", headSha)).
		Set("base", NewJObj().Set("ref", pull.BaseRef).Set("sha", baseSha)).
		Set("html_url", fmt.Sprintf("/%s/%s/pull/%d", repo.Owner, repo.Name, pull.Number))
}

// ---------------------------------------------------------------------------
// tools

// mcpNeedRepo resolves args.owner / args.repo (non-strings read as "").
func mcpNeedRepo(args *JObj) *RepoRec {
	owner := stringOr(args.Get("owner"), "")
	name := stringOr(args.Get("repo"), "")
	repo, ok := theForge.GetRepo(owner, name)
	if !ok {
		panic(toolErrorf("repository %s/%s not found", owner, name))
	}
	return repo
}

// mcpNeedString reads a required string argument inside a tool run.
func mcpNeedString(args *JObj, field string) string {
	value := stringOr(args.Get(field), "")
	if value == "" {
		panic(toolErrorf("missing required argument: %s", field))
	}
	return value
}

type mcpTool struct {
	name        string
	description string
	properties  *JObj
	required    []string
	run         func(args *JObj) any
}

func stringProp(description string) *JObj {
	return NewJObj().Set("type", "string").Set("description", description)
}

// convertApiError runs fn, translating an ApiError panic into a ToolError
// panic (the TS tools wrap openPull/squashMerge exactly this way).
func convertApiError(fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if apiErr, ok := recovered.(*ApiError); ok {
				panic(toolErrorf("%s", apiErr.Message))
			}
			panic(recovered)
		}
	}()
	fn()
}

var mcpTools = []mcpTool{
	{
		name:        "create_repo",
		description: "Create a new repository owned by the authenticated user.",
		properties: NewJObj().
			Set("name", stringProp("The repository name.")).
			Set("description", stringProp("A short description of the repository.")).
			Set("private", NewJObj().Set("type", "boolean").Set("description", "Whether the repository is private.")),
		required: []string{"name"},
		run: func(args *JObj) any {
			name := mcpNeedString(args, "name")
			if existing, ok := theForge.GetRepo(VIEWER.Login, name); ok {
				return mcpRepoSummary(existing)
			}
			isPrivate := args.Get("private") == true
			repo := theForge.CreateRepo(name, isPrivate, stringOr(args.Get("description"), ""), "main")
			return mcpRepoSummary(repo)
		},
	},
	{
		name:        "get_repo",
		description: "Read one repository by owner and name.",
		properties: NewJObj().
			Set("owner", stringProp("The repository owner login.")).
			Set("repo", stringProp("The repository name.")),
		required: []string{"owner", "repo"},
		run: func(args *JObj) any {
			return mcpRepoSummary(mcpNeedRepo(args))
		},
	},
	{
		name:        "create_issue",
		description: "Open a new issue in a repository.",
		properties: NewJObj().
			Set("owner", stringProp("The repository owner login.")).
			Set("repo", stringProp("The repository name.")).
			Set("title", stringProp("The issue title.")).
			Set("body", stringProp("The issue body.")),
		required: []string{"owner", "repo", "title"},
		run: func(args *JObj) any {
			repo := mcpNeedRepo(args)
			title := mcpNeedString(args, "title")
			stamp := tickEpochSec()
			issue := &IssueRec{
				ID:           theForge.AllocObjectID(),
				Number:       repo.NextNumber,
				Title:        title,
				Body:         stringOr(args.Get("body"), ""),
				State:        "open",
				Labels:       []string{},
				CreatedEpoch: stamp,
				UpdatedEpoch: stamp,
				Comments:     []*CommentRec{},
			}
			repo.NextNumber++
			repo.Issues[issue.Number] = issue
			return mcpIssueSummary(repo, issue)
		},
	},
	{
		name:        "list_issues",
		description: "List the issues of a repository, optionally filtered by state (open, closed or all).",
		properties: NewJObj().
			Set("owner", stringProp("The repository owner login.")).
			Set("repo", stringProp("The repository name.")).
			Set("state", stringProp("open, closed or all (default open).")),
		required: []string{"owner", "repo"},
		run: func(args *JObj) any {
			repo := mcpNeedRepo(args)
			state := stringOr(args.Get("state"), "open")
			out := JArr{}
			for _, number := range sortedIssueNumbers(repo) {
				issue := repo.Issues[number]
				if state == "all" || issue.State == state {
					out = append(out, mcpIssueSummary(repo, issue))
				}
			}
			return out
		},
	},
	{
		name:        "create_pull_request",
		description: "Open a pull request from a head branch into a base branch.",
		properties: NewJObj().
			Set("owner", stringProp("The repository owner login.")).
			Set("repo", stringProp("The repository name.")).
			Set("title", stringProp("The pull request title.")).
			Set("body", stringProp("The pull request body.")).
			Set("head", stringProp("The head branch name.")).
			Set("base", stringProp("The base branch name.")),
		required: []string{"owner", "repo", "title", "head", "base"},
		run: func(args *JObj) any {
			repo := mcpNeedRepo(args)
			var pull *PullRec
			convertApiError(func() {
				pull = OpenPull(repo,
					mcpNeedString(args, "title"),
					stringOr(args.Get("body"), ""),
					mcpNeedString(args, "head"),
					mcpNeedString(args, "base"))
			})
			return mcpPullSummary(repo, pull)
		},
	},
	{
		name:        "merge_pull_request",
		description: "Squash-merge an open pull request into its base branch.",
		properties: NewJObj().
			Set("owner", stringProp("The repository owner login.")).
			Set("repo", stringProp("The repository name.")).
			Set("pull_number", NewJObj().Set("type", "number").Set("description", "The pull request number.")),
		required: []string{"owner", "repo", "pull_number"},
		run: func(args *JObj) any {
			repo := mcpNeedRepo(args)
			raw := args.Get("pull_number")
			number, _ := raw.(float64)
			if _, isNumber := raw.(float64); !isNumber {
				// JS: Number(value) — strings parse, everything else is NaN.
				if text, isText := raw.(string); isText {
					parsed, ok := jsNumberParse(text)
					if ok {
						number = parsed
					} else {
						number = math.NaN()
					}
				} else if raw == true {
					number = 1
				} else {
					number = math.NaN()
				}
			}
			var pull *PullRec
			if !math.IsNaN(number) && number == math.Trunc(number) && math.Abs(number) < 1e15 {
				pull = repo.Pulls[int(number)]
			}
			if pull == nil {
				panic(toolErrorf("pull request #%s not found", jsStringNumber(number)))
			}
			convertApiError(func() {
				SquashMerge(repo, pull)
			})
			return mcpPullSummary(repo, pull)
		},
	},
}

// toolsListResult is built once: byte-identical across calls.
var toolsListResult = buildToolsListResult()

func buildToolsListResult() *JObj {
	tools := JArr{}
	for _, tool := range mcpTools {
		required := JArr{}
		for _, field := range tool.required {
			required = append(required, field)
		}
		tools = append(tools, NewJObj().
			Set("name", tool.name).
			Set("description", tool.description).
			Set("inputSchema", NewJObj().
				Set("type", "object").
				Set("properties", tool.properties).
				Set("required", required).
				Set("additionalProperties", false)))
	}
	return NewJObj().Set("tools", tools)
}

// ---------------------------------------------------------------------------
// JSON-RPC plumbing

func rpcResult(id any, result any) *JObj {
	return NewJObj().
		Set("jsonrpc", "2.0").
		Set("id", id).
		Set("result", result)
}

func rpcError(id any, code int, message string) *JObj {
	return NewJObj().
		Set("jsonrpc", "2.0").
		Set("id", id).
		Set("error", NewJObj().Set("code", code).Set("message", message))
}

// rpcFailure carries a JSON-RPC error out of callTool as a panic value.
type rpcFailure struct {
	code    int
	message string
}

func (e rpcFailure) Error() string { return e.message }

func callTool(params *JObj) (result *JObj) {
	name := ""
	args := NewJObj()
	if params != nil {
		name = stringOr(params.Get("name"), "")
		if rawArgs, ok := params.Get("arguments").(*JObj); ok {
			args = rawArgs
		}
	}
	var tool *mcpTool
	for i := range mcpTools {
		if mcpTools[i].name == name {
			tool = &mcpTools[i]
			break
		}
	}
	if tool == nil {
		display := name
		if display == "" {
			display = "(none)"
		}
		panic(rpcFailure{code: -32602, message: "unknown tool: " + display})
	}
	for _, field := range tool.required {
		value, present := args.Lookup(field)
		if !present || value == nil || value == "" {
			return NewJObj().
				Set("content", JArr{NewJObj().Set("type", "text").Set("text", "missing required argument: "+field)}).
				Set("isError", true)
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if toolErr, ok := recovered.(*toolError); ok {
				result = NewJObj().
					Set("content", JArr{NewJObj().Set("type", "text").Set("text", toolErr.message)}).
					Set("isError", true)
				return
			}
			panic(recovered)
		}
	}()
	payload := tool.run(args)
	return NewJObj().
		Set("content", JArr{NewJObj().Set("type", "text").Set("text", string(marshalJSON(payload)))}).
		Set("structuredContent", payload).
		Set("isError", false)
}

// dispatchRpc runs one parsed JSON-RPC message. It returns (payload, hasPayload):
// an unknown method on a notification produces no payload.
func dispatchRpc(message *JObj) (*JObj, bool) {
	id, hasID := message.Lookup("id")
	method := stringOr(message.Get("method"), "")
	params, _ := message.Get("params").(*JObj)
	switch method {
	case "server/discover":
		versions := JArr{}
		for _, version := range mcpSupportedVersions {
			versions = append(versions, version)
		}
		return rpcResult(id, NewJObj().
			Set("resultType", "complete").
			Set("supportedVersions", versions).
			Set("capabilities", NewJObj().Set("tools", NewJObj())).
			Set("serverInfo", mcpServerInfo).
			Set("_meta", NewJObj().Set("io.modelcontextprotocol/serverInfo", mcpServerInfo))), true
	case "initialize":
		offered := ""
		if params != nil {
			offered = stringOr(params.Get("protocolVersion"), "")
		}
		version := "2025-11-25"
		for _, supported := range mcpSupportedVersions {
			if offered == supported {
				version = offered
				break
			}
		}
		return rpcResult(id, NewJObj().
			Set("protocolVersion", version).
			Set("capabilities", NewJObj().Set("tools", NewJObj())).
			Set("serverInfo", mcpServerInfo)), true
	case "ping":
		return rpcResult(id, NewJObj()), true
	case "tools/list":
		return rpcResult(id, toolsListResult), true
	case "tools/call":
		result, failure := func() (res *JObj, failure *rpcFailure) {
			defer func() {
				if recovered := recover(); recovered != nil {
					if f, ok := recovered.(rpcFailure); ok {
						failure = &f
						return
					}
					panic(recovered)
				}
			}()
			res = callTool(params)
			return res, nil
		}()
		if failure != nil {
			return rpcError(id, failure.code, failure.message), true
		}
		return rpcResult(id, result), true
	default:
		if !hasID {
			return nil, false
		}
		return rpcError(id, -32601, "method not found: "+method), true
	}
}

// HandleMcp is the /mcp entry point.
func HandleMcp(ctx *ReqCtx) *Reply {
	if ctx.Method == "DELETE" {
		return JsonReply(200, NewJObj())
	}
	if ctx.Method != "POST" {
		return JsonReply(405, rpcError(nil, -32601, "method not allowed"))
	}
	if !ctx.Authorized {
		return JsonReply(401, rpcError(nil, -32001, "unauthorized"))
	}
	// The TS parses the raw body with JSON.parse: an empty body is a parse
	// error there, unlike the lenient bodyJSON used elsewhere.
	if len(strings.TrimSpace(string(ctx.Body))) == 0 {
		return JsonReply(400, rpcError(nil, -32700, "Parse error"))
	}
	parsed, malformed := ctx.bodyJSON()
	if malformed {
		return JsonReply(400, rpcError(nil, -32700, "Parse error"))
	}
	if batch, isBatch := parsed.(JArr); isBatch {
		outcomes := JArr{}
		for _, entry := range batch {
			message, isRecord := entry.(*JObj)
			if !isRecord {
				continue
			}
			if payload, has := dispatchRpc(message); has {
				outcomes = append(outcomes, payload)
			}
		}
		return JsonReply(200, outcomes)
	}
	message, isRecord := parsed.(*JObj)
	if !isRecord {
		return JsonReply(400, rpcError(nil, -32600, "Invalid Request"))
	}
	payload, has := dispatchRpc(message)
	if !has {
		return &Reply{Status: 202, ContentType: "application/json; charset=utf-8", Body: []byte{}, Headers: map[string]string{}}
	}
	return JsonReply(200, payload)
}
