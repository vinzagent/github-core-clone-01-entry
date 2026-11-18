// rest.go — the GitHub REST API subset (mounted at /api/v3): the root and
// meta documents, the viewer, repositories, contents, commits, branches,
// git refs, labels, issues, comments and pull requests.

package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const apiRoot = "/api/v3"

// ---------------------------------------------------------------------------
// shared JSON shapes

// nodeId is GitHub's legacy base64 node id: base64 of "<kind><id>".
func nodeId(kind string, id int) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + strconv.Itoa(id)))
}

func userJson(ctx *ReqCtx) *JObj {
	return NewJObj().
		Set("login", VIEWER.Login).
		Set("id", VIEWER.ID).
		Set("node_id", nodeId("04:User", VIEWER.ID)).
		Set("avatar_url", "").
		Set("gravatar_id", "").
		Set("url", ctx.Origin+apiRoot+"/users/"+VIEWER.Login).
		Set("html_url", ctx.Origin+"/"+VIEWER.Login).
		Set("type", VIEWER.Type).
		Set("site_admin", false)
}

func repoJson(ctx *ReqCtx, repo *RepoRec) *JObj {
	base := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name
	host := ctx.Origin
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimPrefix(host, "https://")
	openIssues := 0
	for _, number := range sortedIssueNumbers(repo) {
		if repo.Issues[number].State == "open" {
			openIssues++
		}
	}
	license := NewJObj().
		Set("key", "mit").
		Set("name", "MIT License").
		Set("spdx_id", "MIT").
		Set("url", nil).
		Set("node_id", "MDc6TGljZW5zZW1pdA==")
	return NewJObj().
		Set("id", repo.ID).
		Set("node_id", nodeId("04:Repository", repo.ID)).
		Set("name", repo.Name).
		Set("full_name", repo.Owner+"/"+repo.Name).
		Set("owner", userJson(ctx)).
		Set("private", repo.IsPrivate).
		Set("html_url", ctx.Origin+"/"+repo.Owner+"/"+repo.Name).
		Set("description", repo.Description).
		Set("fork", false).
		Set("url", base).
		Set("created_at", iso(repo.CreatedEpoch)).
		Set("updated_at", iso(repo.UpdatedEpoch)).
		Set("pushed_at", iso(repo.UpdatedEpoch)).
		Set("git_url", "git://"+host+"/"+repo.Owner+"/"+repo.Name+".git").
		Set("clone_url", ctx.Origin+"/"+repo.Owner+"/"+repo.Name+".git").
		Set("homepage", "").
		Set("size", 1).
		Set("stargazers_count", 0).
		Set("watchers_count", 0).
		Set("language", nil).
		Set("has_issues", true).
		Set("has_projects", true).
		Set("has_downloads", true).
		Set("has_wiki", true).
		Set("has_pages", false).
		Set("forks_count", 0).
		Set("open_issues_count", openIssues).
		Set("license", license).
		Set("allow_forking", true).
		Set("is_template", false).
		Set("topics", JArr{}).
		Set("visibility", map[bool]string{true: "private", false: "public"}[repo.IsPrivate]).
		Set("forks", 0).
		Set("open_issues", openIssues).
		Set("watchers", 0).
		Set("default_branch", repo.DefaultBranch)
}

// utf16Len is the JS string length (UTF-16 code units).
func utf16Len(text string) int {
	length := 0
	for _, ch := range text {
		length++
		if ch > 0xFFFF {
			length++
		}
	}
	return length
}

func labelJson(ctx *ReqCtx, repo *RepoRec, name string) *JObj {
	label, hasLabel := repo.Labels.Get(name)
	// GitHub label ids derive from the name: a stable int32 hash, Math.abs'd.
	hash := int32(7)
	for _, ch := range name {
		unit := ch
		if ch > 0xFFFF {
			unit = 0xD800 + ((ch - 0x10000) >> 10)
		}
		hash = hash*31 + unit
	}
	id := int64(hash)
	if id < 0 {
		id = -id
	}
	color := "ededed"
	var description any
	if hasLabel {
		color = label.Color
		if label.Description != nil {
			description = *label.Description
		}
	}
	return NewJObj().
		Set("id", id).
		Set("node_id", nodeId("05:Label", repo.ID*1000+utf16Len(name))).
		Set("url", fmt.Sprintf("%s%s/repos/%s/%s/labels/%s", ctx.Origin, apiRoot, repo.Owner, repo.Name, encodeURIComponent(name))).
		Set("name", name).
		Set("color", color).
		Set("default", false).
		Set("description", description)
}

func issueJson(ctx *ReqCtx, repo *RepoRec, issue *IssueRec) *JObj {
	base := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name
	labels := JArr{}
	for _, name := range issue.Labels {
		labels = append(labels, labelJson(ctx, repo, name))
	}
	var closedAt any
	if issue.State == "closed" {
		closedAt = iso(issue.UpdatedEpoch)
	}
	return NewJObj().
		Set("id", issue.ID).
		Set("node_id", nodeId("05:Issue", issue.ID)).
		Set("url", fmt.Sprintf("%s/issues/%d", base, issue.Number)).
		Set("repository_url", base).
		Set("labels_url", fmt.Sprintf("%s/issues/%d/labels{/name}", base, issue.Number)).
		Set("comments_url", fmt.Sprintf("%s/issues/%d/comments", base, issue.Number)).
		Set("events_url", fmt.Sprintf("%s/issues/%d/events", base, issue.Number)).
		Set("html_url", fmt.Sprintf("%s/%s/%s/issues/%d", ctx.Origin, repo.Owner, repo.Name, issue.Number)).
		Set("number", issue.Number).
		Set("state", issue.State).
		Set("title", issue.Title).
		Set("body", issue.Body).
		Set("user", userJson(ctx)).
		Set("labels", labels).
		Set("assignee", nil).
		Set("assignees", JArr{}).
		Set("locked", false).
		Set("comments", len(issue.Comments)).
		Set("closed_at", closedAt).
		Set("created_at", iso(issue.CreatedEpoch)).
		Set("updated_at", iso(issue.UpdatedEpoch))
}

