// Git smart HTTP: info/refs advertisements (protocol v2 for upload-pack
// when the client asks, v0 otherwise; receive-pack is v0 only), v2 ls-refs
// and fetch, v0 upload-pack fetch, and v0 receive-pack pushes with
// report-status over side-band-64k. Every ref update is fast-forward only.
package main

import (
	"bytes"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// gitTarget parses /{owner}/{repo}.git/... into owner, repo and the tail.
func gitTarget(path string) (owner, repo, tail string, ok bool) {
	rest := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || !strings.HasSuffix(parts[1], ".git") {
		return "", "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), parts[2], true
}

func (s *server) serveGit(w http.ResponseWriter, r *http.Request) {
	owner, repo, tail, ok := gitTarget(r.URL.Path)
	if !ok {
		errJSON(w, 404, "Not Found")
		return
	}
	s.store.mu.RLock()
	rep := s.store.lookup(owner, repo)
	s.store.mu.RUnlock()
	if rep == nil {
		// A missing repository is 404 on both services' advertisements; a
		// missing repo on a POST is also 404, and the server stays up.
		errJSON(w, 404, "Not Found")
		return
	}
	switch {
	case r.Method == "GET" && tail == "info/refs":
		s.gitInfoRefs(w, r, rep)
	case r.Method == "POST" && tail == "git-upload-pack":
		s.gitUploadPack(w, r, rep)
	case r.Method == "POST" && tail == "git-receive-pack":
		s.gitReceivePack(w, r, rep)
	default:
		errJSON(w, 404, "Not Found")
	}
}

// sideband wraps payloads as band-1 sideband packets, chunked under the
// pkt-line limit.
func sideband(pack []byte) []byte {
	var buf bytes.Buffer
	for len(pack) > 0 {
		n := len(pack)
		if n > 65515 {
			n = 65515
		}
		chunk := make([]byte, 1+n)
		chunk[0] = 1
		copy(chunk[1:], pack[:n])
		buf.Write(pktLine(chunk))
		pack = pack[n:]
	}
	return buf.Bytes()
}

// refLine renders one v0 advertisement ref line.
func refLine(oid, name string) string { return oid + " " + name + "\n" }

// v0Advertisement builds the v0 ref advertisement for a service, with the
// "# service=..." preamble and a flush separator.
func (s *server) v0Advertisement(rep *Repo, service string, caps string) []byte {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=" + service + "\n")))
	buf.Write(pktFlush())
	refs := rep.refNames()
	// HEAD first, pointing at the default branch tip, with symref capability.
	head := rep.Refs["refs/heads/"+rep.DefaultBranch]
	first := true
	emit := func(oid, name string) {
		line := refLine(oid, name)
		if first {
			line = oid + " " + name + "\x00" + caps + "\n"
			first = false
		}
		buf.Write(pktLine([]byte(line)))
	}
	if head != "" {
		emit(head, "HEAD")
	}
	for _, name := range refs {
		oid := rep.Refs[name]
		emit(oid, name)
		// Annotated tags peel to their commit.
		if strings.HasPrefix(name, "refs/tags/") {
			if obj := rep.Objects[oid]; obj != nil && obj.Type == "tag" {
				if peeled := peelToCommit(oid, func(id string) *Obj { return rep.Objects[id] }); peeled != "" {
					buf.Write(pktLine([]byte(refLine(peeled, name+"^{}"))))
				}
			}
		}
	}
	if first {
		// Empty repository: the capabilities^{} pseudo-ref.
		buf.Write(pktLine([]byte(zeroOID + " capabilities^{}\x00" + caps + "\n")))
	}
	buf.Write(pktFlush())
	return buf.Bytes()
}

func (s *server) gitInfoRefs(w http.ResponseWriter, r *http.Request, rep *Repo) {
	service := r.URL.Query().Get("service")
	wantV2 := r.Header.Get("Git-Protocol") == "version=2"
	if service == "git-upload-pack" {
		if wantV2 {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			var buf bytes.Buffer
			buf.Write(pktLine([]byte("version 2\n")))
			buf.Write(pktLine([]byte("ls-refs=unborn\n")))
			buf.Write(pktLine([]byte("fetch=shallow thin-pack ofs-delta\n")))
			buf.Write(pktLine([]byte("object-format=sha1\n")))
			buf.Write(pktLine([]byte("agent=forge/1.0\n")))
			buf.Write(pktFlush())
			w.WriteHeader(200)
			_, _ = w.Write(buf.Bytes())
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		caps := "side-band-64k thin-pack ofs-delta shallow multi_ack symref=HEAD:refs/heads/" + rep.DefaultBranch + " agent=forge/1.0"
		w.WriteHeader(200)
		_, _ = w.Write(s.v0Advertisement(rep, "git-upload-pack", caps))
		return
	}
	if service == "git-receive-pack" {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		caps := "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"
		w.WriteHeader(200)
		_, _ = w.Write(s.v0Advertisement(rep, "git-receive-pack", caps))
		return
	}
	errJSON(w, 400, "unknown service")
}

// fetchSelection computes the objects a fetch must send, plus the shallow
// boundary lists for the shallow-info section.
//
//   - wants: oids the client asked for.
//   - haves: oids the client already holds.
//   - heldShallow: the client's current shallow boundaries.
//   - depth: 0 means no depth limit.
func (s *server) fetchSelection(rep *Repo, wants, haves, heldShallow []string, depth int) (send []*Obj, shallow, unshallow []string) {
	lookup := func(id string) *Obj { return rep.Objects[id] }
	held := map[string]bool{}
	for _, h := range haves {
		held[h] = true
	}
	heldSet := map[string]bool{}
	if len(haves) > 0 {
		// Objects the client already holds: the closure of its haves,
		// truncated at its shallow boundaries (a shallow boundary's
		// parents are not held).
		heldShallowSet := map[string]bool{}
		for _, b := range heldShallow {
			heldShallowSet[b] = true
		}
		queue := append([]string(nil), haves...)
		for len(queue) > 0 {
			oid := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if oid == zeroOID || heldSet[oid] {
				continue
			}
			obj := lookup(oid)
			if obj == nil {
				continue
			}
			heldSet[oid] = true
			switch obj.Type {
			case "commit":
				cv := parseCommit(obj.Data)
				queue = append(queue, cv.Tree)
				if !heldShallowSet[oid] {
					queue = append(queue, cv.Parents...)
				}
			case "tree":
				for _, e := range parseTree(obj.Data) {
					if e.Mode != "160000" {
						queue = append(queue, e.OID)
					}
				}
			case "tag":
				target, _ := parseTagTarget(obj.Data)
				if target != "" {
					queue = append(queue, target)
				}
			}
		}
	}
	// Wanted closure, honouring depth (depth counts commit generations).
	sendSet := map[string]bool{}
	var boundary []string // deepest included commits, for shallow-info
	if depth > 0 {
		// BFS by commit generation from the wants. A commit contributes
		// itself plus its tree closure only: its parents stay out, or the
		// depth limit would be meaningless.
		type item struct {
			oid string
			gen int
		}
		seenCommit := map[string]bool{}
		queue := []item{}
		for _, w := range wants {
			queue = append(queue, item{w, 1})
		}
		for len(queue) > 0 {
			it := queue[0]
			queue = queue[1:]
			if it.gen > depth || seenCommit[it.oid] {
				continue
			}
			obj := lookup(it.oid)
			if obj == nil {
				continue
			}
			if obj.Type == "tag" {
				// The tag itself travels; its target continues at this depth.
				sendSet[it.oid] = true
				target, _ := parseTagTarget(obj.Data)
				if target != "" {
					queue = append(queue, item{target, it.gen})
				}
				continue
			}
			if obj.Type != "commit" {
				continue
			}
			seenCommit[it.oid] = true
			sendSet[it.oid] = true
			cv := parseCommit(obj.Data)
			collectTreeObjects(cv.Tree, lookup, sendSet)
			if it.gen == depth {
				boundary = append(boundary, it.oid)
			}
			for _, p := range cv.Parents {
				queue = append(queue, item{p, it.gen + 1})
			}
		}
		// A boundary commit whose parents are all included is not shallow.
		// (With depth d the deepest included commits always have an
		// excluded parent, unless they are roots.)
		kept := boundary[:0]
		for _, b := range boundary {
			obj := lookup(b)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			parents := parseCommit(obj.Data).Parents
			if len(parents) == 0 {
				continue
			}
			kept = append(kept, b)
		}
		shallow = kept
		// Old boundaries that are now fully present become unshallow.
		for _, old := range heldShallow {
			if !seenCommit[old] {
				continue
			}
			obj := lookup(old)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			full := true
			for _, p := range parseCommit(obj.Data).Parents {
				if !seenCommit[p] && !heldSet[p] {
					full = false
				}
			}
			if full {
				unshallow = append(unshallow, old)
			}
		}
	} else {
		for o := range collectClosure(wants, lookup, true) {
			sendSet[o] = true
		}
	}
	for oid := range sendSet {
		if heldSet[oid] {
			continue
		}
		if obj := lookup(oid); obj != nil {
			send = append(send, obj)
		}
	}
	// Deterministic pack order: commits, trees, tags, blobs, each by id.
	sort.Slice(send, func(i, j int) bool {
		rank := func(t string) int {
			switch t {
			case "commit":
				return 0
			case "tree":
				return 1
			case "tag":
				return 2
			default:
				return 3
			}
		}
		if rank(send[i].Type) != rank(send[j].Type) {
			return rank(send[i].Type) < rank(send[j].Type)
		}
		return hashObj(send[i].Type, send[i].Data) < hashObj(send[j].Type, send[j].Data)
	})
	sort.Strings(shallow)
	sort.Strings(unshallow)
	return send, shallow, unshallow
}

func (s *server) gitUploadPack(w http.ResponseWriter, r *http.Request, rep *Repo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	isV2 := r.Header.Get("Git-Protocol") == "version=2"
	if isV2 {
		s.uploadPackV2(w, r, rep, body)
	} else {
		s.uploadPackV0(w, r, rep, body)
	}
}

func (s *server) uploadPackV2(w http.ResponseWriter, r *http.Request, rep *Repo, body []byte) {
	pr := &pktReader{buf: body}
	var command string
	var wants, haves, shallow []string
	var depth int
	for {
		kind, payload, ok := pr.next()
		if !ok || kind == "flush" {
			break
		}
		if kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		switch {
		case strings.HasPrefix(line, "command="):
			command = line[8:]
		case strings.HasPrefix(line, "want "):
			wants = append(wants, line[5:])
		case strings.HasPrefix(line, "have "):
			haves = append(haves, line[5:])
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, line[8:])
		case strings.HasPrefix(line, "deepen "):
			if d, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				depth = d
			}
		}
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(200)
	if command == "ls-refs" {
		s.writeLsRefs(w, rep, body)
		return
	}
	if command != "fetch" {
		return
	}
	send, newShallow, unshallow := s.fetchSelection(rep, wants, haves, shallow, depth)
	var buf bytes.Buffer
	// The acknowledgments section only exists when the client negotiated
	// with have lines; a fresh clone expects the next section directly.
	if len(haves) > 0 {
		buf.Write(pktLine([]byte("acknowledgments\n")))
		buf.Write(pktLine([]byte("NAK\n")))
		buf.Write(pktFlush())
	}
	if depth > 0 || len(newShallow) > 0 || len(unshallow) > 0 {
		buf.Write(pktLine([]byte("shallow-info\n")))
		for _, b := range newShallow {
			buf.Write(pktLine([]byte("shallow " + b + "\n")))
		}
		for _, b := range unshallow {
			buf.Write(pktLine([]byte("unshallow " + b + "\n")))
		}
		buf.Write(pktFlush())
	}
	buf.Write(pktLine([]byte("packfile\n")))
	buf.Write(sideband(writePack(send)))
	buf.Write(pktFlush())
	_, _ = w.Write(buf.Bytes())
}

// writeLsRefs answers a v2 ls-refs command, honouring peel, symrefs,
// unborn and ref-prefix arguments.
func (s *server) writeLsRefs(w http.ResponseWriter, rep *Repo, body []byte) {
	pr := &pktReader{buf: body}
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	seenDelim := false
	for {
		kind, payload, ok := pr.next()
		if !ok || kind == "flush" {
			break
		}
		if kind == "delim" {
			seenDelim = true
			continue
		}
		if kind != "data" || !seenDelim {
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[11:])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var buf bytes.Buffer
	lookup := func(id string) *Obj { return rep.Objects[id] }
	headTarget := "refs/heads/" + rep.DefaultBranch
	headOID := rep.Refs[headTarget]
	if keep("HEAD") && symrefs {
		if headOID != "" {
			buf.Write(pktLine([]byte(headOID + " HEAD symref-target:" + headTarget + "\n")))
		} else if unborn {
			buf.Write(pktLine([]byte("unborn HEAD symref-target:" + headTarget + "\n")))
		}
	}
	for _, name := range rep.refNames() {
		if !keep(name) {
			continue
		}
		oid := rep.Refs[name]
		line := oid + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if obj := rep.Objects[oid]; obj != nil && obj.Type == "tag" {
				if peeled := peelToCommit(oid, lookup); peeled != "" {
					line += " peeled:" + peeled
				}
			}
		}
		buf.Write(pktLine([]byte(line + "\n")))
	}
	// An empty repository advertises only the unborn HEAD line above: an
	// "unborn refs/heads/<branch>" line would be read as a real ref.
	buf.Write(pktFlush())
	_, _ = w.Write(buf.Bytes())
}

func (s *server) uploadPackV0(w http.ResponseWriter, r *http.Request, rep *Repo, body []byte) {
	pr := &pktReader{buf: body}
	var wants, haves, shallow []string
	var depth int
	section := 0
	for {
		kind, payload, ok := pr.next()
		if !ok {
			break
		}
		if kind == "flush" {
			section++
			continue
		}
		if kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			fields := strings.Fields(line[5:])
			if len(fields) > 0 {
				wants = append(wants, fields[0])
			}
		case strings.HasPrefix(line, "have "):
			haves = append(haves, line[5:])
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, line[8:])
		case strings.HasPrefix(line, "deepen "):
			if d, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				depth = d
			}
		case line == "done":
		}
		_ = section
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(200)
	send, newShallow, unshallow := s.fetchSelection(rep, wants, haves, shallow, depth)
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("NAK\n")))
	for _, b := range newShallow {
		buf.Write(pktLine([]byte("shallow " + b + "\n")))
	}
	for _, b := range unshallow {
		buf.Write(pktLine([]byte("unshallow " + b + "\n")))
	}
	buf.Write(sideband(writePack(send)))
	buf.Write(pktFlush())
	_, _ = w.Write(buf.Bytes())
}

