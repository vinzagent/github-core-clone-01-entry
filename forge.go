// forge.go — the in-memory heart of the forge: users, repositories, the git
// object store, issues, labels and pull requests, plus the pure git plumbing
// (trees, commits, packs, deltas, reachability, merges) every surface shares.
//
// Everything is deterministic: ids come from counters and timestamps from a
// fixed base epoch plus a counter, never from the wall clock or a random
// source, so the same request sequence always produces the same bytes.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// identity

// UserRec is the one user this forge knows: the bearer token's owner.
type UserRec struct {
	Login string
	ID    int
	Name  string
	Email string
	Type  string
}

var VIEWER = UserRec{
	Login: "adaiversen",
	ID:    1000,
	Name:  "ada iversen",
	Email: "adaiversen@example.invalid",
	Type:  "User",
}

// Deterministic clock: a fixed base epoch, advanced one second per tick.
const baseEpochSec = 1_700_000_000

var clockTicks atomic.Int64

func tickEpochSec() int64 {
	return baseEpochSec + clockTicks.Add(1)
}

func isoOf(epochSec int64) string {
	return time.Unix(epochSec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// viewerIdent is the git author/committer line for the viewer at a tick.
func viewerIdent(epochSec int64) string {
	return fmt.Sprintf("%s <%s> %d +0000", VIEWER.Name, VIEWER.Email, epochSec)
}

// ---------------------------------------------------------------------------
// ordered string map (JS Map insertion-order semantics)

// OrderedMap is a string-keyed map that iterates in insertion order.
type OrderedMap[V any] struct {
	keys []string
	vals map[string]V
}

func NewOrderedMap[V any]() *OrderedMap[V] {
	return &OrderedMap[V]{vals: make(map[string]V)}
}

func (m *OrderedMap[V]) Get(key string) (V, bool) {
	value, ok := m.vals[key]
	return value, ok
}

func (m *OrderedMap[V]) Has(key string) bool {
	_, ok := m.vals[key]
	return ok
}

func (m *OrderedMap[V]) Set(key string, value V) {
	if _, ok := m.vals[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = value
}

func (m *OrderedMap[V]) Delete(key string) bool {
	if _, ok := m.vals[key]; !ok {
		return false
	}
	delete(m.vals, key)
	for i, candidate := range m.keys {
		if candidate == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
	return true
}

func (m *OrderedMap[V]) Keys() []string {
	return append([]string(nil), m.keys...)
}

func (m *OrderedMap[V]) Len() int {
	return len(m.keys)
}

// ---------------------------------------------------------------------------
// git objects

const ZERO_OID = "0000000000000000000000000000000000000000"

var typeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

type GitObject struct {
	Type string
	Data []byte
}

func hashObject(objType string, data []byte) string {
	hash := sha1.New()
	hash.Write([]byte(fmt.Sprintf("%s %d\x00", objType, len(data))))
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

// ObjectStore is a content-addressed object store: one per repository.
type ObjectStore struct {
	objects map[string]*GitObject
}

func NewObjectStore() *ObjectStore {
	return &ObjectStore{objects: make(map[string]*GitObject)}
}

func (s *ObjectStore) Put(objType string, data []byte) string {
	oid := hashObject(objType, data)
	if _, ok := s.objects[oid]; !ok {
		s.objects[oid] = &GitObject{Type: objType, Data: data}
	}
	return oid
}

func (s *ObjectStore) Get(oid string) (*GitObject, bool) {
	object, ok := s.objects[oid]
	return object, ok
}

func (s *ObjectStore) Has(oid string) bool {
	_, ok := s.objects[oid]
	return ok
}

// ---------------------------------------------------------------------------
// trees

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

func parseTree(data []byte) []TreeEntry {
	entries := []TreeEntry{}
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			Oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries
}

func treeEntryKey(entry TreeEntry) string {
	if entry.Mode == "40000" {
		return entry.Name + "/"
	}
	return entry.Name
}

func buildTreeData(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeEntryKey(sorted[i]) < treeEntryKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, entry := range sorted {
		buf.WriteString(entry.Mode)
		buf.WriteByte(' ')
		buf.WriteString(entry.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(entry.Oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// flattenTree flattens a tree (recursively) into path -> blob oid.
func flattenTree(store *ObjectStore, treeOid string, prefix string, out map[string]string) map[string]string {
	if out == nil {
		out = make(map[string]string)
	}
	tree, ok := store.Get(treeOid)
	if !ok || tree.Type != "tree" {
		return out
	}
	for _, entry := range parseTree(tree.Data) {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			flattenTree(store, entry.Oid, path, out)
		} else {
			out[path] = entry.Oid
		}
	}
	return out
}

// buildTreeFromFlat builds nested trees from a flat path -> blob oid map.
func buildTreeFromFlat(store *ObjectStore, flat map[string]string) string {
	type node struct {
		files map[string]string
		dirs  map[string]*node
	}
	makeNode := func() *node {
		return &node{files: make(map[string]string), dirs: make(map[string]*node)}
	}
	root := makeNode()
	for path, oid := range flat {
		segments := strings.Split(path, "/")
		current := root
		for i := 0; i < len(segments)-1; i++ {
			name := segments[i]
			if _, ok := current.dirs[name]; !ok {
				current.dirs[name] = makeNode()
			}
			current = current.dirs[name]
		}
		current.files[segments[len(segments)-1]] = oid
	}
	var build func(n *node) string
	build = func(n *node) string {
		entries := []TreeEntry{}
		for name, oid := range n.files {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, Oid: oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: build(child)})
		}
		return store.Put("tree", buildTreeData(entries))
	}
	return build(root)
}

// subtreeAt returns the entries of the tree at path inside rootTreeOid.
func subtreeAt(store *ObjectStore, rootTreeOid string, path string) (string, []TreeEntry, bool) {
	current := rootTreeOid
	if path != "" {
		for _, segment := range strings.Split(path, "/") {
			tree, ok := store.Get(current)
			if !ok || tree.Type != "tree" {
				return "", nil, false
			}
			found := false
			for _, entry := range parseTree(tree.Data) {
				if entry.Name == segment {
					if entry.Mode != "40000" {
						return "", nil, false
					}
					current = entry.Oid
					found = true
					break
				}
			}
			if !found {
				return "", nil, false
			}
		}
	}
	tree, ok := store.Get(current)
	if !ok || tree.Type != "tree" {
		return "", nil, false
	}
	return current, parseTree(tree.Data), true
}

// ---------------------------------------------------------------------------
// commits and tags

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) CommitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	message := ""
	if blank >= 0 {
		headerEnd = blank
		message = string(data[blank+2:])
	}
	view := CommitView{Parents: []string{}, Message: message}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		key, value := line, ""
		if space := strings.IndexByte(line, ' '); space >= 0 {
			key, value = line[:space], line[space+1:]
		}
		switch key {
		case "tree":
			view.Tree = value
		case "parent":
			view.Parents = append(view.Parents, value)
		case "author":
			view.Author = value
		case "committer":
			view.Committer = value
		}
	}
	return view
}

func buildCommitData(tree string, parents []string, message string, ident string) []byte {
	lines := []string{"tree " + tree}
	for _, parent := range parents {
		lines = append(lines, "parent "+parent)
	}
	lines = append(lines, "author "+ident, "committer "+ident, "")
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	return []byte(strings.Join(lines, "\n") + "\n" + message)
}

type tagView struct {
	Object string
	Type   string
	Tag    string
}

func parseTag(data []byte) tagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headers := make(map[string]string)
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if space := strings.IndexByte(line, ' '); space > 0 {
			headers[line[:space]] = line[space+1:]
		}
	}
	return tagView{Object: headers["object"], Type: headers["type"], Tag: headers["tag"]}
}

// peelToCommit peels a tag chain to the commit it ultimately names.
func peelToCommit(store *ObjectStore, oid string) string {
	current := oid
	for guard := 0; guard < 32; guard++ {
		object, ok := store.Get(current)
		if !ok {
			return current
		}
		if object.Type == "commit" {
			return current
		}
		if object.Type == "tag" {
			current = parseTag(object.Data).Object
			continue
		}
		return current
	}
	return current
}

// peelOne peels one level: a tag's target, for ^{} advertisement lines.
func peelOne(store *ObjectStore, oid string) (string, bool) {
	object, ok := store.Get(oid)
	if !ok || object.Type != "tag" {
		return "", false
	}
	return parseTag(object.Data).Object, true
}

// ---------------------------------------------------------------------------
// reachability

// collectReachable returns every object reachable from roots.
func collectReachable(store *ObjectStore, roots []string) map[string]bool {
	seen := make(map[string]bool)
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] || oid == ZERO_OID {
			continue
		}
		object, ok := store.Get(oid)
		if !ok {
			continue
		}
		seen[oid] = true
		switch object.Type {
		case "commit":
			commit := parseCommit(object.Data)
			queue = append(queue, commit.Tree)
			queue = append(queue, commit.Parents...)
		case "tag":
			queue = append(queue, parseTag(object.Data).Object)
		case "tree":
			for _, entry := range parseTree(object.Data) {
				if entry.Mode == "160000" {
					continue
				}
				queue = append(queue, entry.Oid)
			}
		}
	}
	return seen
}