func commentJson(ctx *ReqCtx, repo *RepoRec, issue *IssueRec, comment *CommentRec) *JObj {
	return NewJObj().
		Set("id", comment.ID).
		Set("node_id", nodeId("05:IssueComment", comment.ID)).
		Set("url", fmt.Sprintf("%s%s/repos/%s/%s/issues/comments/%d", ctx.Origin, apiRoot, repo.Owner, repo.Name, comment.ID)).
		Set("html_url", fmt.Sprintf("%s/%s/%s/issues/%d#issuecomment-%d", ctx.Origin, repo.Owner, repo.Name, issue.Number, comment.ID)).
		Set("body", comment.Body).
		Set("user", userJson(ctx)).
		Set("created_at", iso(comment.CreatedEpoch)).
		Set("updated_at", iso(comment.CreatedEpoch))
}

func refSideJson(ctx *ReqCtx, repo *RepoRec, ref string) *JObj {
	tip, _ := repo.Refs.Get("refs/heads/" + ref)
	return NewJObj().
		Set("label", repo.Owner+":"+ref).
		Set("ref", ref).
		Set("sha", tip).
		Set("user", userJson(ctx)).
		Set("repo", repoJson(ctx, repo))
}

func pullJson(ctx *ReqCtx, repo *RepoRec, pull *PullRec) *JObj {
	base := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name
	var closedAt any
	if pull.State == "closed" {
		closedAt = iso(pull.UpdatedEpoch)
	}
	var mergedAt any
	if pull.Merged && pull.MergedEpoch != nil {
		mergedAt = iso(*pull.MergedEpoch)
	}
	var mergeCommitSha any
	if pull.MergeCommitSha != nil {
		mergeCommitSha = *pull.MergeCommitSha
	}
	return NewJObj().
		Set("id", pull.ID).
		Set("node_id", nodeId("05:PullRequest", pull.ID)).
		Set("url", fmt.Sprintf("%s/pulls/%d", base, pull.Number)).
		Set("html_url", fmt.Sprintf("%s/%s/%s/pull/%d", ctx.Origin, repo.Owner, repo.Name, pull.Number)).
		Set("diff_url", fmt.Sprintf("%s/%s/%s/pull/%d.diff", ctx.Origin, repo.Owner, repo.Name, pull.Number)).
		Set("patch_url", fmt.Sprintf("%s/%s/%s/pull/%d.patch", ctx.Origin, repo.Owner, repo.Name, pull.Number)).
		Set("number", pull.Number).
		Set("state", pull.State).
		Set("locked", false).
		Set("title", pull.Title).
		Set("body", pull.Body).
		Set("user", userJson(ctx)).
		Set("created_at", iso(pull.CreatedEpoch)).
		Set("updated_at", iso(pull.UpdatedEpoch)).
		Set("closed_at", closedAt).
		Set("merged_at", mergedAt).
		Set("merge_commit_sha", mergeCommitSha).
		Set("merged", pull.Merged).
		Set("head", refSideJson(ctx, repo, pull.HeadRef)).
		Set("base", refSideJson(ctx, repo, pull.BaseRef))
}

var identPattern = regexp.MustCompile(`^(.*) <(.*)> (\d+) [+-]\d{4}$`)

func identParts(ident string) *JObj {
	match := identPattern.FindStringSubmatch(ident)
	if match == nil {
		return NewJObj().Set("name", ident).Set("email", "").Set("date", isoOf(0))
	}
	epoch, _ := strconv.ParseInt(match[3], 10, 64)
	return NewJObj().Set("name", match[1]).Set("email", match[2]).Set("date", isoOf(epoch))
}

func commitJson(ctx *ReqCtx, repo *RepoRec, sha string) *JObj {
	object, ok := repo.Store.Get(sha)
	if !ok || object.Type != "commit" {
		panic(NotFound())
	}
	view := parseCommit(object.Data)
	base := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name
	message := view.Message
	if strings.HasSuffix(message, "\n") {
		message = message[:len(message)-1]
	}
	parents := JArr{}
	for _, parent := range view.Parents {
		parents = append(parents, NewJObj().
			Set("sha", parent).
			Set("url", base+"/commits/"+parent).
			Set("html_url", fmt.Sprintf("%s/%s/%s/commit/%s", ctx.Origin, repo.Owner, repo.Name, parent)))
	}
	return NewJObj().
		Set("sha", sha).
		Set("node_id", nodeId("06:Commit", repo.ID)).
		Set("commit", NewJObj().
			Set("author", identParts(view.Author)).
			Set("committer", identParts(view.Committer)).
			Set("message", message).
			Set("tree", NewJObj().
				Set("sha", view.Tree).
				Set("url", base+"/git/trees/"+view.Tree)).
			Set("url", base+"/git/commits/"+sha).
			Set("comment_count", 0)).
		Set("url", base+"/commits/"+sha).
		Set("html_url", fmt.Sprintf("%s/%s/%s/commit/%s", ctx.Origin, repo.Owner, repo.Name, sha)).
		Set("comments_url", base+"/commits/"+sha+"/comments").
		Set("author", userJson(ctx)).
		Set("committer", userJson(ctx)).
		Set("parents", parents)
}

// ---------------------------------------------------------------------------
// pagination

