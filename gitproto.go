// gitproto.go — the git smart-HTTP protocol: ref advertisement (v0 and v2),
// ls-refs, fetch (v2 sections and the v0 sideband form, with shallow deepen
// support) and receive-pack with ref update validation and report-status.

package main

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const gitAgent = "vinz-forge/1.0"
const pktMaxPayload = 65516

// ---------------------------------------------------------------------------
// pkt-line framing

func pktLine(payload []byte) []byte {
	header := fmt.Sprintf("%04x", len(payload)+4)
	return append([]byte(header), payload...)
}

func pktLineText(text string) []byte {
	return pktLine([]byte(text))
}

var pktFlush = []byte("0000")

type pktFrame struct {
	kind    string // "data" | "flush" | "delim" | "end"
	payload []byte
}

var pktHeaderPattern = regexp.MustCompile(`^[0-9a-fA-F]{4}$`)

// parsePacketStream parses pkt-lines; it returns the frames and the offset
// of the first unconsumed byte.
func parsePacketStream(buffer []byte) ([]pktFrame, int) {
	frames := []pktFrame{}
	cursor := 0
	for cursor+4 <= len(buffer) {
		header := string(buffer[cursor : cursor+4])
		if !pktHeaderPattern.MatchString(header) {
			break
		}
		length, _ := strconv.ParseUint(header, 16, 32)
		if length == 0 {
			frames = append(frames, pktFrame{kind: "flush"})
			cursor += 4
			continue
		}
		if length == 1 {
			frames = append(frames, pktFrame{kind: "delim"})
			cursor += 4
			continue
		}
		if length == 2 {
			frames = append(frames, pktFrame{kind: "end"})
			cursor += 4
			continue
		}
		if length < 4 || cursor+int(length) > len(buffer) {
			break
		}
		frames = append(frames, pktFrame{kind: "data", payload: buffer[cursor+4 : cursor+int(length)]})
		cursor += int(length)
	}
	return frames, cursor
}

// parseGroups splits a request body into argument groups: delim and flush
// packets both start a new group; trailing empty groups are dropped (but at
// least one group always remains).
func parseGroups(buffer []byte) [][][]byte {
	frames, _ := parsePacketStream(buffer)
	groups := [][][]byte{{}}
	for _, frame := range frames {
		if frame.kind == "data" {
			groups[len(groups)-1] = append(groups[len(groups)-1], frame.payload)
		} else if frame.kind == "delim" || frame.kind == "flush" {
			groups = append(groups, [][]byte{})
		}
	}
	for len(groups) > 1 && len(groups[len(groups)-1]) == 0 {
		groups = groups[:len(groups)-1]
	}
	return groups
}

// linesOf maps each packet payload to a string with one trailing \n stripped
// (the TS linesOf: one entry per packet, never split further).
func linesOf(group [][]byte) []string {
	lines := make([]string, 0, len(group))
	for _, payload := range group {
		lines = append(lines, strings.TrimSuffix(string(payload), "\n"))
	}
	return lines
}

func groupAt(groups [][][]byte, index int) [][]byte {
	if index < len(groups) {
		return groups[index]
	}
	return [][]byte{}
}

// sidebandPackets chunks a pack into band-1 pkt-lines (an empty pack still
// gets one band-only packet).
func sidebandPackets(pack []byte) []byte {
	var out bytes.Buffer
	chunkSize := pktMaxPayload - 1
	if len(pack) == 0 {
		out.Write(pktLine([]byte{1}))
		return out.Bytes()
	}
	for offset := 0; offset < len(pack); offset += chunkSize {
		end := offset + chunkSize
		if end > len(pack) {
			end = len(pack)
		}
		chunk := append([]byte{1}, pack[offset:end]...)
		out.Write(pktLine(chunk))
	}
	return out.Bytes()
}

// sidebandWrap chunks an already-framed stream into band-1 pkt-lines.
func sidebandWrap(stream []byte) []byte {
	var out bytes.Buffer
	chunkSize := pktMaxPayload - 1
	for offset := 0; offset < len(stream); offset += chunkSize {
		end := offset + chunkSize
		if end > len(stream) {
			end = len(stream)
		}
		chunk := append([]byte{1}, stream[offset:end]...)
		out.Write(pktLine(chunk))
	}
	return out.Bytes()
}

// ---------------------------------------------------------------------------
// ref advertisement

type refLine struct {
	oid  string
	name string
}

