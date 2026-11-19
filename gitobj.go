package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"

type Obj struct {
	Typ  string // commit | tree | blob | tag
	Data []byte
}

func (o *Obj) oid() string { return gitHash(o.Typ, o.Data) }

func putObj(r *Repo, typ string, data []byte) string {
	o := &Obj{Typ: typ, Data: data}
	oid := o.oid()
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = o
	}
	return oid
}

// ---------------------------------------------------------------------------
// Commits and trees
// ---------------------------------------------------------------------------

type Commit struct {
	Tree    string
	Parents []string
	Author  string
	Message string
}

func parseCommit(data []byte) *Commit {
	c := &Commit{}
	rest := data
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		line := string(rest[:i])
		rest = rest[i+1:]
		if line == "" {
			c.Message = strings.TrimSuffix(string(rest), "\n")
			break
		}
		if strings.HasPrefix(line, "tree ") {
			c.Tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			c.Parents = append(c.Parents, strings.TrimSpace(line[7:]))
		} else if strings.HasPrefix(line, "author ") {
			c.Author = strings.TrimSpace(line[7:])
		}
	}
	return c
}

func buildCommit(tree string, parents []string, msg string) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&sb, "parent %s\n", p)
	}
	fmt.Fprintf(&sb, "author %s <%s@example.invalid> 1735689600 +0000\n", viewerLogin, viewerLogin)
	fmt.Fprintf(&sb, "committer %s <%s@example.invalid> 1735689600 +0000\n\n%s", viewerLogin, viewerLogin, msg)
	return []byte(sb.String())
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			break
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+21 {
			break
		}
		name := string(data[:nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
		data = data[nul+21:]
	}
	return out
}