// pageRange slices [start, end) GitHub-style and records the Link header on
// the context (attachLink copies it onto the reply).
func pageRange(ctx *ReqCtx, total int, path string) (int, int) {
	perPageRaw := "30"
	if values, present := ctx.Query["per_page"]; present && len(values) > 0 {
		perPageRaw = values[0]
	}
	perPage := 30
	if value, ok := jsNumberParse(perPageRaw); ok && value > 0 {
		if value >= 100 {
			perPage = 100
		} else {
			perPage = int(value)
		}
	}
	pageRaw := "1"
	if values, present := ctx.Query["page"]; present && len(values) > 0 {
		pageRaw = values[0]
	}
	page := 1
	if value, ok := jsNumberParse(pageRaw); ok && value > 0 {
		if value > 1e12 {
			page = 1 << 40
		} else {
			page = int(value)
		}
	}
	start := (page - 1) * perPage
	end := start + perPage
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	lastPage := (total + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	url := func(target int) string {
		return fmt.Sprintf("%s%s?per_page=%d&page=%d", ctx.Origin, path, perPage, target)
	}
	links := []string{}
	if page < lastPage {
		links = append(links, fmt.Sprintf("<%s>; rel=\"next\"", url(page+1)), fmt.Sprintf("<%s>; rel=\"last\"", url(lastPage)))
	}
	if page > 1 {
		links = append(links, fmt.Sprintf("<%s>; rel=\"first\"", url(1)), fmt.Sprintf("<%s>; rel=\"prev\"", url(page-1)))
	}
	if len(links) > 0 {
		ctx.linkHeader = strings.Join(links, ", ")
	}
	return start, end
}

// attachLink copies a Link header computed by pageRange onto the reply.
func attachLink(ctx *ReqCtx, reply *Reply) {
	if ctx.linkHeader != "" {
		reply.Headers["link"] = ctx.linkHeader
	}
}

// ---------------------------------------------------------------------------
// root and meta

func rootJson(ctx *ReqCtx) *JObj {
	root := ctx.Origin + apiRoot
	return NewJObj().
		Set("current_user_url", root+"/user").
		Set("current_user_authorizations_html_url", ctx.Origin+"/settings/connections/applications{/client_id}").
		Set("authorizations_url", root+"/authorizations").
		Set("code_search_url", root+"/search/code?q={query}{&page,per_page,sort,order}").
		Set("commit_search_url", root+"/search/commits?q={query}{&page,per_page,sort,order}").
		Set("emails_url", root+"/user/emails").
		Set("emojis_url", root+"/emojis").
		Set("events_url", root+"/events").
		Set("feeds_url", root+"/feeds").
		Set("followers_url", root+"/user/followers").
		Set("following_url", root+"/user/following{/target}").
		Set("gists_url", root+"/gists{/gist_id}").
		Set("hub_url", root+"/hub").
		Set("issue_search_url", root+"/search/issues?q={query}{&page,per_page,sort,order}").
		Set("issues_url", root+"/issues").
		Set("keys_url", root+"/user/keys").
		Set("label_search_url", root+"/search/labels?q={query}{&repository_id,page,per_page}").
		Set("notifications_url", root+"/notifications").
		Set("organization_url", root+"/orgs/{org}").
		Set("organization_repositories_url", root+"/orgs/{org}/repos{?type,page,per_page,sort}").
		Set("organization_teams_url", root+"/orgs/{org}/teams").
		Set("public_gists_url", root+"/gists/public").
		Set("rate_limit_url", root+"/rate_limit").
		Set("repository_url", root+"/repos/{owner}/{repo}").
		Set("repository_search_url", root+"/search/repositories?q={query}{&page,per_page,sort,order}").
		Set("current_user_repositories_url", root+"/user/repos{?type,page,per_page,sort}").
		Set("starred_url", root+"/user/starred{/owner}{/repo}").
		Set("user_url", root+"/users/{user}").
		Set("user_organizations_url", root+"/users/{user}/orgs").
		Set("user_repositories_url", root+"/users/{user}/repos{?type,page,per_page,sort}").
		Set("user_search_url", root+"/search/users?q={query}{&page,per_page,sort,order}")
}

func metaJson(ctx *ReqCtx) *JObj {
	root := ctx.Origin + apiRoot
	return NewJObj().
		Set("installed_version", "1.0.0").
		Set("hooks_url", root+"/hooks").
		Set("issue_search_url", root+"/search/issues").
		Set("repository_search_url", root+"/search/repositories").
		Set("code_search_url", root+"/search/code").
		Set("commit_search_url", root+"/search/commits").
		Set("user_search_url", root+"/search/users").
		Set("verifiable_password_authentication", false).
		Set("ssh_key_fingerprints", NewJObj()).
		Set("ssh_keys", JArr{}).
		Set("api_url", root+"/")
}

// ---------------------------------------------------------------------------
// helpers

func requireRepo(owner string, name string) *RepoRec {
	repo, ok := theForge.GetRepo(owner, name)
	if !ok {
		panic(NotFound())
	}
	return repo
}

// bodyString is the TS stringField: the field when it is a string.
func bodyString(body *JObj, field string) (string, bool) {
	value, ok := body.Get(field).(string)
	return value, ok
}

// queryParam reads a query parameter with URLSearchParams.get semantics:
// (value, present) — present distinguishes "?ref=" from no ref at all.
func queryParam(ctx *ReqCtx, name string) (string, bool) {
	values, present := ctx.Query[name]
	if !present || len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// ---------------------------------------------------------------------------
// the router

// HandleRest is the REST router. Mirrors the TS handleRest: ApiErrors become
// message replies here (so the request-id header still gets added by the
// dispatcher); anything else propagates.
func HandleRest(ctx *ReqCtx) (reply *Reply) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if apiErr, ok := recovered.(*ApiError); ok {
				reply = MessageReply(apiErr.Status, apiErr.Message)
				return
			}
			panic(recovered)
		}
	}()
	return routeRest(ctx)
}

func routeRest(ctx *ReqCtx) *Reply {
	if !ctx.Authorized {
		return MessageReply(401, "Bad credentials")
	}
	segments := pathSegments(ctx.Path, apiRoot)
	method := ctx.Method

	if len(segments) == 0 {
		if method == "GET" {
			reply := JsonReply(200, rootJson(ctx))
			reply.Headers["x-oauth-scopes"] = "repo, read:org"
			reply.Headers["x-accepted-oauth-scopes"] = "repo, read:org"
			return reply
		}
		panic(NotFound())
	}
	if segments[0] == "meta" && len(segments) == 1 {
		if method == "GET" {
			return JsonReply(200, metaJson(ctx))
		}
		panic(NotFound())
	}
	if segments[0] == "user" && len(segments) == 1 {
		if method == "GET" {
			return JsonReply(200, userJson(ctx).
				Set("name", VIEWER.Name).
				Set("email", VIEWER.Email).
				Set("plan", NewJObj().
					Set("name", "forge").
					Set("space", 0).
					Set("private_repos", 0).
					Set("collaborators", 0)))
		}
		panic(NotFound())
	}
	if segments[0] == "user" && len(segments) == 2 && segments[1] == "repos" {
		if method == "GET" {
			repos := theForge.ViewerRepos()
			start, end := pageRange(ctx, len(repos), apiRoot+"/user/repos")
			out := JArr{}
			for _, repo := range repos[start:end] {
				out = append(out, repoJson(ctx, repo))
			}
			reply := JsonReply(200, out)
			attachLink(ctx, reply)
			return reply
		}
		if method == "POST" {
			// A malformed JSON body is a 400 before anything else.
			if _, malformed := ctx.bodyJSON(); malformed && len(ctx.Body) > 0 {
				return MessageReply(400, "Problems parsing JSON")
			}
			return createRepo(ctx)
		}
		panic(NotFound())
	}
	if segments[0] == "repos" && len(segments) >= 3 {
		owner, name := segments[1], segments[2]
		rest := segments[3:]
		if len(rest) == 0 {
			switch method {
			case "GET":
				return JsonReply(200, repoJson(ctx, requireRepo(owner, name)))
			case "PATCH":
				return patchRepo(ctx, requireRepo(owner, name))
			case "DELETE":
				if !theForge.DeleteRepo(owner, name) {
					panic(NotFound())
				}
				return &Reply{Status: 204, ContentType: "application/json; charset=utf-8", Body: []byte{}, Headers: map[string]string{}}
			}
			panic(NotFound())
		}
		repo := requireRepo(owner, name)
		return routeRepo(ctx, repo, rest)
	}
	// A malformed JSON body on a body-carrying route still answers 400.
	if method == "POST" || method == "PUT" || method == "PATCH" {
		if _, malformed := ctx.bodyJSON(); malformed && len(ctx.Body) > 0 {
			return MessageReply(400, "Problems parsing JSON")
		}
	}
	panic(NotFound())
}

