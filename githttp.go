package main

import (
	"bytes"
	"net/http"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// pkt-line helpers for advertisements
// ---------------------------------------------------------------------------

func refLines(r *Repo, caps string, first bool) [][]byte {
	var lines [][]byte
	refs := sortedRefs(r)
	// HEAD first, carrying the capabilities on its line.
	if tip := r.tip(r.DefaultBranch); tip != "" {
		head := pktLine([]byte(tip + " HEAD\x00" + caps + " symref=HEAD:refs/heads/" + r.DefaultBranch + "\n"))
		lines = append(lines, head)
	} else if first {
		lines = append(lines, pktLine([]byte(zeroOID+" capabilities^{}\x00"+caps+"\n")))
		return lines
	}
	for i, name := range refs {
		oid := r.Refs[name]
		if i == 0 && len(lines) == 0 {
			lines = append(lines, pktLine([]byte(oid+" "+name+"\x00"+caps+"\n")))
		} else {
			lines = append(lines, pktLine([]byte(oid+" "+name+"\n")))
		}
		// Peeled line for annotated tags.
		if strings.HasPrefix(name, "refs/tags/") {
			if o := r.Objects[oid]; o != nil && o.Typ == "tag" {
				target := tagTarget(o.Data)
				if target != "" {
					lines = append(lines, pktLine([]byte(target+" "+name+"^{}\n")))
				}
			}
		}
	}
	return lines
}

func sortedRefs(r *Repo) []string {
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func tagTarget(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
		if line == "" {
			break
		}
	}
	return ""
}

func isV2(r *http.Request) bool {
	return r.Header.Get("Git-Protocol") == "version=2"
}

// ---------------------------------------------------------------------------
// info/refs
// ---------------------------------------------------------------------------

func gitInfoRefs(w http.ResponseWriter, r *http.Request, owner, name, service string) {
	store.mu.RLock()
	repo := store.lookup(owner, name)
	store.mu.RUnlock()
	if repo == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if service == "git-upload-pack" && isV2(r) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		var buf bytes.Buffer
		for _, l := range []string{
			"version 2\n", "agent=forge/1.0\n", "ls-refs=unborn\n",
			"fetch=shallow wait-for-done filter\n", "server-option\n", "object-format=sha1\n",
		} {
			buf.Write(pktLine([]byte(l)))
		}
		buf.Write(pktFlush)
		w.Write(buf.Bytes())
		return
	}
	if service == "git-upload-pack" {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	} else {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	}
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	preamble := "# service=" + service + "\n"
	buf.Write(pktLine([]byte(preamble)))
	buf.Write(pktFlush)
	store.mu.RLock()
	defer store.mu.RUnlock()
	caps := "side-band-64k thin-pack ofs-delta agent=forge/1.0"
	if service == "git-receive-pack" {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"
	}
	for _, l := range refLines(repo, caps, true) {
		buf.Write(l)
	}
	buf.Write(pktFlush)
	w.Write(buf.Bytes())
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

// fetchPlan is the object set a fetch must send.
type fetchPlan struct {
	objects   []*Obj
	shallow   []string
	unshallow []string
}

func planFetch(r *Repo, wants, haves []string, oldShallow []string, depth int) *fetchPlan {
	plan := &fetchPlan{}
	haveReach := map[string]bool{}
	if len(haves) > 0 {
		haveReach = reachableCommits(r, haves)
	}
	included := map[string]bool{}
	if depth > 0 {
		dist := commitDistance(r, wants)
		for oid, d := range dist {
			if d < depth && !haveReach[oid] {
				included[oid] = true
			}
		}
	} else {
		for oid := range reachableCommits(r, wants) {
			if !haveReach[oid] {
				included[oid] = true
			}
		}
	}
	// Shallow boundary: included commits with a parent outside the set.
	for oid := range included {
		o := r.Objects[oid]
		if o == nil || o.Typ != "commit" {
			continue
		}
		for _, p := range parseCommit(o.Data).Parents {
			if !included[p] {
				plan.shallow = append(plan.shallow, oid)
				break
			}
		}
	}
	sort.Strings(plan.shallow)
	shallowSet := map[string]bool{}
	for _, s := range plan.shallow {
		shallowSet[s] = true
	}
	for _, old := range oldShallow {
		if included[old] && !shallowSet[old] {
			plan.unshallow = append(plan.unshallow, old)
		}
	}
	sort.Strings(plan.unshallow)
	// Objects: included commits, their tree closure, and wanted tag objects.
	seen := map[string]bool{}
	var add func(oid string)
	add = func(oid string) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		o := r.Objects[oid]
		if o == nil {
			return
		}
		plan.objects = append(plan.objects, o)
		if o.Typ == "commit" {
			add(parseCommit(o.Data).Tree)
		} else if o.Typ == "tree" {
			for _, e := range parseTree(o.Data) {
				add(e.OID)
			}
		} else if o.Typ == "tag" {
			if t := tagTarget(o.Data); t != "" {
				add(t)
			}
		}
	}
	for oid := range included {
		add(oid)
	}
	for _, w := range wants {
		o := r.Objects[w]
		if o == nil {
			continue
		}
		if o.Typ != "commit" {
			add(w)
		}
	}
	return plan
}