func buildTree(entries []TreeEntry) []byte {
	sort.Slice(entries, func(i, j int) bool {
		ni, nj := entries[i].Name, entries[j].Name
		if entries[i].Mode == "40000" {
			ni += "/"
		}
		if entries[j].Mode == "40000" {
			nj += "/"
		}
		return ni < nj
	})
	var buf bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// flattenTree returns path -> (oid, mode) for every blob in the tree.
func flattenTree(r *Repo, treeOID string, prefix string, out map[string]TreeEntry) {
	o := r.Objects[treeOID]
	if o == nil || o.Typ != "tree" {
		return
	}
	for _, e := range parseTree(o.Data) {
		path := prefix + e.Name
		if e.Mode == "40000" {
			flattenTree(r, e.OID, path+"/", out)
		} else {
			out[path] = e
		}
	}
}

// treeFromFiles builds nested trees for a flat path->blob map and returns the root oid.
func treeFromFiles(r *Repo, files map[string]string) string {
	type node struct {
		blobs map[string]string
		dirs  map[string]*node
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []TreeEntry
		for name, oid := range n.blobs {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return putObj(r, "tree", buildTree(entries))
	}
	root := &node{blobs: map[string]string{}, dirs: map[string]*node{}}
	for path, oid := range files {
		parts := strings.Split(path, "/")
		cur := root
		for _, p := range parts[:len(parts)-1] {
			if cur.dirs[p] == nil {
				cur.dirs[p] = &node{blobs: map[string]string{}, dirs: map[string]*node{}}
			}
			cur = cur.dirs[p]
		}
		cur.blobs[parts[len(parts)-1]] = oid
	}
	return build(root)
}

// commitFiles creates a commit on top of parent ("" for none) with the given
// file changes applied (nil value deletes the path) and returns its oid.
func commitFiles(r *Repo, parent, msg string, changes map[string]*string) string {
	files := map[string]string{}
	if parent != "" {
		if co := r.Objects[parent]; co != nil {
			flat := map[string]TreeEntry{}
			flattenTree(r, parseCommit(co.Data).Tree, "", flat)
			for p, e := range flat {
				files[p] = e.OID
			}
		}
	}
	for path, content := range changes {
		if content == nil {
			delete(files, path)
			continue
		}
		files[path] = putObj(r, "blob", []byte(*content))
	}
	tree := treeFromFiles(r, files)
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	return putObj(r, "commit", buildCommit(tree, parents, msg))
}

// isAncestor reports whether anc is reachable from oid (or equal).
func isAncestor(r *Repo, anc, oid string) bool {
	if anc == oid {
		return true
	}
	seen := map[string]bool{}
	stack := []string{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o := r.Objects[cur]
		if o == nil || o.Typ != "commit" {
			continue
		}
		stack = append(stack, parseCommit(o.Data).Parents...)
	}
	return false
}

// commitDistance returns the shortest parent-distance from any want, capped.
func commitDistance(r *Repo, wants []string) map[string]int {
	dist := map[string]int{}
	queue := []string{}
	for _, w := range wants {
		if _, ok := dist[w]; !ok {
			dist[w] = 0
			queue = append(queue, w)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		o := r.Objects[cur]
		if o == nil || o.Typ != "commit" {
			continue
		}
		for _, p := range parseCommit(o.Data).Parents {
			if _, ok := dist[p]; !ok {
				dist[p] = dist[cur] + 1
				queue = append(queue, p)
			}
		}
	}
	return dist
}

// reachableCommits returns all commits reachable from roots.
func reachableCommits(r *Repo, roots []string) map[string]bool {
	out := map[string]bool{}
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		out[cur] = true
		o := r.Objects[cur]
		if o == nil || o.Typ != "commit" {
			continue
		}
		stack = append(stack, parseCommit(o.Data).Parents...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Packfiles
// ---------------------------------------------------------------------------

func packHeader(typ int, size int64) []byte {
	var out []byte
	b := byte((typ << 4) | int(size&0x0f))
	size >>= 4
	if size > 0 {
		b |= 0x80
	}
	out = append(out, b)
	for size > 0 {
		b = byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

var packTypeNum = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

func zlibOf(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// writePack serialises objects (no deltas) into a packfile with trailer.
func writePack(objs []*Obj) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	binary.Write(&buf, binary.BigEndian, uint32(2))
	binary.Write(&buf, binary.BigEndian, uint32(len(objs)))
	for _, o := range objs {
		buf.Write(packHeader(packTypeNum[o.Typ], int64(len(o.Data))))
		buf.Write(zlibOf(o.Data))
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

type packObject struct {
	typ  int
	data []byte
	base string // for deltas: base oid (ref) or "" with ofsBase set
	ofs  int64  // negative offset for ofs-delta, -1 when unused
}

// readPack parses a packfile, resolving deltas against objects in the pack
// and, through lookup, objects the server already holds.
func readPack(pack []byte, lookup func(oid string) *Obj) (map[string]*Obj, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack magic")
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	objects := map[string]*Obj{}
	byOffset := map[int64]*Obj{}
	pos := 12
	pending := []struct {
		off  int64
		po   packObject
		raw  []byte
	}{}
	for i := 0; i < count; i++ {
		start := int64(pos)
		// object header varint
		b := pack[pos]
		pos++
		typ := int((b >> 4) & 0x7)
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = pack[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		po := packObject{typ: typ, ofs: -1}
		if typ == 6 { // ofs-delta
			b = pack[pos]
			pos++
			off := int64(b & 0x7f)
			for b&0x80 != 0 {
				b = pack[pos]
				pos++
				off = ((off + 1) << 7) | int64(b&0x7f)
			}
			po.ofs = off
			po.base = ""
		} else if typ == 7 { // ref-delta
			po.base = hex.EncodeToString(pack[pos : pos+20])
			pos += 20
		}
		zr, err := zlib.NewReader(bytes.NewReader(pack[pos:]))
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, err
		}
		// advance pos past the zlib stream: re-encode to find length is
		// unreliable, so locate the stream end by re-compressing is wrong;
		// instead measure with a counting reader over the same slice.
		pos = pos + zlibLen(pack[pos:])
		_ = size
		if typ == 6 || typ == 7 {
			pending = append(pending, struct {
				off int64
				po  packObject
				raw []byte
			}{start, po, data})
			continue
		}
		var typName string
		switch typ {
		case 1:
			typName = "commit"
		case 2:
			typName = "tree"
		case 3:
			typName = "blob"
		case 4:
			typName = "tag"
		default:
			return nil, fmt.Errorf("unknown pack type %d", typ)
		}
		o := &Obj{Typ: typName, Data: data}
		objects[o.oid()] = o
		byOffset[start] = o
	}
	// Resolve deltas iteratively (a delta's base may itself be a delta).
	for len(pending) > 0 {
		progress := false
		rest := pending[:0]
		for _, p := range pending {
			var base *Obj
			if p.po.typ == 7 {
				base = objects[p.po.base]
				if base == nil && lookup != nil {
					base = lookup(p.po.base)
				}
			} else {
				base = byOffset[p.off-p.po.ofs]
			}
			if base == nil {
				rest = append(rest, p)
				continue
			}
			data, err := applyDelta(base.Data, p.raw)
			if err != nil {
				return nil, err
			}
			o := &Obj{Typ: base.Typ, Data: data}
			objects[o.oid()] = o
			byOffset[p.off] = o
			progress = true
		}
		pending = rest
		if !progress {
			return nil, fmt.Errorf("unresolvable delta base")
		}
	}
	return objects, nil
}

// zlibLen returns the byte length of the zlib stream at the head of b.
// Decompression of a prefix succeeds exactly once the prefix holds the whole
// stream (a short prefix ends in unexpected EOF, a longer one is ignored),
// so the length is the smallest successful prefix, found by binary search.
func zlibLen(b []byte) int {
	ok := func(l int) bool {
		zr, err := zlib.NewReader(bytes.NewReader(b[:l]))
		if err != nil {
			return false
		}
		_, err = io.ReadAll(zr)
		zr.Close()
		return err == nil
	}
	if !ok(len(b)) {
		return len(b)
	}
	lo, hi := 0, len(b)
	for lo < hi {
		mid := (lo + hi) / 2
		if ok(mid) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		shift := uint(0)
		for pos < len(delta) {
			b := delta[pos]
			pos++
			v |= int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, nil
			}
		}
		return 0, fmt.Errorf("truncated delta varint")
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			cpOff, cpSize := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					cpOff |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					cpSize |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out.Write(base[cpOff : cpOff+cpSize])
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, fmt.Errorf("delta insert out of range")
			}
			out.Write(delta[pos : pos+int(op)])
			pos += int(op)
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if int64(out.Len()) != dstSize {
		return nil, fmt.Errorf("delta result size mismatch")
	}
	return out.Bytes(), nil
}

// ---------------------------------------------------------------------------
// pkt-lines
// ---------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 4+len(payload))
	copy(out, []byte(fmt.Sprintf("%04x", n)))
	copy(out[4:], payload)
	return out
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type pktReader struct {
	data []byte
	pos  int
}

func (r *pktReader) next() (kind string, payload []byte, err error) {
	if r.pos+4 > len(r.data) {
		return "eof", nil, nil
	}
	var n int
	if _, err := fmt.Sscanf(string(r.data[r.pos:r.pos+4]), "%04x", &n); err != nil {
		return "", nil, fmt.Errorf("bad pkt-len")
	}
	if n == 0 {
		r.pos += 4
		return "flush", nil, nil
	}
	if n == 1 {
		r.pos += 4
		return "delim", nil, nil
	}
	if r.pos+n > len(r.data) {
		return "", nil, fmt.Errorf("truncated pkt-line")
	}
	payload = r.data[r.pos+4 : r.pos+n]
	r.pos += n
	return "data", payload, nil
}

// readPktLines consumes pkt-lines until flush/eof, returning payloads.
// A delim packet (0001) only separates sections in protocol v2 requests
// (command/args come after it), so it is skipped, not treated as the end.
func readPktLines(data []byte) ([][]byte, int, error) {
	r := &pktReader{data: data}
	var out [][]byte
	for {
		kind, payload, err := r.next()
		if err != nil {
			return nil, r.pos, err
		}
		if kind == "data" {
			out = append(out, payload)
			continue
		}
		if kind == "delim" {
			continue
		}
		return out, r.pos, nil
	}
}