func createRepo(ctx *ReqCtx) *Reply {
	body, _ := ctx.bodyObject()
	if body == nil {
		panic(ApiErrorf(400, "Problems parsing JSON"))
	}
	name, _ := bodyString(body, "name")
	if name == "" {
		panic(ApiErrorf(422, "Validation Failed: name is required"))
	}
	if _, exists := theForge.GetRepo(VIEWER.Login, name); exists {
		panic(ApiErrorf(422, "Validation Failed: name already exists on this account"))
	}
	isPrivate := body.Get("private") == true
	description, _ := bodyString(body, "description")
	defaultBranch := "main"
	if value, ok := bodyString(body, "default_branch"); ok {
		defaultBranch = value
	}
	repo := theForge.CreateRepo(name, isPrivate, description, defaultBranch)
	return JsonReply(201, repoJson(ctx, repo))
}

func patchRepo(ctx *ReqCtx, repo *RepoRec) *Reply {
	body, _ := ctx.bodyObject()
	if body == nil {
		panic(ApiErrorf(400, "Problems parsing JSON"))
	}
	if description, ok := bodyString(body, "description"); ok {
		repo.Description = description
	}
	if isPrivate, ok := body.Get("private").(bool); ok {
		repo.IsPrivate = isPrivate
	}
	if defaultBranch, ok := bodyString(body, "default_branch"); ok && defaultBranch != "" {
		repo.DefaultBranch = defaultBranch
	}
	repo.UpdatedEpoch = tickEpochSec()
	return JsonReply(200, repoJson(ctx, repo))
}

// ---------------------------------------------------------------------------
// repository sub-routes

func routeRepo(ctx *ReqCtx, repo *RepoRec, rest []string) *Reply {
	method := ctx.Method
	head := rest[0]
	switch head {
	case "issues":
		return routeIssues(ctx, repo, rest)
	case "labels":
		return routeLabels(ctx, repo, rest)
	case "pulls":
		return routePulls(ctx, repo, rest)
	case "branches":
		if len(rest) == 1 && method == "GET" {
			return branchesList(ctx, repo)
		}
		if len(rest) >= 2 && method == "GET" {
			return branchGet(ctx, repo, strings.Join(rest[1:], "/"))
		}
	case "commits":
		if len(rest) == 1 && method == "GET" {
			return commitsList(ctx, repo)
		}
		if len(rest) == 2 && method == "GET" {
			return JsonReply(200, commitJson(ctx, repo, strings.ToLower(rest[1])))
		}
	case "contents":
		path := strings.Join(rest[1:], "/")
		switch method {
		case "GET":
			return contentsGet(ctx, repo, path)
		case "PUT":
			return contentsPut(ctx, repo, path)
		case "DELETE":
			return contentsDelete(ctx, repo, path)
		}
	case "git":
		if len(rest) >= 2 && rest[1] == "refs" {
			if len(rest) == 2 {
				if method == "POST" {
					return createRef(ctx, repo)
				}
				if method == "GET" {
					return refsList(ctx, repo)
				}
			}
			if len(rest) >= 3 && method == "GET" {
				return getRef(ctx, repo, strings.Join(rest[1:], "/"))
			}
		}
	}
	panic(NotFound())
}

// --- contents ---------------------------------------------------------------

// encodeBase64Wrapped is GitHub's contents encoding: standard base64 with a
// newline inserted after every 60 characters (never a trailing one).
func encodeBase64Wrapped(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	if len(encoded) <= 60 {
		return encoded
	}
	var out strings.Builder
	for len(encoded) > 60 {
		out.WriteString(encoded[:60])
		out.WriteByte('\n')
		encoded = encoded[60:]
	}
	out.WriteString(encoded)
	return out.String()
}

// decodeBase64Lenient mirrors Node's Buffer.from(text, 'base64'): whitespace
// is stripped by the caller; invalid characters are ignored and missing
// padding is tolerated.
func decodeBase64Lenient(text string) []byte {
	filtered := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			filtered = append(filtered, c)
		}
	}
	for len(filtered)%4 != 0 {
		filtered = append(filtered, '=')
	}
	decoded, err := base64.StdEncoding.DecodeString(string(filtered))
	if err != nil {
		// DecodeString returns the bytes decoded before the error.
		return decoded
	}
	return decoded
}

func stripWhitespace(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case ' ', '\t', '\n', '\r', '\v', '\f':
		default:
			out.WriteByte(text[i])
		}
	}
	return out.String()
}

