// web.go — the shared HTTP plumbing types: what a request looks like once
// main.go has read it, what a surface hands back as a reply, and the ordered
// JSON writer that reproduces JSON.stringify's key order and escaping.

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// ReqCtx is one request, fully read.
type ReqCtx struct {
	Method        string
	Path          string // decoded URL pathname
	Query         url.Values
	Origin        string // http://<host> of this very request
	Headers       map[string]string
	Body          []byte
	Authorized    bool
	GitProtocolV2 bool // the Git-Protocol header asked for protocol v2

	linkHeader string // set by paginate, copied onto the reply by attachLink

	bodyParsed    bool
	bodyValue     any
	bodyMalformed bool
}

// Reply is one response.
type Reply struct {
	Status      int
	ContentType string
	Body        []byte
	Headers     map[string]string

	poolBuf *bytes.Buffer // set when Body aliases a pooled marshal buffer
}

// ---------------------------------------------------------------------------
// ordered JSON

// JObj is a JSON object that preserves insertion order, like a JS object
// literal passed to JSON.stringify.
type JObj struct {
	keys []string
	vals []any
	idx  map[string]int
}

// JArr is a JSON array.
type JArr = []any

func NewJObj() *JObj {
	return &JObj{idx: make(map[string]int)}
}

func (o *JObj) Set(key string, value any) *JObj {
	if at, ok := o.idx[key]; ok {
		o.vals[at] = value
		return o
	}
	o.idx[key] = len(o.keys)
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, value)
	return o
}

// Get returns the value for key, or nil when absent.
func (o *JObj) Get(key string) any {
	if at, ok := o.idx[key]; ok {
		return o.vals[at]
	}
	return nil
}

// Lookup returns the value for key and whether the key is present at all
// (the JS `in` operator).
func (o *JObj) Lookup(key string) (any, bool) {
	if at, ok := o.idx[key]; ok {
		return o.vals[at], true
	}
	return nil, false
}

func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		if start < i {
			buf.WriteString(s[start:i])
		}
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			buf.WriteString(`\u00`)
			const hexdigits = "0123456789abcdef"
			buf.WriteByte(hexdigits[c>>4])
			buf.WriteByte(hexdigits[c&0xf])
		}
		start = i + 1
	}
	if start < len(s) {
		buf.WriteString(s[start:])
	}
	buf.WriteByte('"')
}

// jsNumber formats a float64 the way JSON.stringify would.
func jsNumber(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return "null"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatInt(int64(f), 10)
	}
	text := strconv.FormatFloat(f, 'e', -1, 64)
	if !strings.ContainsAny(text, "eE") {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	// JS uses exponent form only outside [1e-6, 1e21).
	if abs := math.Abs(f); abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	// Strip a leading zero in the exponent ("e-07" -> "e-7").
	if i := strings.IndexByte(text, 'e'); i >= 0 {
		mantissa, exponent := text[:i], text[i+1:]
		sign := ""
		if strings.HasPrefix(exponent, "-") || strings.HasPrefix(exponent, "+") {
			sign, exponent = exponent[:1], exponent[1:]
		}
		exponent = strings.TrimLeft(exponent, "0")
		if exponent == "" {
			exponent = "0"
		}
		return mantissa + "e" + sign + exponent
	}
	return text
}

func writeJSONValue(buf *bytes.Buffer, value any) {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeJSONString(buf, v)
	case int:
		buf.WriteString(strconv.Itoa(v))
	case int64:
		buf.WriteString(strconv.FormatInt(v, 10))
	case float64:
		buf.WriteString(jsNumber(v))
	case *JObj:
		buf.WriteByte('{')
		for i, key := range v.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, key)
			buf.WriteByte(':')
			writeJSONValue(buf, v.vals[i])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, entry := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONValue(buf, entry)
		}
		buf.WriteByte(']')
	case []string:
		buf.WriteByte('[')
		for i, entry := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, entry)
		}
		buf.WriteByte(']')
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			buf.WriteString("null")
		} else {
			buf.Write(raw)
		}
	}
}

func marshalJSON(value any) []byte {
	var buf bytes.Buffer
	writeJSONValue(&buf, value)
	return buf.Bytes()
}

// Marshal buffers are pooled for whole replies: JsonReply renders into a
// pooled buffer and writeReply returns it to the pool once the body has
// been written out, so the hot read/write paths stop re-growing a fresh
// buffer (and its backing arrays) for every response.
var jsonBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func marshalJSONPooled(value any) ([]byte, *bytes.Buffer) {
	buf := jsonBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	writeJSONValue(buf, value)
	return buf.Bytes(), buf
}