func sortedRefLines(repo *RepoRec) []refLine {
	names := repo.Refs.Keys()
	sort.Strings(names)
	lines := []refLine{}
	for _, name := range names {
		oid, _ := repo.Refs.Get(name)
		lines = append(lines, refLine{oid: oid, name: name})
		if object, ok := repo.Store.Get(oid); ok && object.Type == "tag" {
			lines = append(lines, refLine{oid: peelToCommit(repo.Store, oid), name: name + "^{}"})
		}
	}
	return lines
}

func uploadCaps(repo *RepoRec) string {
	return strings.Join([]string{
		"multi_ack_detailed",
		"side-band-64k",
		"thin-pack",
		"ofs-delta",
		"shallow",
		"deepen-since",
		"deepen-not",
		"agent=" + gitAgent,
		"symref=HEAD:" + headRef(repo),
	}, " ")
}

func receiveCaps(repo *RepoRec) string {
	return strings.Join([]string{
		"report-status",
		"side-band-64k",
		"delete-refs",
		"ofs-delta",
		"agent=" + gitAgent,
		"symref=HEAD:" + headRef(repo),
	}, " ")
}

func advertiseV0(repo *RepoRec, service string) *Reply {
	caps := uploadCaps(repo)
	if service == "git-receive-pack" {
		caps = receiveCaps(repo)
	}
	var out bytes.Buffer
	out.Write(pktLineText("# service=" + service + "\n"))
	out.Write(pktFlush)
	head := headRef(repo)
	headTip, hasHeadTip := repo.Refs.Get(head)
	refLines := sortedRefLines(repo)
	capsPending := true
	emit := func(text string) {
		if capsPending {
			out.Write(pktLineText(text + "\x00" + caps))
			capsPending = false
		} else {
			out.Write(pktLineText(text))
		}
	}
	if hasHeadTip {
		emit(headTip + " HEAD")
	} else if len(refLines) > 0 {
		emit(ZERO_OID + " HEAD")
	}
	for _, line := range refLines {
		emit(line.oid + " " + line.name)
	}
	if capsPending {
		out.Write(pktLineText(ZERO_OID + " capabilities^{}\x00" + caps))
	}
	out.Write(pktFlush)
	return &Reply{
		Status:      200,
		ContentType: "application/x-" + service + "-advertisement",
		Body:        out.Bytes(),
		Headers:     map[string]string{"cache-control": "no-cache, max-age=0, must-revalidate"},
	}
}

func advertiseV2(service string) *Reply {
	var out bytes.Buffer
	out.Write(pktLineText("version 2\n"))
	out.Write(pktLineText("agent=" + gitAgent + "\n"))
	out.Write(pktLineText("ls-refs=unborn\n"))
	out.Write(pktLineText("fetch=shallow wait-for-done\n"))
	out.Write(pktLineText("object-format=sha1\n"))
	out.Write(pktFlush)
	return &Reply{
		Status:      200,
		ContentType: "application/x-" + service + "-advertisement",
		Body:        out.Bytes(),
		Headers:     map[string]string{"cache-control": "no-cache, max-age=0, must-revalidate"},
	}
}

// ---------------------------------------------------------------------------
// ls-refs