func contentsGet(ctx *ReqCtx, repo *RepoRec, path string) *Reply {
	refParam, present := queryParam(ctx, "ref")
	if !present {
		refParam = repo.DefaultBranch
	}
	tip, ok := resolveRef(repo, refParam)
	if !ok {
		panic(NotFound())
	}
	tree := treeOfCommit(repo, tip)
	if tree == "" {
		panic(NotFound())
	}
	base := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name + "/contents"
	if path != "" {
		segments := strings.Split(path, "/")
		treeOid := tree
		for index, segment := range segments {
			treeObject, ok := repo.Store.Get(treeOid)
			if !ok || treeObject.Type != "tree" {
				panic(NotFound())
			}
			var entry *TreeEntry
			for _, candidate := range parseTree(treeObject.Data) {
				if candidate.Name == segment {
					candidateCopy := candidate
					entry = &candidateCopy
					break
				}
			}
			if entry == nil {
				panic(NotFound())
			}
			if index == len(segments)-1 {
				if entry.Mode == "40000" {
					return JsonReply(200, dirListing(ctx, repo, entry.Oid, path))
				}
				blob, ok := repo.Store.Get(entry.Oid)
				if !ok {
					panic(NotFound())
				}
				encodedRef := encodeURIComponent(refParam)
				return JsonReply(200, NewJObj().
					Set("name", entry.Name).
					Set("path", path).
					Set("sha", entry.Oid).
					Set("size", len(blob.Data)).
					Set("url", fmt.Sprintf("%s/%s?ref=%s", base, path, encodedRef)).
					Set("html_url", fmt.Sprintf("%s/%s/%s/blob/%s/%s", ctx.Origin, repo.Owner, repo.Name, encodedRef, path)).
					Set("git_url", fmt.Sprintf("%s%s/repos/%s/%s/git/blobs/%s", ctx.Origin, apiRoot, repo.Owner, repo.Name, entry.Oid)).
					Set("download_url", fmt.Sprintf("%s/%s/%s/raw/%s/%s", ctx.Origin, repo.Owner, repo.Name, encodedRef, path)).
					Set("type", "file").
					Set("content", encodeBase64Wrapped(blob.Data)).
					Set("encoding", "base64").
					Set("_links", NewJObj().
						Set("self", fmt.Sprintf("%s/%s?ref=%s", base, path, encodedRef)).
						Set("git", fmt.Sprintf("%s%s/repos/%s/%s/git/blobs/%s", ctx.Origin, apiRoot, repo.Owner, repo.Name, entry.Oid)).
						Set("html", fmt.Sprintf("%s/%s/%s/blob/%s/%s", ctx.Origin, repo.Owner, repo.Name, encodedRef, path))))
			}
			if entry.Mode != "40000" {
				panic(NotFound())
			}
			treeOid = entry.Oid
		}
	}
	return JsonReply(200, dirListing(ctx, repo, tree, ""))
}

func dirListing(ctx *ReqCtx, repo *RepoRec, treeOid string, prefix string) JArr {
	tree, ok := repo.Store.Get(treeOid)
	if !ok || tree.Type != "tree" {
		panic(NotFound())
	}
	base := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name + "/contents"
	out := JArr{}
	for _, entry := range parseTree(tree.Data) {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		isDir := entry.Mode == "40000"
		size := 0
		if !isDir {
			if blob, ok := repo.Store.Get(entry.Oid); ok {
				size = len(blob.Data)
			}
		}
		kind := "blob"
		if isDir {
			kind = "tree"
		}
		gitURL := fmt.Sprintf("%s%s/repos/%s/%s/git/%ss/%s", ctx.Origin, apiRoot, repo.Owner, repo.Name, kind, entry.Oid)
		htmlURL := fmt.Sprintf("%s/%s/%s/%s/%s/%s", ctx.Origin, repo.Owner, repo.Name, kind, repo.DefaultBranch, path)
		var downloadURL any
		if !isDir {
			downloadURL = fmt.Sprintf("%s/%s/%s/raw/%s/%s", ctx.Origin, repo.Owner, repo.Name, repo.DefaultBranch, path)
		}
		entryType := "file"
		if isDir {
			entryType = "dir"
		}
		out = append(out, NewJObj().
			Set("name", entry.Name).
			Set("path", path).
			Set("sha", entry.Oid).
			Set("size", size).
			Set("url", base+"/"+path).
			Set("html_url", htmlURL).
			Set("git_url", gitURL).
			Set("download_url", downloadURL).
			Set("type", entryType).
			Set("_links", NewJObj().
				Set("self", base+"/"+path).
				Set("git", gitURL).
				Set("html", htmlURL)))
	}
	return out
}

func contentsPut(ctx *ReqCtx, repo *RepoRec, path string) *Reply {
	body, _ := ctx.bodyObject()
	if body == nil {
		panic(ApiErrorf(400, "Problems parsing JSON"))
	}
	message, _ := bodyString(body, "message")
	if _, hasMessage := bodyString(body, "message"); !hasMessage {
		message = "Update file"
	}
	contentB64, hasContent := bodyString(body, "content")
	if !hasContent {
		panic(ApiErrorf(422, "Validation Failed: content is required"))
	}
	content := decodeBase64Lenient(stripWhitespace(contentB64))
	var branch *string
	if value, ok := bodyString(body, "branch"); ok {
		branch = &value
	}
	var sha *string
	if value, ok := bodyString(body, "sha"); ok {
		sha = &value
	}
	result := CommitFileChange(repo, CommitFileInput{Branch: branch, Path: path, Message: message, Content: &content, Sha: sha})
	commitObject, _ := repo.Store.Get(result.CommitSha)
	view := parseCommit(commitObject.Data)
	repoBase := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name
	branchForURL := repo.DefaultBranch
	if branch != nil {
		branchForURL = *branch
	}
	var blobSha any
	if result.BlobSha != nil {
		blobSha = *result.BlobSha
	}
	parents := JArr{}
	for _, parent := range view.Parents {
		parents = append(parents, NewJObj().
			Set("sha", parent).
			Set("url", repoBase+"/git/commits/"+parent).
			Set("html_url", fmt.Sprintf("%s/%s/%s/commit/%s", ctx.Origin, repo.Owner, repo.Name, parent)))
	}
	status := 200
	if result.Created {
		status = 201
	}
	name := path
	if slash := strings.LastIndex(path, "/"); slash >= 0 {
		name = path[slash+1:]
	}
	ident := NewJObj().
		Set("name", VIEWER.Name).
		Set("email", VIEWER.Email).
		Set("date", iso(repo.UpdatedEpoch))
	return JsonReply(status, NewJObj().
		Set("content", NewJObj().
			Set("name", name).
			Set("path", path).
			Set("sha", blobSha).
			Set("size", len(content)).
			Set("url", repoBase+"/contents/"+path).
			Set("html_url", fmt.Sprintf("%s/%s/%s/blob/%s/%s", ctx.Origin, repo.Owner, repo.Name, branchForURL, path)).
			Set("git_url", fmt.Sprintf("%s/git/blobs/%v", repoBase, blobSha)).
			Set("download_url", fmt.Sprintf("%s/%s/%s/raw/%s/%s", ctx.Origin, repo.Owner, repo.Name, repo.DefaultBranch, path)).
			Set("type", "file").
			Set("encoding", "base64")).
		Set("commit", NewJObj().
			Set("sha", result.CommitSha).
			Set("node_id", nodeId("06:Commit", repo.ID)).
			Set("url", repoBase+"/git/commits/"+result.CommitSha).
			Set("html_url", fmt.Sprintf("%s/%s/%s/commit/%s", ctx.Origin, repo.Owner, repo.Name, result.CommitSha)).
			Set("author", ident).
			Set("committer", ident).
			Set("tree", NewJObj().
				Set("sha", view.Tree).
				Set("url", repoBase+"/git/trees/"+view.Tree)).
			Set("message", message).
			Set("parents", parents)))
}

