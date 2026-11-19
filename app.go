// app.go — one dispatch function for the whole product. It takes a parsed
// request (method, decoded path, query, headers, body) and returns a Reply.
// The server shell in main.go is the only other piece.

package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// theForge is the single in-memory forge instance.
var theForge = NewForge()

// stateMu serializes access to the forge: reads run under the read lock,
// mutations under the write lock. This mirrors the single-threaded TS
// server's semantics while still letting the HTTP layer stay concurrent.
var stateMu sync.RWMutex

// ApiError is the HTTP error every surface raises (via panic) and the
// dispatchers translate into a message reply.
type ApiError struct {
	Status  int
	Message string
}

func (e *ApiError) Error() string { return e.Message }

func ApiErrorf(status int, format string, args ...any) *ApiError {
	return &ApiError{Status: status, Message: fmt.Sprintf(format, args...)}
}

func NotFound() *ApiError { return &ApiError{Status: 404, Message: "Not Found"} }
func NotMergeable() *ApiError {
	return &ApiError{Status: 405, Message: "Pull Request is not mergeable"}
}
func ValidationFailed(msg string) *ApiError {
	return &ApiError{Status: 422, Message: "Validation Failed: " + msg}
}

var requestCounter atomic.Int64

func nextRequestId() string {
	counter := requestCounter.Add(1)
	var buf [48]byte
	out := append(buf[:0], "vinz-"...)
	out = strconv.AppendInt(out, counter, 36)
	out = append(out, '-')
	out = strconv.AppendUint(out, uint64((counter*2654435761)%0xffffffff), 16)
	return string(out)
}

var gitRoutePattern = regexp.MustCompile(`^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$`)

func isMutation(ctx *ReqCtx) bool {
	if strings.HasPrefix(ctx.Path, "/api/graphql") || ctx.Path == "/mcp" {
		return true
	}
	if strings.Contains(ctx.Path, "git-receive-pack") {
		return true
	}
	if strings.Contains(ctx.Path, "git-upload-pack") {
		// Fetch and ls-refs only read the store (objects are immutable
		// once stored), so they run under the read lock and parallelise.
		return false
	}
	switch ctx.Method {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}

// Dispatch routes a request. Mirrors the TS handleRequest: routing errors
// become message replies, and /api/ replies get a request-id header — added
// only when routing returned normally (an ApiError skips it, as in TS).
func Dispatch(ctx *ReqCtx) (reply *Reply) {
	if isMutation(ctx) {
		stateMu.Lock()
		defer stateMu.Unlock()
	} else {
		stateMu.RLock()
		defer stateMu.RUnlock()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if apiErr, ok := recovered.(*ApiError); ok {
				reply = MessageReply(apiErr.Status, apiErr.Message)
			} else {
				reply = MessageReply(500, "Internal Server Error")
			}
		}
	}()
	reply = route(ctx)
	if strings.HasPrefix(ctx.Path, "/api/") {
		reply.Headers["x-github-request-id"] = nextRequestId()
	}
	return reply
}

func route(ctx *ReqCtx) *Reply {
	if ctx.Path == "/healthz" {
		return JsonReply(200, NewJObj().Set("ok", true))
	}
	if ctx.Path == "/api/v3" || strings.HasPrefix(ctx.Path, "/api/v3/") {
		return HandleRest(ctx)
	}
	if ctx.Path == "/api/graphql" {
		return HandleGraphQL(ctx)
	}
	if ctx.Path == "/mcp" {
		return HandleMcp(ctx)
	}
	match := gitRoutePattern.FindStringSubmatch(ctx.Path)
	if match != nil {
		// The TS decodes the captures with decodeURIComponent, which throws
		// on malformed escapes; the dispatcher turns that into a 500.
		owner := decodeStrict(match[1])
		repoName := decodeStrict(match[2])
		return HandleGit(ctx, owner, repoName, match[3])
	}
	return MessageReply(404, "Not Found")
}