// commitAncestors is the set of commits reachable from tip (inclusive).
func commitAncestors(store *ObjectStore, tip string) map[string]bool {
	seen := make(map[string]bool)
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		object, ok := store.Get(oid)
		if !ok || object.Type != "commit" {
			continue
		}
		seen[oid] = true
		queue = append(queue, parseCommit(object.Data).Parents...)
	}
	return seen
}

func isAncestor(store *ObjectStore, ancestor string, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	return commitAncestors(store, descendant)[ancestor]
}

// mergeBase finds a merge base: the first commit reachable from b (BFS)
// that is an ancestor of a.
func mergeBase(store *ObjectStore, a string, b string) (string, bool) {
	ancestorsOfA := commitAncestors(store, a)
	seen := make(map[string]bool)
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestorsOfA[oid] {
			return oid, true
		}
		if object, ok := store.Get(oid); ok && object.Type == "commit" {
			queue = append(queue, parseCommit(object.Data).Parents...)
		}
	}
	return "", false
}

// listCommits lists commits in history order from a tip: newest first,
// parents after their children (BFS).
func listCommits(store *ObjectStore, tip string) []string {
	order := []string{}
	seen := make(map[string]bool)
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		object, ok := store.Get(oid)
		if !ok || object.Type != "commit" {
			continue
		}
		seen[oid] = true
		order = append(order, oid)
		queue = append(queue, parseCommit(object.Data).Parents...)
	}
	return order
}