func handleLsRefs(repo *RepoRec, groups [][][]byte) *Reply {
	args := linesOf(groupAt(groups, 1))
	wantPeel := containsString(args, "peel")
	wantSymrefs := containsString(args, "symrefs")
	wantUnborn := containsString(args, "unborn")
	prefixes := []string{}
	for _, line := range args {
		if strings.HasPrefix(line, "ref-prefix ") {
			prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var out bytes.Buffer
	head := headRef(repo)
	headTip, hasHeadTip := repo.Refs.Get(head)
	if matches("HEAD") {
		if hasHeadTip {
			line := headTip + " HEAD"
			if wantSymrefs {
				line += " symref-target:" + head
			}
			out.Write(pktLineText(line + "\n"))
		} else if wantUnborn {
			line := "unborn HEAD"
			if wantSymrefs {
				line += " symref-target:" + head
			}
			out.Write(pktLineText(line + "\n"))
		}
	}
	names := repo.Refs.Keys()
	sort.Strings(names)
	for _, name := range names {
		if !matches(name) {
			continue
		}
		oid, _ := repo.Refs.Get(name)
		suffix := ""
		if wantPeel {
			if object, ok := repo.Store.Get(oid); ok && object.Type == "tag" {
				suffix = " peeled:" + peelToCommit(repo.Store, oid)
			}
		}
		out.Write(pktLineText(oid + " " + name + suffix + "\n"))
	}
	out.Write(pktFlush)
	return &Reply{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: out.Bytes(), Headers: map[string]string{}}
}

func containsString(list []string, target string) bool {
	for _, entry := range list {
		if entry == target {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// fetch planning

// treeClosure adds a commit and its full tree closure to into.
func treeClosure(repo *RepoRec, commitOid string, into map[string]bool) {
	commit, ok := repo.Store.Get(commitOid)
	if !ok || commit.Type != "commit" {
		return
	}
	into[commitOid] = true
	tree := parseCommit(commit.Data).Tree
	for oid := range collectReachable(repo.Store, []string{tree}) {
		into[oid] = true
	}
}

type fetchPlan struct {
	send      []*GitObject
	shallow   []string
	unshallow []string
	ack       string
	hasAck    bool
}

// planFetch decides what a fetch sends. depth is nil for a full fetch.
func planFetch(repo *RepoRec, wants []string, haves []string, shallowLines []string, depth *float64) fetchPlan {
	store := repo.Store
	shallowSet := make(map[string]bool)
	for _, oid := range shallowLines {
		shallowSet[oid] = true
	}
	held := make(map[string]bool)
	for _, have := range haves {
		object, ok := store.Get(have)
		if !ok {
			continue
		}
		if shallowSet[have] && object.Type == "commit" {
			treeClosure(repo, have, held)
		} else {
			for oid := range collectReachable(store, []string{have}) {
				held[oid] = true
			}
		}
	}
	ack := ""
	hasAck := false
	for _, have := range haves {
		if store.Has(have) {
			ack = have
			hasAck = true
		}
	}
	sendOids := make(map[string]bool)
	newShallow := []string{}
	unshallow := []string{}
	if depth != nil {
		// Breadth-first by generation: a want is generation 1.
		generation := make(map[string]int)
		type queueEntry struct {
			oid string
			gen int
		}
		queue := []queueEntry{}
		for _, want := range wants {
			if store.Has(want) {
				queue = append(queue, queueEntry{oid: want, gen: 1})
			}
		}
		included := make(map[string]bool)
		includedOrder := []string{}
		for len(queue) > 0 {
			entry := queue[0]
			queue = queue[1:]
			if _, seen := generation[entry.oid]; seen || float64(entry.gen) > *depth {
				continue
			}
			generation[entry.oid] = entry.gen
			if !included[entry.oid] {
				includedOrder = append(includedOrder, entry.oid)
			}
			included[entry.oid] = true
			if object, ok := store.Get(entry.oid); ok && object.Type == "commit" {
				for _, parent := range parseCommit(object.Data).Parents {
					queue = append(queue, queueEntry{oid: parent, gen: entry.gen + 1})
				}
			}
		}
		closure := make(map[string]bool)
		for _, oid := range includedOrder {
			object, ok := store.Get(oid)
			if !ok {
				continue
			}
			if object.Type == "commit" {
				treeClosure(repo, oid, closure)
			} else {
				for sub := range collectReachable(store, []string{oid}) {
					closure[sub] = true
				}
			}
		}
		for oid := range closure {
			if !held[oid] {
				sendOids[oid] = true
			}
		}
		for _, oid := range includedOrder {
			object, ok := store.Get(oid)
			if !ok || object.Type != "commit" {
				continue
			}
			boundary := false
			for _, parent := range parseCommit(object.Data).Parents {
				if store.Has(parent) && !included[parent] {
					boundary = true
					break
				}
			}
			if boundary && !held[oid] {
				newShallow = append(newShallow, oid)
			} else if boundary && held[oid] && shallowSet[oid] {
				newShallow = append(newShallow, oid)
			}
			if shallowSet[oid] && !boundary {
				unshallow = append(unshallow, oid)
			}
		}
	} else {
		existingWants := []string{}
		for _, want := range wants {
			if store.Has(want) {
				existingWants = append(existingWants, want)
			}
		}
		for oid := range collectReachable(store, existingWants) {
			if !held[oid] {
				sendOids[oid] = true
			}
		}
	}
	send := []*GitObject{}
	for oid := range sendOids {
		if object, ok := store.Get(oid); ok {
			send = append(send, object)
		}
	}
	return fetchPlan{send: send, shallow: newShallow, unshallow: unshallow, ack: ack, hasAck: hasAck}
}

// ---------------------------------------------------------------------------
// fetch (v2 and v0)

func parseFetchArgs(lines []string, v2 bool) (wants []string, haves []string, shallow []string, depth *float64) {
	wants = []string{}
	haves = []string{}
	shallow = []string{}
	for _, line := range lines {
		if strings.HasPrefix(line, "want ") {
			want := strings.TrimSpace(line[5:])
			if !v2 {
				fields := strings.Fields(want)
				want = ""
				if len(fields) > 0 {
					want = fields[0]
				}
			}
			wants = append(wants, want)
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "shallow ") {
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		} else if strings.HasPrefix(line, "deepen ") {
			if value, ok := jsNumberParse(strings.TrimSpace(line[7:])); ok {
				depth = &value
			} else {
				nan := nanFloat()
				depth = &nan
			}
		}
	}
	return wants, haves, shallow, depth
}

func handleFetchV2(repo *RepoRec, groups [][][]byte) *Reply {
	args := linesOf(groupAt(groups, 1))
	wants, haves, shallow, depth := parseFetchArgs(args, true)
	plan := planFetch(repo, wants, haves, shallow, depth)
	var out bytes.Buffer
	out.Write(pktLineText("acknowledgments\n"))
	if plan.hasAck {
		out.Write(pktLineText("ACK " + plan.ack + "\n"))
	} else {
		out.Write(pktLineText("NAK\n"))
	}
	out.Write(pktFlush)
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		out.Write(pktLineText("shallow-info\n"))
		for _, oid := range plan.shallow {
			out.Write(pktLineText("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			out.Write(pktLineText("unshallow " + oid + "\n"))
		}
		out.Write(pktFlush)
	}
	out.Write(pktLineText("packfile\n"))
	out.Write(sidebandPackets(writePack(plan.send)))
	out.Write(pktFlush)
	return &Reply{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: out.Bytes(), Headers: map[string]string{}}
}

func handleFetchV0(repo *RepoRec, groups [][][]byte) *Reply {
	first := linesOf(groupAt(groups, 0))
	second := linesOf(groupAt(groups, 1))
	wants, _, shallow, depth := parseFetchArgs(first, false)
	_, haves, _, _ := parseFetchArgs(second, false)
	plan := planFetch(repo, wants, haves, shallow, depth)
	var out bytes.Buffer
	for _, oid := range plan.shallow {
		out.Write(pktLineText("shallow " + oid + "\n"))
	}
	for _, oid := range plan.unshallow {
		out.Write(pktLineText("unshallow " + oid + "\n"))
	}
	if plan.hasAck {
		out.Write(pktLineText("ACK " + plan.ack + "\n"))
	} else {
		out.Write(pktLineText("NAK\n"))
	}
	out.Write(sidebandPackets(writePack(plan.send)))
	out.Write(pktFlush)
	return &Reply{Status: 200, ContentType: "application/x-git-upload-pack-result", Body: out.Bytes(), Headers: map[string]string{}}
}

func handleUploadPack(repo *RepoRec, body []byte) *Reply {
	groups := parseGroups(body)
	command := ""
	for _, line := range linesOf(groupAt(groups, 0)) {
		if strings.HasPrefix(line, "command=") {
			command = strings.TrimPrefix(line, "command=")
			break
		}
	}
	if command == "ls-refs" {
		return handleLsRefs(repo, groups)
	}
	if command == "fetch" {
		return handleFetchV2(repo, groups)
	}
	return handleFetchV0(repo, groups)
}

// ---------------------------------------------------------------------------
// receive-pack

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

var pushOidPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// applyPushCommand validates and applies one ref update, returning "" on
// success or the refusal reason.
func applyPushCommand(repo *RepoRec, command pushCommand) string {
	current, exists := repo.Refs.Get(command.ref)
	if command.newOid == ZERO_OID {
		// A deletion.
		if !exists {
			return "reference not found"
		}
		if command.ref == headRef(repo) {
			return "deletion of the current branch prohibited"
		}
		if current != command.oldOid {
			return "stale info"
		}
		repo.Refs.Delete(command.ref)
		return ""
	}
	object, ok := repo.Store.Get(command.newOid)
	if !ok {
		return "invalid object"
	}
	if command.oldOid == ZERO_OID {
		if exists {
			return "already exists"
		}
		repo.Refs.Set(command.ref, command.newOid)
		return ""
	}
	if current != command.oldOid {
		return "stale info"
	}
	if strings.HasPrefix(command.ref, "refs/heads/") {
		oldObject, oldOk := repo.Store.Get(command.oldOid)
		if oldOk && oldObject.Type == "commit" && object.Type == "commit" {
			if !isAncestor(repo.Store, command.oldOid, command.newOid) {
				return "non-fast-forward"
			}
		}
	}
	repo.Refs.Set(command.ref, command.newOid)
	return ""
}

func handleReceivePack(repo *RepoRec, body []byte) *Reply {
	// Command pkt-lines run to the first flush; everything after is the pack.
	frames, _ := parsePacketStream(body)
	commandBytes := 0
	{
		cursor := 0
		for _, frame := range frames {
			if frame.kind == "data" {
				cursor += 4 + len(frame.payload)
			} else {
				cursor += 4
				if frame.kind == "flush" {
					break
				}
			}
		}
		commandBytes = cursor
	}
	commandPayloads := [][]byte{}
	for _, frame := range frames {
		if frame.kind == "flush" {
			break
		}
		if frame.kind == "data" {
			commandPayloads = append(commandPayloads, frame.payload)
		}
	}
	rawLines := linesOf(commandPayloads)
	commands := []pushCommand{}
	capabilities := ""
	for index, line := range rawLines {
		text := line
		if index == 0 {
			if nul := strings.IndexByte(text, 0); nul >= 0 {
				capabilities = text[nul+1:]
				text = text[:nul]
			}
		}
		parts := strings.Split(text, " ")
		if len(parts) >= 3 && pushOidPattern.MatchString(parts[0]) && pushOidPattern.MatchString(parts[1]) {
			commands = append(commands, pushCommand{
				oldOid: strings.ToLower(parts[0]),
				newOid: strings.ToLower(parts[1]),
				ref:    strings.Join(parts[2:], " "),
			})
		}
	}
	sideband := false
	for _, capability := range strings.Split(capabilities, " ") {
		if capability == "side-band-64k" {
			sideband = true
		}
	}

	var report bytes.Buffer
	pack := body[commandBytes:]
	unpackOk := true
	unpackDetail := "ok"
	if len(pack) > 0 {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					unpackOk = false
					if err, isErr := recovered.(error); isErr {
						unpackDetail = err.Error()
					} else {
						unpackDetail = "index-pack failed"
					}
				}
			}()
			readPackInto(repo.Store, pack)
		}()
	}
	if unpackOk {
		report.Write(pktLineText("unpack ok\n"))
	} else {
		report.Write(pktLineText("unpack " + unpackDetail + "\n"))
	}
	if unpackOk {
		for _, command := range commands {
			if reason := applyPushCommand(repo, command); reason != "" {
				report.Write(pktLineText("ng " + command.ref + " " + reason + "\n"))
			} else {
				report.Write(pktLineText("ok " + command.ref + "\n"))
			}
		}
	}
	report.Write(pktFlush)
	reportStream := report.Bytes()

	if sideband {
		var out bytes.Buffer
		out.Write(sidebandWrap(reportStream))
		out.Write(pktFlush)
		return &Reply{Status: 200, ContentType: "application/x-git-receive-pack-result", Body: out.Bytes(), Headers: map[string]string{}}
	}
	return &Reply{Status: 200, ContentType: "application/x-git-receive-pack-result", Body: reportStream, Headers: map[string]string{}}
}

// ---------------------------------------------------------------------------
// entry point

var gitProtocolV2Pattern = regexp.MustCompile(`(?:^|,)version=2(?:,|$)`)

// HandleGit is the git smart-HTTP entry point.
func HandleGit(ctx *ReqCtx, owner string, repoName string, action string) *Reply {
	if !ctx.Authorized {
		return MessageReply(401, "Bad credentials")
	}
	repo, ok := theForge.GetRepo(owner, repoName)
	if !ok {
		return &Reply{Status: 404, ContentType: "text/plain; charset=utf-8", Body: []byte("not found\n"), Headers: map[string]string{}}
	}
	if action == "info/refs" {
		service := ctx.Query.Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return &Reply{Status: 400, ContentType: "text/plain; charset=utf-8", Body: []byte("missing service\n"), Headers: map[string]string{}}
		}
		protocol := ctx.Headers["git-protocol"]
		if service == "git-upload-pack" && gitProtocolV2Pattern.MatchString(protocol) {
			return advertiseV2(service)
		}
		return advertiseV0(repo, service)
	}
	if action == "git-upload-pack" && ctx.Method == "POST" {
		return handleUploadPack(repo, ctx.Body)
	}
	if action == "git-receive-pack" && ctx.Method == "POST" {
		return handleReceivePack(repo, ctx.Body)
	}
	return &Reply{Status: 404, ContentType: "text/plain; charset=utf-8", Body: []byte("not found\n"), Headers: map[string]string{}}
}