func sidebandPack(pack []byte) []byte {
	var buf bytes.Buffer
	const maxChunk = 65516
	for len(pack) > 0 {
		n := len(pack)
		if n > maxChunk {
			n = maxChunk
		}
		payload := make([]byte, 1+n)
		payload[0] = 1
		copy(payload[1:], pack[:n])
		buf.Write(pktLine(payload))
		pack = pack[n:]
	}
	if buf.Len() == 0 {
		payload := []byte{1}
		buf.Write(pktLine(payload))
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

func gitUploadPack(w http.ResponseWriter, r *http.Request, owner, name string, body []byte) {
	store.mu.RLock()
	repo := store.lookup(owner, name)
	store.mu.RUnlock()
	if repo == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if isV2(r) {
		gitUploadPackV2(w, r, repo, body)
		return
	}
	// v0: want/have/done/deepen lines.
	lines, _, err := readPktLines(body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var wants, haves []string
	depth := 0
	for _, raw := range lines {
		line := strings.TrimSuffix(string(raw), "\n")
		if i := strings.Index(line, "\x00"); i >= 0 {
			line = line[:i]
		}
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "deepen "):
			fmtSscanfInt(line[7:], &depth)
		}
	}
	store.mu.RLock()
	plan := planFetch(repo, wants, haves, nil, depth)
	var pack []byte
	if len(plan.objects) > 0 || true {
		pack = writePack(plan.objects)
	}
	// Acks: common haves.
	var ack bytes.Buffer
	common := map[string]bool{}
	if len(haves) > 0 {
		reach := reachableCommits(repo, wants)
		for _, h := range haves {
			if reach[h] {
				common[h] = true
			}
		}
	}
	store.mu.RUnlock()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	if len(haves) > 0 {
		if len(common) == 0 {
			buf.Write(pktLine([]byte("NAK\n")))
		} else {
			for h := range common {
				buf.Write(pktLine([]byte("ACK " + h + "\n")))
			}
		}
	} else {
		buf.Write(pktLine([]byte("NAK\n")))
	}
	_ = ack
	for _, s := range plan.shallow {
		buf.Write(pktLine([]byte("shallow " + s + "\n")))
	}
	for _, s := range plan.unshallow {
		buf.Write(pktLine([]byte("unshallow " + s + "\n")))
	}
	buf.Write(sidebandPack(pack))
	w.Write(buf.Bytes())
}

func fmtSscanfInt(s string, out *int) {
	v := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return
		}
		v = v*10 + int(c-'0')
	}
	*out = v
}