// ---------------------------------------------------------------------------
// tree diffs and merges

type PathChange struct {
	From *string
	To   *string
}

func strPtr(s string) *string { return &s }

// diffTrees computes the path-level diff between two trees.
func diffTrees(store *ObjectStore, fromTree string, toTree string) map[string]PathChange {
	from := flattenTree(store, fromTree, "", nil)
	to := flattenTree(store, toTree, "", nil)
	changes := make(map[string]PathChange)
	for path, oid := range to {
		before, existed := from[path]
		if !existed {
			changes[path] = PathChange{From: nil, To: strPtr(oid)}
		} else if before != oid {
			changes[path] = PathChange{From: strPtr(before), To: strPtr(oid)}
		}
	}
	for path, oid := range from {
		if _, ok := to[path]; !ok {
			changes[path] = PathChange{From: strPtr(oid), To: nil}
		}
	}
	return changes
}

type MergeOutcome struct {
	OK        bool
	Tree      string
	Conflicts []string
}

// threeWayMerge applies the head side's changes (against the merge base)
// onto the base side's tree. A path changed on both sides to different
// results is a conflict.
func threeWayMerge(store *ObjectStore, baseTree string, headTree string, ontoTree string) MergeOutcome {
	headChanges := diffTrees(store, baseTree, headTree)
	ontoChanges := diffTrees(store, baseTree, ontoTree)
	conflicts := []string{}
	for path, change := range headChanges {
		if onto, ok := ontoChanges[path]; ok && !sameOpt(onto.To, change.To) {
			conflicts = append(conflicts, path)
		}
	}
	if len(conflicts) > 0 {
		return MergeOutcome{OK: false, Tree: ontoTree, Conflicts: conflicts}
	}
	flat := flattenTree(store, ontoTree, "", nil)
	for path, change := range headChanges {
		if change.To == nil {
			delete(flat, path)
		} else {
			flat[path] = *change.To
		}
	}
	return MergeOutcome{OK: true, Tree: buildTreeFromFlat(store, flat), Conflicts: []string{}}
}