// gitReceivePack handles a v0 receive-pack push: command list, pack, then a
// report-status answer over side-band-64k.
func (s *server) gitReceivePack(w http.ResponseWriter, r *http.Request, rep *Repo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	pr := &pktReader{buf: body}
	type cmd struct{ old, new, ref string }
	var cmds []cmd
	for {
		kind, payload, ok := pr.next()
		if !ok || kind == "flush" {
			break
		}
		if kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if idx := strings.IndexByte(line, 0); idx >= 0 {
			line = line[:idx]
		}
		fields := strings.Fields(line)
		if len(fields) == 3 {
			cmds = append(cmds, cmd{fields[0], fields[1], fields[2]})
		}
	}
	packBytes := pr.rest()

	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	lookup := func(id string) *Obj { return rep.Objects[id] }

	// Unpack first: parse the pack (when one is present) and store objects.
	unpackOK := true
	unpackDetail := "ok"
	if len(packBytes) > 0 {
		objs, ok := readPack(packBytes, lookup)
		if !ok {
			unpackOK = false
			unpackDetail = "unpack error"
		} else {
			for oid, obj := range objs {
				rep.Objects[oid] = obj
			}
		}
	}
	// An update-only push with no pack at all is an unpack error; a
	// delete-only push carries no pack and is fine.
	needsPack := false
	for _, c := range cmds {
		if c.new != zeroOID {
			needsPack = true
		}
	}
	if needsPack && len(packBytes) == 0 {
		unpackOK = false
		unpackDetail = "unpack error"
	}

	// Apply each ref update, fast-forward only, per ref.
	type outcome struct {
		ref    string
		ok     bool
		reason string
	}
	results := make([]outcome, 0, len(cmds))
	for _, c := range cmds {
		if !unpackOK {
			results = append(results, outcome{c.ref, false, "unpack error"})
			continue
		}
		current, exists := rep.Refs[c.ref]
		switch {
		case c.new == zeroOID: // delete
			if !exists {
				results = append(results, outcome{c.ref, false, "ref does not exist"})
			} else if c.old != zeroOID && c.old != current {
				results = append(results, outcome{c.ref, false, "non-fast-forward"})
			} else {
				delete(rep.Refs, c.ref)
				results = append(results, outcome{c.ref, true, ""})
			}
		case c.old == zeroOID: // create
			if exists {
				results = append(results, outcome{c.ref, false, "already exists"})
			} else if rep.Objects[c.new] == nil {
				results = append(results, outcome{c.ref, false, "missing object"})
			} else {
				rep.Refs[c.ref] = c.new
				results = append(results, outcome{c.ref, true, ""})
			}
		default: // update
			if !exists || current != c.old {
				results = append(results, outcome{c.ref, false, "non-fast-forward"})
			} else if rep.Objects[c.new] == nil {
				results = append(results, outcome{c.ref, false, "missing object"})
			} else if !s.fastForward(rep, c.old, c.new) {
				results = append(results, outcome{c.ref, false, "non-fast-forward"})
			} else {
				rep.Refs[c.ref] = c.new
				results = append(results, outcome{c.ref, true, ""})
			}
		}
	}

	// Report-status, pkt-lined inside band 1.
	var report bytes.Buffer
	report.Write(pktLine([]byte("unpack " + unpackDetail + "\n")))
	for _, res := range results {
		if res.ok {
			report.Write(pktLine([]byte("ok " + res.ref + "\n")))
		} else {
			report.Write(pktLine([]byte("ng " + res.ref + " " + res.reason + "\n")))
		}
	}
	report.Write(pktFlush())
	var out bytes.Buffer
	for len(report.Bytes()) > 0 {
		chunk := report.Bytes()
		n := len(chunk)
		if n > 65515 {
			n = 65515
		}
		band := make([]byte, 1+n)
		band[0] = 1
		copy(band[1:], chunk[:n])
		out.Write(pktLine(band))
		report.Next(n)
	}
	// A top-level flush ends the sideband stream; without it a real git
	// client reports an unexpected disconnect.
	out.Write(pktFlush())
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(200)
	_, _ = w.Write(out.Bytes())
}

// fastForward reports whether new descends from old (both commits), or
// whether the update is a tag/object update that needs no ancestry check.
func (s *server) fastForward(rep *Repo, old, new string) bool {
	newObj := rep.Objects[new]
	oldObj := rep.Objects[old]
	if newObj == nil || oldObj == nil {
		return false
	}
	if newObj.Type != "commit" || oldObj.Type != "commit" {
		// Tag or other object updates move freely once the old value matched.
		return true
	}
	return isAncestor(old, new, func(id string) *Obj { return rep.Objects[id] })
}