func gitUploadPackV2(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	lines, _, err := readPktLines(body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(lines) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cmd := strings.TrimSuffix(string(lines[0]), "\n")
	if cmd == "command=ls-refs" {
		lsRefsV2(w, repo, lines[1:])
		return
	}
	if cmd != "command=fetch" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var wants, haves, oldShallow []string
	depth := 0
	for _, raw := range lines[1:] {
		line := strings.TrimSuffix(string(raw), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			oldShallow = append(oldShallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			fmtSscanfInt(line[7:], &depth)
		}
	}
	store.mu.RLock()
	plan := planFetch(repo, wants, haves, oldShallow, depth)
	pack := writePack(plan.objects)
	// Common haves for the acknowledgments section.
	var acks []string
	if len(haves) > 0 {
		reach := reachableCommits(repo, wants)
		for _, h := range haves {
			if reach[h] {
				acks = append(acks, h)
			}
		}
	}
	store.mu.RUnlock()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	// Acknowledgments section only when the client sent haves.
	if len(haves) > 0 {
		buf.Write(pktLine([]byte("acknowledgments\n")))
		if len(acks) == 0 {
			buf.Write(pktLine([]byte("NAK\n")))
		} else {
			for _, a := range acks {
				buf.Write(pktLine([]byte("ACK " + a + "\n")))
			}
			buf.Write(pktLine([]byte("ready\n")))
		}
		buf.Write(pktDelim)
	}
	if depth > 0 || len(oldShallow) > 0 {
		buf.Write(pktLine([]byte("shallow-info\n")))
		for _, s := range plan.shallow {
			buf.Write(pktLine([]byte("shallow " + s + "\n")))
		}
		for _, s := range plan.unshallow {
			buf.Write(pktLine([]byte("unshallow " + s + "\n")))
		}
		buf.Write(pktDelim)
	}
	buf.Write(pktLine([]byte("packfile\n")))
	buf.Write(sidebandPack(pack))
	w.Write(buf.Bytes())
}

func lsRefsV2(w http.ResponseWriter, repo *Repo, args [][]byte) {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, raw := range args {
		line := strings.TrimSuffix(string(raw), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimSpace(line[11:]))
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
	store.mu.RLock()
	defer store.mu.RUnlock()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	tip := repo.tip(repo.DefaultBranch)
	if len(repo.Refs) == 0 {
		// Empty repository: only HEAD is unborn, and only when asked.
		if unborn {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:refs/heads/" + repo.DefaultBranch
			}
			buf.Write(pktLine([]byte(line + "\n")))
		}
		buf.Write(pktFlush)
		w.Write(buf.Bytes())
		return
	}
	if symrefs && tip != "" && keep("HEAD") {
		buf.Write(pktLine([]byte(tip + " HEAD symref-target:refs/heads/" + repo.DefaultBranch + "\n")))
	}
	for _, name := range sortedRefs(repo) {
		if !keep(name) {
			continue
		}
		oid := repo.Refs[name]
		line := oid + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if o := repo.Objects[oid]; o != nil && o.Typ == "tag" {
				if t := tagTarget(o.Data); t != "" {
					line += " peeled:" + t
				}
			}
		}
		buf.Write(pktLine([]byte(line + "\n")))
	}
	buf.Write(pktFlush)
	w.Write(buf.Bytes())
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

func gitReceivePack(w http.ResponseWriter, r *http.Request, owner, name string, body []byte) {
	store.mu.Lock()
	repo := store.lookup(owner, name)
	if repo == nil {
		store.mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	lines, consumed, err := readPktLines(body)
	if err != nil {
		store.mu.Unlock()
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	type cmd struct{ old, new, ref string }
	var cmds []cmd
	for _, raw := range lines {
		line := strings.TrimSuffix(string(raw), "\n")
		if i := strings.Index(line, "\x00"); i >= 0 {
			line = line[:i]
		}
		parts := strings.Fields(line)
		if len(parts) == 3 {
			cmds = append(cmds, cmd{parts[0], parts[1], parts[2]})
		}
	}
	// The packfile follows the command list, raw.
	var packObjs map[string]*Obj
	packErr := ""
	if len(body) > consumed {
		var err error
		packObjs, err = readPack(body[consumed:], func(oid string) *Obj { return repo.Objects[oid] })
		if err != nil {
			packErr = err.Error()
		} else {
			for oid, o := range packObjs {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = o
				}
			}
		}
	}
	// Apply commands in order, one report line each.
	type report struct{ ref, status, reason string }
	reports := make([]report, 0, len(cmds))
	for _, c := range cmds {
		cur := repo.Refs[c.ref]
		fail := func(reason string) { reports = append(reports, report{c.ref, "ng", reason}) }
		if packErr != "" && c.new != zeroOID {
			fail("unpacker error")
			continue
		}
		if c.new == zeroOID { // delete
			if cur == "" {
				fail("ref does not exist")
			} else if c.old != zeroOID && c.old != cur {
				fail("non-fast-forward")
			} else {
				delete(repo.Refs, c.ref)
				reports = append(reports, report{c.ref, "ok", ""})
			}
			continue
		}
		if cur == "" {
			if c.old != zeroOID {
				fail("non-fast-forward")
				continue
			}
		} else if c.old != cur {
			fail("non-fast-forward")
			continue
		}
		o := repo.Objects[c.new]
		if o == nil {
			fail("object not found")
			continue
		}
		if strings.HasPrefix(c.ref, "refs/heads/") {
			if o.Typ != "commit" {
				fail("not a commit")
				continue
			}
			if cur != "" && !isAncestor(repo, cur, c.new) {
				fail("non-fast-forward")
				continue
			}
		}
		repo.Refs[c.ref] = c.new
		reports = append(reports, report{c.ref, "ok", ""})
	}
	if len(cmds) > 0 {
		store.tick()
	}
	store.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	writeReport := func(text string) {
		payload := append([]byte{1}, []byte(text)...)
		buf.Write(pktLine(payload))
	}
	if packErr != "" {
		writeReport("unpack " + packErr + "\n")
	} else {
		writeReport("unpack ok\n")
	}
	for _, rep := range reports {
		if rep.status == "ok" {
			writeReport("ok " + rep.ref + "\n")
		} else {
			writeReport("ng " + rep.ref + " " + rep.reason + "\n")
		}
	}
	buf.Write(pktFlush)
	w.Write(buf.Bytes())
}