func sameOpt(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ---------------------------------------------------------------------------
// packfiles

func writePackObjectHeader(objType string, size int) []byte {
	code := typeCode[objType]
	bytesOut := []byte{}
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining = remaining / 16
	for remaining > 0 {
		bytesOut = append(bytesOut, byte(first|0x80))
		first = remaining & 0x7f
		remaining = remaining / 128
	}
	bytesOut = append(bytesOut, byte(first))
	return bytesOut
}

// writePack builds a version-2 pack of whole objects.
func writePack(objects []*GitObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	writeU32(&buf, 2)
	writeU32(&buf, uint32(len(objects)))
	for _, object := range objects {
		buf.Write(writePackObjectHeader(object.Type, len(object.Data)))
		compressor, _ := zlib.NewWriterLevelDict(&buf, zlib.DefaultCompression, nil)
		compressor.Write(object.Data)
		compressor.Close()
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

func writeU32(buf *bytes.Buffer, value uint32) {
	buf.WriteByte(byte(value >> 24))
	buf.WriteByte(byte(value >> 16))
	buf.WriteByte(byte(value >> 8))
	buf.WriteByte(byte(value))
}

func readPackVarintSize(buffer []byte, start int) (size int64, next int, first byte) {
	b := buffer[start]
	first = b
	size = int64(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		b = buffer[offset]
		offset++
		size += int64(b&0x7f) << shift
		shift += 7
		if shift > 63 {
			panic("pack entry size varint is too long")
		}
	}
	return size, offset, first
}

func readOfsDeltaBase(buffer []byte, start int) (baseOffset int64, next int) {
	b := buffer[start]
	offset := start + 1
	value := int64(b & 0x7f)
	for b&0x80 != 0 {
		b = buffer[offset]
		offset++
		value = (value+1)*128 + int64(b&0x7f)
	}
	return value, offset
}

// inflateAt inflates the zlib stream starting at offset, reporting how many
// input bytes it consumed.
func inflateAt(buffer []byte, offset int) ([]byte, int, error) {
	reader := bytes.NewReader(buffer[offset:])
	zr, err := zlib.NewReader(reader)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	if err := zr.Close(); err != nil {
		return nil, 0, err
	}
	consumed := len(buffer[offset:]) - reader.Len()
	return data, offset + consumed, nil
}

// applyDelta applies a git delta instruction stream to a base object's bytes.
func applyDelta(base []byte, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() int64 {
		var value int64
		var shift uint
		for {
			b := delta[offset]
			offset++
			value += int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	sourceSize := readSize()
	targetSize := readSize()
	if sourceSize != int64(len(base)) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	out := make([]byte, 0, targetSize)
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			var copyOffset, copySize int64
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					copyOffset += int64(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					copySize += int64(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			out = append(out, base[copyOffset:copyOffset+copySize]...)
		} else if op != 0 {
			literal := int(op & 0x7f)
			out = append(out, delta[offset:offset+literal]...)
			offset += literal
		} else {
			return nil, fmt.Errorf("delta opcode 0 is invalid")
		}
	}
	if int64(len(out)) != targetSize {
		return nil, fmt.Errorf("delta produced the wrong size")
	}
	return out, nil
}

type rawPackEntry struct {
	offset     int
	typeCode   int
	data       []byte
	baseOffset int
	baseOid    string
	hasBaseOff bool
	hasBaseOid bool
}

// readPackInto reads a pack into a store, resolving OFS_DELTA and REF_DELTA
// entries (a thin pack's base may already live in the store). It returns the
// object ids stored, and panics with an error value on malformed input (the
// receive-pack handler recovers it into the unpack line).
func readPackInto(store *ObjectStore, pack []byte) []string {
	fail := func(message string) {
		panic(packError(message))
	}
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		fail("not a packfile")
	}
	version := uint32(pack[4])<<24 | uint32(pack[5])<<16 | uint32(pack[6])<<8 | uint32(pack[7])
	if version != 2 && version != 3 {
		fail(fmt.Sprintf("unsupported pack version %d", version))
	}
	count := int(uint32(pack[8])<<24 | uint32(pack[9])<<16 | uint32(pack[10])<<8 | uint32(pack[11]))
	trailerStart := len(pack) - 20
	raw := []*rawPackEntry{}
	byOffset := make(map[int]*rawPackEntry)
	offset := 12
	for index := 0; index < count; index++ {
		entryOffset := offset
		size, next, first := readPackVarintSize(pack, offset)
		offset = next
		typeCodeValue := int(first>>4) & 0x07
		entry := &rawPackEntry{offset: entryOffset, typeCode: typeCodeValue}
		if typeCodeValue == 6 {
			baseOffset, after := readOfsDeltaBase(pack, offset)
			offset = after
			entry.baseOffset = entryOffset - int(baseOffset)
			entry.hasBaseOff = true
		} else if typeCodeValue == 7 {
			entry.baseOid = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
			entry.hasBaseOid = true
		} else if _, ok := codeType[typeCodeValue]; !ok {
			fail(fmt.Sprintf("unknown pack type code %d", typeCodeValue))
		}
		data, after, err := inflateAt(pack[:trailerStart], offset)
		if err != nil {
			fail(err.Error())
		}
		if int64(len(data)) != size {
			fail("pack entry size mismatch")
		}
		entry.data = data
		offset = after
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	type resolvedObject struct {
		objType string
		data    []byte
		oid     string
	}
	resolved := make(map[int]*resolvedObject)
	resolving := make(map[int]bool)
	var resolve func(entry *rawPackEntry) *resolvedObject
	resolve = func(entry *rawPackEntry) *resolvedObject {
		if cached, ok := resolved[entry.offset]; ok {
			return cached
		}
		if resolving[entry.offset] {
			fail("delta cycle in pack")
		}
		resolving[entry.offset] = true
		var objType string
		var data []byte
		if entry.typeCode == 6 {
			baseEntry, ok := byOffset[entry.baseOffset]
			if !ok {
				fail("ofs-delta base is not in the pack")
			}
			base := resolve(baseEntry)
			objType = base.objType
			applied, err := applyDelta(base.data, entry.data)
			if err != nil {
				fail(err.Error())
			}
			data = applied
		} else if entry.typeCode == 7 {
			var base *resolvedObject
			for _, candidate := range resolved {
				if candidate.oid == entry.baseOid {
					base = candidate
					break
				}
			}
			var baseType string
			var baseData []byte
			if base != nil {
				baseType, baseData = base.objType, base.data
			} else if stored, ok := store.Get(entry.baseOid); ok {
				baseType, baseData = stored.Type, stored.Data
			} else {
				fail("ref-delta base is neither in the pack nor the store")
			}
			objType = baseType
			applied, err := applyDelta(baseData, entry.data)
			if err != nil {
				fail(err.Error())
			}
			data = applied
		} else {
			objType = codeType[entry.typeCode]
			data = entry.data
		}
		value := &resolvedObject{objType: objType, data: data, oid: hashObject(objType, data)}
		resolved[entry.offset] = value
		delete(resolving, entry.offset)
		return value
	}
	stored := []string{}
	for _, entry := range raw {
		object := resolve(entry)
		if !store.Has(object.oid) {
			store.objects[object.oid] = &GitObject{Type: object.objType, Data: object.data}
		}
		stored = append(stored, object.oid)
	}
	return stored
}

// packError is the panic value readPackInto fails with.
type packError string

func (e packError) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// domain records

type LabelRec struct {
	Name        string
	Color       string
	Description *string
}

type CommentRec struct {
	ID           int
	Body         string
	CreatedEpoch int64
}

type IssueRec struct {
	ID           int
	Number       int
	Title        string
	Body         string
	State        string // "open" | "closed"
	Labels       []string
	CreatedEpoch int64
	UpdatedEpoch int64
	Comments     []*CommentRec
}

type PullRec struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedEpoch    *int64
	MergeCommitSha *string
	HeadRef        string
	BaseRef        string
	CreatedEpoch   int64
	UpdatedEpoch   int64
}

type RepoRec struct {
	ID            int
	Name          string
	Owner         string
	IsPrivate     bool
	Description   string
	DefaultBranch string
	CreatedEpoch  int64
	UpdatedEpoch  int64
	Store         *ObjectStore
	Refs          *OrderedMap[string]
	Labels        *OrderedMap[*LabelRec]
	Issues        map[int]*IssueRec
	Pulls         map[int]*PullRec
	// Issues and pull requests share one number sequence, as on GitHub.
	NextNumber int
}

type Forge struct {
	repos         *OrderedMap[*RepoRec]
	nextRepoID    int
	nextObjectID  int
	nextCommentID int
}

func NewForge() *Forge {
	return &Forge{
		repos:         NewOrderedMap[*RepoRec](),
		nextRepoID:    1,
		nextObjectID:  1,
		nextCommentID: 1,
	}
}

func repoKey(owner string, name string) string {
	return owner + "/" + name
}

func (f *Forge) GetRepo(owner string, name string) (*RepoRec, bool) {
	return f.repos.Get(repoKey(owner, name))
}

func (f *Forge) CreateRepo(name string, isPrivate bool, description string, defaultBranch string) *RepoRec {
	epoch := tickEpochSec()
	repo := &RepoRec{
		ID:            f.nextRepoID,
		Name:          name,
		Owner:         VIEWER.Login,
		IsPrivate:     isPrivate,
		Description:   description,
		DefaultBranch: defaultBranch,
		CreatedEpoch:  epoch,
		UpdatedEpoch:  epoch,
		Store:         NewObjectStore(),
		Refs:          NewOrderedMap[string](),
		Labels:        NewOrderedMap[*LabelRec](),
		Issues:        make(map[int]*IssueRec),
		Pulls:         make(map[int]*PullRec),
		NextNumber:    1,
	}
	f.nextRepoID++
	f.repos.Set(repoKey(repo.Owner, repo.Name), repo)
	return repo
}

func (f *Forge) DeleteRepo(owner string, name string) bool {
	return f.repos.Delete(repoKey(owner, name))
}

// ViewerRepos is the viewer's repositories in creation order.
func (f *Forge) ViewerRepos() []*RepoRec {
	out := []*RepoRec{}
	for _, key := range f.repos.Keys() {
		repo, _ := f.repos.Get(key)
		if repo.Owner == VIEWER.Login {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *Forge) AllocObjectID() int {
	id := f.nextObjectID
	f.nextObjectID++
	return id
}

func (f *Forge) AllocCommentID() int {
	id := f.nextCommentID
	f.nextCommentID++
	return id
}

func (f *Forge) FindRepoByID(id int) *RepoRec {
	for _, key := range f.repos.Keys() {
		repo, _ := f.repos.Get(key)
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

// headRef is the ref a repo's HEAD names: its default branch, born or unborn.
func headRef(repo *RepoRec) string {
	return "refs/heads/" + repo.DefaultBranch
}

func branchTip(repo *RepoRec, branch string) (string, bool) {
	return repo.Refs.Get("refs/heads/" + branch)
}

var hexOidPattern = regexpMustCompileHex()

func regexpMustCompileHex() func(string) bool {
	return func(text string) bool {
		if len(text) != 40 {
			return false
		}
		for i := 0; i < len(text); i++ {
			c := text[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
		return true
	}
}

// resolveRef resolves a REST ref parameter: a branch name, a tag name, or a
// commit id.
func resolveRef(repo *RepoRec, ref string) (string, bool) {
	if hexOidPattern(ref) {
		lower := strings.ToLower(ref)
		if object, ok := repo.Store.Get(lower); ok {
			if object.Type == "tag" {
				return peelToCommit(repo.Store, lower), true
			}
			return lower, true
		}
		return "", false
	}
	if branch, ok := repo.Refs.Get("refs/heads/" + ref); ok {
		return branch, true
	}
	if tag, ok := repo.Refs.Get("refs/tags/" + ref); ok {
		return peelToCommit(repo.Store, tag), true
	}
	if full, ok := repo.Refs.Get(ref); ok {
		return peelToCommit(repo.Store, full), true
	}
	return "", false
}

// countLines counts lines for pull-request file stats.
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := bytes.Count(data, []byte{0x0a})
	if data[len(data)-1] != 0x0a {
		lines++
	}
	return lines
}

// iso is a deterministic ISO timestamp for a stored epoch tick.
func iso(epoch int64) string {
	return isoOf(epoch)
}

// parseIntOr is strconv.Atoi with a fallback.
func parseIntOr(text string, fallback int) int {
	value, err := strconv.Atoi(text)
	if err != nil {
		return fallback
	}
	return value
}