func contentsDelete(ctx *ReqCtx, repo *RepoRec, path string) *Reply {
	body, _ := ctx.bodyObject()
	if body == nil {
		panic(ApiErrorf(400, "Problems parsing JSON"))
	}
	message, hasMessage := bodyString(body, "message")
	if !hasMessage {
		message = "Delete file"
	}
	var branch *string
	if value, ok := bodyString(body, "branch"); ok {
		branch = &value
	}
	var sha *string
	if value, ok := bodyString(body, "sha"); ok {
		sha = &value
	}
	result := CommitFileChange(repo, CommitFileInput{Branch: branch, Path: path, Message: message, Content: nil, Sha: sha})
	repoBase := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name
	return JsonReply(200, NewJObj().
		Set("content", nil).
		Set("commit", NewJObj().
			Set("sha", result.CommitSha).
			Set("url", repoBase+"/git/commits/"+result.CommitSha).
			Set("message", message)))
}

// --- branches and commits ---------------------------------------------------

func branchesList(ctx *ReqCtx, repo *RepoRec) *Reply {
	type branchEntry struct {
		name string
		obj  *JObj
	}
	entries := []branchEntry{}
	for _, ref := range repo.Refs.Keys() {
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		sha, _ := repo.Refs.Get(ref)
		name := strings.TrimPrefix(ref, "refs/heads/")
		entries = append(entries, branchEntry{name: name, obj: NewJObj().
			Set("name", name).
			Set("commit", NewJObj().
				Set("sha", sha).
				Set("url", fmt.Sprintf("%s%s/repos/%s/%s/commits/%s", ctx.Origin, apiRoot, repo.Owner, repo.Name, sha))).
			Set("protected", false)})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	out := JArr{}
	for _, entry := range entries {
		out = append(out, entry.obj)
	}
	return JsonReply(200, out)
}

func branchGet(ctx *ReqCtx, repo *RepoRec, name string) *Reply {
	sha, ok := repo.Refs.Get("refs/heads/" + name)
	if !ok {
		panic(NotFound())
	}
	repoBase := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name
	return JsonReply(200, NewJObj().
		Set("name", name).
		Set("commit", NewJObj().
			Set("sha", sha).
			Set("node_id", nodeId("06:Commit", repo.ID)).
			Set("url", repoBase+"/commits/"+sha).
			Set("html_url", fmt.Sprintf("%s/%s/%s/commit/%s", ctx.Origin, repo.Owner, repo.Name, sha))).
		Set("protected", false).
		Set("_links", NewJObj().
			Set("self", repoBase+"/branches/"+name).
			Set("html", fmt.Sprintf("%s/%s/%s/branches/%s", ctx.Origin, repo.Owner, repo.Name, name))))
}

func commitsList(ctx *ReqCtx, repo *RepoRec) *Reply {
	refParam, present := queryParam(ctx, "sha")
	if !present {
		refParam = repo.DefaultBranch
	}
	tip, ok := resolveRef(repo, refParam)
	if !ok {
		return JsonReply(200, JArr{})
	}
	commits := listCommits(repo.Store, tip)
	start, end := pageRange(ctx, len(commits), apiRoot+"/repos/"+repo.Owner+"/"+repo.Name+"/commits")
	out := JArr{}
	for _, sha := range commits[start:end] {
		out = append(out, commitJson(ctx, repo, sha))
	}
	reply := JsonReply(200, out)
	attachLink(ctx, reply)
	return reply
}

// --- git refs ---------------------------------------------------------------

func refObjectJson(ctx *ReqCtx, repo *RepoRec, ref string, sha string) *JObj {
	objectType := "commit"
	if object, ok := repo.Store.Get(sha); ok {
		objectType = object.Type
	}
	repoBase := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name
	return NewJObj().
		Set("ref", ref).
		Set("node_id", nodeId("07:Ref", repo.ID)).
		Set("url", repoBase+"/git/"+ref).
		Set("object", NewJObj().
			Set("type", objectType).
			Set("sha", sha).
			Set("url", repoBase+"/git/commits/"+sha))
}

func refsList(ctx *ReqCtx, repo *RepoRec) *Reply {
	out := JArr{}
	for _, ref := range repo.Refs.Keys() {
		sha, _ := repo.Refs.Get(ref)
		out = append(out, refObjectJson(ctx, repo, ref, sha))
	}
	return JsonReply(200, out)
}

func getRef(ctx *ReqCtx, repo *RepoRec, ref string) *Reply {
	sha, ok := repo.Refs.Get(ref)
	if !ok {
		panic(NotFound())
	}
	return JsonReply(200, refObjectJson(ctx, repo, ref, sha))
}

func createRef(ctx *ReqCtx, repo *RepoRec) *Reply {
	body, _ := ctx.bodyObject()
	if body == nil {
		panic(ApiErrorf(400, "Problems parsing JSON"))
	}
	ref, _ := bodyString(body, "ref")
	sha, _ := bodyString(body, "sha")
	if ref == "" || !strings.HasPrefix(ref, "refs/") {
		panic(ApiErrorf(422, "Validation Failed: ref is invalid"))
	}
	lowerSha := strings.ToLower(sha)
	if sha == "" {
		panic(ApiErrorf(422, "Validation Failed: sha does not name an object"))
	}
	object, ok := repo.Store.Get(lowerSha)
	if !ok {
		panic(ApiErrorf(422, "Validation Failed: sha does not name an object"))
	}
	if _, exists := repo.Refs.Get(ref); exists {
		panic(ApiErrorf(422, "Reference already exists"))
	}
	repo.Refs.Set(ref, lowerSha)
	repoBase := ctx.Origin + apiRoot + "/repos/" + repo.Owner + "/" + repo.Name
	objectKind := "objects"
	if object.Type == "commit" {
		objectKind = "commits"
	}
	return JsonReply(201, NewJObj().
		Set("ref", ref).
		Set("node_id", nodeId("07:Ref", repo.ID)).
		Set("url", repoBase+"/git/"+ref).
		Set("object", NewJObj().
			Set("type", object.Type).
			Set("sha", lowerSha).
			Set("url", repoBase+"/git/"+objectKind+"/"+lowerSha)))
}

// --- labels -----------------------------------------------------------------

func routeLabels(ctx *ReqCtx, repo *RepoRec, rest []string) *Reply {
	if len(rest) == 1 {
		if ctx.Method == "POST" {
			body, _ := ctx.bodyObject()
			if body == nil {
				panic(ApiErrorf(400, "Problems parsing JSON"))
			}
			name, _ := bodyString(body, "name")
			if name == "" {
				panic(ApiErrorf(422, "Validation Failed: name is required"))
			}
			if _, exists := repo.Labels.Get(name); exists {
				panic(ApiErrorf(422, "Validation Failed: a label with that name already exists"))
			}
			color, hasColor := bodyString(body, "color")
			if !hasColor {
				color = "ededed"
			}
			color = strings.TrimPrefix(color, "#")
			var description *string
			if value, ok := bodyString(body, "description"); ok {
				description = &value
			}
			repo.Labels.Set(name, &LabelRec{Name: name, Color: color, Description: description})
			return JsonReply(201, labelJson(ctx, repo, name))
		}
		if ctx.Method == "GET" {
			out := JArr{}
			for _, name := range repo.Labels.Keys() {
				out = append(out, labelJson(ctx, repo, name))
			}
			return JsonReply(200, out)
		}
	}
	panic(NotFound())
}

// labelNamesOf extracts label names from a body's labels field: entries may
// be plain strings or {name} objects.
func labelNamesOf(body *JObj) []string {
	raw, present := body.Lookup("labels")
	if !present {
		return []string{}
	}
	list, isList := raw.(JArr)
	if !isList {
		return []string{}
	}
	names := []string{}
	for _, entry := range list {
		if text, ok := entry.(string); ok {
			names = append(names, text)
		} else if record, ok := entry.(*JObj); ok {
			if name, ok := record.Get("name").(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// --- issues -----------------------------------------------------------------

// jsKey converts a JS Number() path segment into a map key, when integral.
func jsKey(segment string) (int, bool) {
	value, ok := jsNumberParse(segment)
	if !ok || value != value || value != float64(int64(value)) {
		return 0, false
	}
	return int(value), true
}

func requireIssue(repo *RepoRec, segment string) *IssueRec {
	number, ok := jsKey(segment)
	if !ok {
		panic(NotFound())
	}
	issue, exists := repo.Issues[number]
	if !exists {
		panic(NotFound())
	}
	return issue
}

func routeIssues(ctx *ReqCtx, repo *RepoRec, rest []string) *Reply {
	method := ctx.Method
	if len(rest) == 1 {
		if method == "GET" {
			state, present := queryParam(ctx, "state")
			if !present {
				state = "open"
			}
			issues := []*IssueRec{}
			for _, number := range sortedIssueNumbers(repo) {
				issue := repo.Issues[number]
				if state == "open" || state == "closed" {
					if issue.State != state {
						continue
					}
				}
				issues = append(issues, issue)
			}
			direction, _ := queryParam(ctx, "direction")
			sort.SliceStable(issues, func(i, j int) bool {
				if direction == "asc" {
					return issues[i].Number < issues[j].Number
				}
				return issues[i].Number > issues[j].Number
			})
			start, end := pageRange(ctx, len(issues), apiRoot+"/repos/"+repo.Owner+"/"+repo.Name+"/issues")
			out := JArr{}
			for _, issue := range issues[start:end] {
				out = append(out, issueJson(ctx, repo, issue))
			}
			reply := JsonReply(200, out)
			attachLink(ctx, reply)
			return reply
		}
		if method == "POST" {
			body, _ := ctx.bodyObject()
			if body == nil {
				panic(ApiErrorf(400, "Problems parsing JSON"))
			}
			title, _ := bodyString(body, "title")
			if title == "" {
				panic(ApiErrorf(422, "Validation Failed: title is required"))
			}
			labels := labelNamesOf(body)
			for _, name := range labels {
				if _, exists := repo.Labels.Get(name); !exists {
					panic(ApiErrorf(422, "Validation Failed: label %q does not exist", name))
				}
			}
			epoch := tickEpochSec()
			issue := &IssueRec{
				ID:           theForge.AllocObjectID(),
				Number:       repo.NextNumber,
				Title:        title,
				Body:         stringOr(body.Get("body"), ""),
				State:        "open",
				Labels:       labels,
				CreatedEpoch: epoch,
				UpdatedEpoch: epoch,
				Comments:     []*CommentRec{},
			}
			repo.NextNumber++
			repo.Issues[issue.Number] = issue
			return JsonReply(201, issueJson(ctx, repo, issue))
		}
		panic(NotFound())
	}
	if len(rest) == 2 {
		if method == "GET" {
			return JsonReply(200, issueJson(ctx, repo, requireIssue(repo, rest[1])))
		}
		if method == "PATCH" {
			issue := requireIssue(repo, rest[1])
			body, _ := ctx.bodyObject()
			if body == nil {
				panic(ApiErrorf(400, "Problems parsing JSON"))
			}
			if title, ok := bodyString(body, "title"); ok {
				issue.Title = title
			}
			if text, ok := bodyString(body, "body"); ok {
				issue.Body = text
			}
			if state, ok := bodyString(body, "state"); ok && (state == "closed" || state == "open") {
				issue.State = state
			}
			if rawLabels, present := body.Lookup("labels"); present {
				if _, isList := rawLabels.(JArr); isList {
					labels := labelNamesOf(body)
					for _, name := range labels {
						if _, exists := repo.Labels.Get(name); !exists {
							panic(ApiErrorf(422, "Validation Failed: label %q does not exist", name))
						}
					}
					issue.Labels = labels
				}
			}
			issue.UpdatedEpoch = tickEpochSec()
			return JsonReply(200, issueJson(ctx, repo, issue))
		}
		panic(NotFound())
	}
	if len(rest) == 3 && rest[2] == "comments" {
		issue := requireIssue(repo, rest[1])
		if method == "GET" {
			out := JArr{}
			for _, comment := range issue.Comments {
				out = append(out, commentJson(ctx, repo, issue, comment))
			}
			return JsonReply(200, out)
		}
		if method == "POST" {
			body, _ := ctx.bodyObject()
			if body == nil {
				panic(ApiErrorf(400, "Problems parsing JSON"))
			}
			text, hasText := bodyString(body, "body")
			if !hasText {
				panic(ApiErrorf(422, "Validation Failed: body is required"))
			}
			comment := &CommentRec{ID: theForge.AllocCommentID(), Body: text, CreatedEpoch: tickEpochSec()}
			issue.Comments = append(issue.Comments, comment)
			return JsonReply(201, commentJson(ctx, repo, issue, comment))
		}
	}
	panic(NotFound())
}

// --- pulls ------------------------------------------------------------------

func requirePull(repo *RepoRec, segment string) *PullRec {
	number, ok := jsKey(segment)
	if !ok {
		panic(NotFound())
	}
	pull, exists := repo.Pulls[number]
	if !exists {
		panic(NotFound())
	}
	return pull
}

func routePulls(ctx *ReqCtx, repo *RepoRec, rest []string) *Reply {
	method := ctx.Method
	if len(rest) == 1 {
		if method == "GET" {
			state, present := queryParam(ctx, "state")
			if !present {
				state = "open"
			}
			pulls := []*PullRec{}
			for _, number := range sortedPullNumbers(repo) {
				pull := repo.Pulls[number]
				if state == "open" || state == "closed" {
					if pull.State != state {
						continue
					}
				}
				pulls = append(pulls, pull)
			}
			direction, _ := queryParam(ctx, "direction")
			sort.SliceStable(pulls, func(i, j int) bool {
				if direction == "asc" {
					return pulls[i].Number < pulls[j].Number
				}
				return pulls[i].Number > pulls[j].Number
			})
			start, end := pageRange(ctx, len(pulls), apiRoot+"/repos/"+repo.Owner+"/"+repo.Name+"/pulls")
			out := JArr{}
			for _, pull := range pulls[start:end] {
				out = append(out, pullJson(ctx, repo, pull))
			}
			reply := JsonReply(200, out)
			attachLink(ctx, reply)
			return reply
		}
		if method == "POST" {
			body, _ := ctx.bodyObject()
			if body == nil {
				panic(ApiErrorf(400, "Problems parsing JSON"))
			}
			title, _ := bodyString(body, "title")
			head, _ := bodyString(body, "head")
			base, _ := bodyString(body, "base")
			if title == "" || head == "" || base == "" {
				panic(ApiErrorf(422, "Validation Failed: title, head and base are required"))
			}
			pull := OpenPull(repo, title, stringOr(body.Get("body"), ""), head, base)
			return JsonReply(201, pullJson(ctx, repo, pull))
		}
		panic(NotFound())
	}
	if len(rest) == 2 && method == "GET" {
		return JsonReply(200, pullJson(ctx, repo, requirePull(repo, rest[1])))
	}
	if len(rest) == 3 && rest[2] == "files" && method == "GET" {
		pull := requirePull(repo, rest[1])
		analysis := AnalyzePull(repo, pull)
		number, _ := jsNumberParse(rest[1])
		path := fmt.Sprintf("%s/repos/%s/%s/pulls/%s/files", apiRoot, repo.Owner, repo.Name, jsStringNumber(number))
		start, end := pageRange(ctx, len(analysis.Files), path)
		out := JArr{}
		for _, file := range analysis.Files[start:end] {
			var sha any
			shaText := ""
			if file.Sha != nil {
				sha = *file.Sha
				shaText = *file.Sha
			}
			out = append(out, NewJObj().
				Set("sha", sha).
				Set("filename", file.Filename).
				Set("status", file.Status).
				Set("additions", file.Additions).
				Set("deletions", file.Deletions).
				Set("changes", file.Changes).
				Set("blob_url", fmt.Sprintf("%s%s/repos/%s/%s/git/blobs/%s", ctx.Origin, apiRoot, repo.Owner, repo.Name, shaText)).
				Set("raw_url", fmt.Sprintf("%s/%s/%s/raw/%s/%s", ctx.Origin, repo.Owner, repo.Name, pull.HeadRef, file.Filename)).
				Set("contents_url", fmt.Sprintf("%s%s/repos/%s/%s/contents/%s?ref=%s", ctx.Origin, apiRoot, repo.Owner, repo.Name, file.Filename, encodeURIComponent(pull.HeadRef))))
		}
		reply := JsonReply(200, out)
		attachLink(ctx, reply)
		return reply
	}
	if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
		pull := requirePull(repo, rest[1])
		body, _ := ctx.bodyObject()
		mergeMethod := "merge"
		if body != nil {
			if value, ok := bodyString(body, "merge_method"); ok {
				mergeMethod = value
			}
		}
		if pull.State != "open" {
			panic(ApiErrorf(405, "Pull Request is not mergeable"))
		}
		var sha string
		if mergeMethod == "merge" {
			sha = mergePullTrue(ctx, repo, pull)
		} else {
			sha = SquashMerge(repo, pull)
		}
		return JsonReply(200, NewJObj().
			Set("sha", sha).
			Set("merged", true).
			Set("message", "Pull Request successfully merged"))
	}
	panic(NotFound())
}

// mergePullTrue performs a true merge (merge commit with two parents), the
// REST surface's default merge method.
func mergePullTrue(ctx *ReqCtx, repo *RepoRec, pull *PullRec) string {
	analysis := AnalyzePull(repo, pull)
	if analysis.Conflict || analysis.Base == nil {
		panic(ApiErrorf(405, "Conflict: the pull request has conflicts"))
	}
	headTip, _ := repo.Refs.Get("refs/heads/" + pull.HeadRef)
	baseTip, _ := repo.Refs.Get("refs/heads/" + pull.BaseRef)
	baseTree := treeOfCommit(repo, *analysis.Base)
	headTree := treeOfCommit(repo, headTip)
	ontoTree := treeOfCommit(repo, baseTip)
	merged := threeWayMerge(repo.Store, baseTree, headTree, ontoTree)
	if !merged.OK {
		panic(ApiErrorf(405, "Conflict: the pull request has conflicts"))
	}
	epoch := tickEpochSec()
	message := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", pull.Number, pull.HeadRef, pull.Title)
	sha := repo.Store.Put("commit", buildCommitData(merged.Tree, []string{baseTip, headTip}, message, viewerIdent(epoch)))
	repo.Refs.Set("refs/heads/"+pull.BaseRef, sha)
	pull.State = "closed"
	pull.Merged = true
	mergedEpoch := epoch
	pull.MergedEpoch = &mergedEpoch
	pull.MergeCommitSha = strPtr(sha)
	pull.UpdatedEpoch = epoch
	return sha
}