// JsonReply is a JSON reply with an initialized (empty) header map.
func JsonReply(status int, value any) *Reply {
	body, buf := marshalJSONPooled(value)
	return &Reply{
		Status:      status,
		ContentType: "application/json; charset=utf-8",
		Body:        body,
		Headers:     map[string]string{},
		poolBuf:     buf,
	}
}

// MessageReply is the {"message": ...} shape every error surface uses.
func MessageReply(status int, message string) *Reply {
	return JsonReply(status, NewJObj().Set("message", message))
}

// ---------------------------------------------------------------------------
// request body JSON

// bodyJSON parses the request body as JSON. It reports (value, malformed):
// an empty body is (nil, false) — the TS bodyJson returns null for empty and
// undefined for unparseable.
func (ctx *ReqCtx) bodyJSON() (any, bool) {
	if ctx.bodyParsed {
		return ctx.bodyValue, ctx.bodyMalformed
	}
	ctx.bodyParsed = true
	if len(bytes.TrimSpace(ctx.Body)) == 0 {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(ctx.Body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		ctx.bodyMalformed = true
		return nil, true
	}
	// JSON.parse rejects trailing garbage; a second token means malformed.
	if decoder.More() {
		ctx.bodyMalformed = true
		return nil, true
	}
	ctx.bodyValue = normalizeJSON(value)
	return ctx.bodyValue, false
}

// normalizeJSON turns json.Number into float64 and objects into *JObj,
// matching JS values.
func normalizeJSON(value any) any {
	switch v := value.(type) {
	case json.Number:
		f, _ := v.Float64()
		return f
	case map[string]any:
		obj := NewJObj()
		for key, entry := range v {
			obj.Set(key, normalizeJSON(entry))
		}
		return obj
	case []any:
		for i, entry := range v {
			v[i] = normalizeJSON(entry)
		}
		return v
	default:
		return value
	}
}

// bodyObject is the request body as a JSON object plus a malformed flag:
// (nil, true) when the body was non-empty but unparseable, (nil, false)
// when it parsed to a non-object or was empty.
func (ctx *ReqCtx) bodyObject() (*JObj, bool) {
	value, malformed := ctx.bodyJSON()
	if malformed {
		return nil, true
	}
	if obj, ok := value.(*JObj); ok {
		return obj, false
	}
	return nil, false
}

// stringOr is the TS stringOr: the value when it is a string, else fallback.
func stringOr(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fallback
}

// ---------------------------------------------------------------------------
// path and encoding helpers

// decodeSegment is decodeURIComponent with a raw fallback, as in the TS.
func decodeSegment(segment string) string {
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return segment
	}
	return decoded
}

// decodeStrict is decodeURIComponent: it panics on malformed escapes, like
// the JS function throws (the dispatcher turns that into a 500 reply).
func decodeStrict(segment string) string {
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		panic(err)
	}
	return decoded
}

// pathSegments strips prefix from the (already once-decoded) path, splits
// on '/', drops empty segments, and decodes each segment again — exactly
// the TS helper.
func pathSegments(path string, prefix string) []string {
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.Split(rest, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, decodeSegment(part))
		}
	}
	return out
}

const uriUnreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"

// encodeURIComponent with JS semantics.
func encodeURIComponent(text string) string {
	var buf strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if strings.IndexByte(uriUnreserved, c) >= 0 {
			buf.WriteByte(c)
		} else {
			const hexdigits = "0123456789ABCDEF"
			buf.WriteByte('%')
			buf.WriteByte(hexdigits[c>>4])
			buf.WriteByte(hexdigits[c&0xf])
		}
	}
	return buf.String()
}

// jsNumberParse is JS Number(string): a float, with ok=false for NaN.
func jsNumberParse(text string) (float64, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, true
	}
	if strings.HasPrefix(trimmed, "0x") || strings.HasPrefix(trimmed, "0X") {
		n, err := strconv.ParseUint(trimmed[2:], 16, 64)
		if err != nil {
			return 0, false
		}
		return float64(n), true
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		// JS also accepts "Infinity"; ParseFloat does too, but it rejects
		// things like "0b101"/"0o17" that JS accepts. Those never appear in
		// this API's numeric path segments, so treat them as NaN.
		return 0, false
	}
	return f, true
}

// numberKey resolves a path segment the way JS Number() + a Map lookup by
// integer key would: "3", "3.0" and "1e2" find key 3/100; "abc" finds none.
func numberKey(text string) (int, bool) {
	f, ok := jsNumberParse(text)
	if !ok || math.IsNaN(f) || f != math.Trunc(f) || math.Abs(f) > 1e15 {
		return 0, false
	}
	return int(f), true
}

func nanFloat() float64 { return math.NaN() }
