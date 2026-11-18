// main.go — the entrypoint: the HTTP server bootstrap and the entry <-> env
// lifecycle. The server binds an ephemeral port (or $PORT when set for local
// development), and when the env is present ($MAKEFUN_ENV_URL) it registers
// its port with the env and long-polls the env's /done until the trial is
// over, then exits 0. Without an env it simply serves, which is how the
// conformance suite is driven locally.
//
// The forge itself lives in app.go and the surface modules; this file only
// owns the socket, the request reading and the lifecycle.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxBodyBytes = 256 * 1024 * 1024

func writeReply(w http.ResponseWriter, reply *Reply) {
	header := w.Header()
	header.Set("content-type", reply.ContentType)
	for key, value := range reply.Headers {
		header.Set(key, value)
	}
	if reply.Status != 204 {
		header.Set("content-length", strconv.Itoa(len(reply.Body)))
	}
	w.WriteHeader(reply.Status)
	if len(reply.Body) > 0 {
		_, _ = w.Write(reply.Body)
	}
}

func writeInternalError(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(500)
	_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
}

// decodePath is decodeURIComponent with a raw fallback.
func decodePath(raw string) string {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return raw
	}
	return decoded
}

func serveHTTP(expectedToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Method != "GET" && r.Method != "HEAD" {
			data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
			if err != nil || len(data) > maxBodyBytes {
				writeInternalError(w)
				return
			}
			body = data
		} else {
			body = []byte{}
		}
		host := r.Host
		if host == "" {
			host = "localhost"
		}
		headers := make(map[string]string, len(r.Header))
		for key, values := range r.Header {
			if len(values) > 0 {
				headers[strings.ToLower(key)] = values[0]
			}
		}
		authorization := r.Header.Get("Authorization")
		ctx := &ReqCtx{
			Method:     r.Method,
			Path:       decodePath(r.URL.EscapedPath()),
			Query:      r.URL.Query(),
			Origin:     "http://" + host,
			Headers:    headers,
			Body:       body,
			Authorized: expectedToken != "" && authorization == "Bearer "+expectedToken,
		}
		reply := Dispatch(ctx)
		writeReply(w, reply)
	}
}

func postRegister(envBase string, port int) (registered bool, stop bool) {
	payload, _ := json.Marshal(map[string]int{"port": port})
	response, err := http.Post(envBase+"/register", "application/json", bytes.NewReader(payload))
	if err != nil {
		return false, false
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	switch response.StatusCode {
	case 200, 409:
		return true, false
	case 400, 405:
		return false, true
	}
	return false, false
}

func pollDone(envBase string) bool {
	response, err := http.Get(envBase + "/done")
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	var parsed map[string]any
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		return false
	}
	done, _ := parsed["done"].(bool)
	return done
}

func run() int {
	envUrl := os.Getenv("MAKEFUN_ENV_URL")
	expectedToken := os.Getenv("MAKEFUN_SHOP_TOKEN")

	server := &http.Server{
		Handler:           serveHTTP(expectedToken),
		IdleTimeout:       75 * time.Second,
		ReadHeaderTimeout: 80 * time.Second,
	}

	requestedPort := 0
	if raw := os.Getenv("PORT"); raw != "" {
		if value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && value > 0 {
			requestedPort = int(value)
		}
	}
	port := requestedPort
	if envUrl != "" && port == 8080 {
		port = 0 // the env owns 8080
	}

	listener, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err.Error())
		return 1
	}
	boundPort := listener.Addr().(*net.TCPAddr).Port
	fmt.Printf("vinz-forge listening on %d\n", boundPort)

	if envUrl == "" {
		// Local development: serve until killed.
		_ = server.Serve(listener)
		return 0
	}

	go func() { _ = server.Serve(listener) }()

	// Register with the env: the first register wins; a 409 means this
	// process lost a race and simply keeps waiting like any other.
	envBase := strings.TrimRight(envUrl, "/")
	registered := false
	for attempt := 0; attempt < 50 && !registered; attempt++ {
		ok, stop := postRegister(envBase, boundPort)
		if ok {
			registered = true
			break
		}
		if stop {
			break
		}
		if !registered {
			time.Sleep(200 * time.Millisecond)
		}
	}

	// Long-poll /done until the env says the trial is over.
	for {
		if pollDone(envBase) {
			break
		}
		time.Sleep(750 * time.Millisecond)
	}
	_ = server.Close()
	return 0
}

func main() {
	os.Exit(run())
}
