// Git object plumbing: SHA-1 ids, pkt-line framing, packfile reading and
// writing, tree and commit parsing, reachability walks, and the small tree
// surgery the contents API and squash merges need. No git binary is involved;
// every byte here is computed in-process.
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

// ---------------------------------------------------------------- pkt-line

// pktLine frames one payload: 4 hex length bytes then the payload.
func pktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	// Length is written as 4 lowercase hex digits.
	n := len(payload) + 4
	out[0] = "0123456789abcdef"[(n>>12)&0xf]
	out[1] = "0123456789abcdef"[(n>>8)&0xf]
	out[2] = "0123456789abcdef"[(n>>4)&0xf]
	out[3] = "0123456789abcdef"[n&0xf]
	copy(out[4:], payload)
	return out
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

// pktLines joins framed payloads and control markers ("flush"/"delim").
func pktLines(items ...any) []byte {
	var buf bytes.Buffer
	for _, it := range items {
		switch v := it.(type) {
		case string:
			if v == "flush" {
				buf.Write(pktFlush())
			} else if v == "delim" {
				buf.Write(pktDelim())
			} else {
				buf.Write(pktLine([]byte(v)))
			}
		case []byte:
			buf.Write(pktLine(v))
		}
	}
	return buf.Bytes()
}

// pktReader decodes a pkt-line stream from a byte slice.
type pktReader struct {
	buf []byte
	pos int
}

// next returns the next packet: kind "data"/"flush"/"delim"/"end", payload, ok.
func (p *pktReader) next() (string, []byte, bool) {
	if p.pos+4 > len(p.buf) {
		return "end", nil, false
	}
	head := string(p.buf[p.pos : p.pos+4])
	if head == "0000" {
		p.pos += 4
		return "flush", nil, true
	}
	if head == "0001" {
		p.pos += 4
		return "delim", nil, true
	}
	if head == "0002" {
		p.pos += 4
		return "end", nil, true
	}
	var n int
	if _, err := fmt.Sscanf(head, "%04x", &n); err != nil || n < 4 || p.pos+n > len(p.buf) {
		return "end", nil, false
	}
	payload := p.buf[p.pos+4 : p.pos+n]
	p.pos += n
	return "data", payload, true
}

// rest returns the unread remainder.
func (p *pktReader) rest() []byte { return p.buf[p.pos:] }

// ---------------------------------------------------------------- objects

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// parseTree decodes tree bytes into entries, in stored order.
func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	pos := 0
	for pos < len(data) {
		sp := bytes.IndexByte(data[pos:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[pos : pos+sp])
		pos += sp + 1
		nul := bytes.IndexByte(data[pos:], 0)
		if nul < 0 || pos+nul+20 > len(data) {
			break
		}
		name := string(data[pos : pos+nul])
		oid := hex.EncodeToString(data[pos+nul+1 : pos+nul+21])
		pos += nul + 21
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return out
}

// buildTree encodes entries. Git sorts tree entries by name, with
// directories ordered as if their name ended in "/".
func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ai, aj := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			ai += "/"
		}
		if sorted[j].Mode == "40000" {
			aj += "/"
		}
		return ai < aj
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// CommitView is the parsed header of a commit object.
type CommitView struct {
	Tree    string
	Parents []string
	Author  string
	Message string
}

// parseCommit decodes a commit object's header and message.
func parseCommit(data []byte) CommitView {
	var cv CommitView
	blank := bytes.Index(data, []byte("\n\n"))
	head := data
	if blank >= 0 {
		head = data[:blank]
		cv.Message = string(data[blank+2:])
	}
	for _, line := range strings.Split(string(head), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key, val := line[:sp], line[sp+1:]
		switch key {
		case "tree":
			cv.Tree = val
		case "parent":
			cv.Parents = append(cv.Parents, val)
		case "author":
			cv.Author = val
		}
	}
	return cv
}

// parseTagTarget reads an annotated tag's target object and type.
func parseTagTarget(data []byte) (string, string) {
	blank := bytes.Index(data, []byte("\n\n"))
	head := data
	if blank >= 0 {
		head = data[:blank]
	}
	var obj, typ string
	for _, line := range strings.Split(string(head), "\n") {
		if strings.HasPrefix(line, "object ") {
			obj = line[7:]
		} else if strings.HasPrefix(line, "type ") {
			typ = line[5:]
		}
	}
	return obj, typ
}

// buildCommit renders a commit object. Identity and timestamp are fixed
// inputs, so the bytes are deterministic for the same inputs.
func buildCommit(tree string, parents []string, message, who string, ts int64) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + who + " " + fmt.Sprint(ts) + " +0000\n")
	b.WriteString("committer " + who + " " + fmt.Sprint(ts) + " +0000\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// ---------------------------------------------------------------- reachability

// collectClosure walks objects reachable from roots (commit -> tree+parents,
// tree -> entries, tag -> target) and returns the set. Missing objects are
// skipped when allowMissing is set.
func collectClosure(roots []string, lookup func(string) *Obj, allowMissing bool) map[string]bool {
	seen := map[string]bool{}
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if oid == zeroOID || seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			if allowMissing {
				continue
			}
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case "commit":
			cv := parseCommit(obj.Data)
			queue = append(queue, cv.Tree)
			queue = append(queue, cv.Parents...)
		case "tree":
			for _, e := range parseTree(obj.Data) {
				if e.Mode != "160000" { // skip gitlinks
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
	return seen
}

// collectTreeObjects adds a tree and everything under it (subtrees and blobs)
// to out, without following any commit. Used for depth-limited fetches,
// where a commit's parents must stay behind.
func collectTreeObjects(treeOID string, lookup func(string) *Obj, out map[string]bool) {
	queue := []string{treeOID}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if oid == zeroOID || out[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil || obj.Type != "tree" {
			continue
		}
		out[oid] = true
		for _, e := range parseTree(obj.Data) {
			if e.Mode == "40000" {
				queue = append(queue, e.OID)
			} else if e.Mode != "160000" {
				out[e.OID] = true
			}
		}
	}
}

// ancestors returns every commit reachable from tip by following parents.
func ancestors(tip string, lookup func(string) *Obj) map[string]bool {
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if oid == zeroOID || seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		seen[oid] = true
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	return seen
}

// isAncestor reports whether old is an ancestor of (or equal to) newer.
func isAncestor(old, newer string, lookup func(string) *Obj) bool {
	if old == newer {
		return true
	}
	if old == zeroOID {
		return true
	}
	return ancestors(newer, lookup)[old]
}

// mergeBase finds the first common ancestor of a and b, walking b's history
// breadth-first.
func mergeBase(a, b string, lookup func(string) *Obj) string {
	if a == "" || b == "" {
		return ""
	}
	setA := ancestors(a, lookup)
	queue := []string{b}
	seen := map[string]bool{}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] || oid == zeroOID {
			continue
		}
		seen[oid] = true
		if setA[oid] {
			return oid
		}
		obj := lookup(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	return ""
}

// peelToCommit follows annotated tags to the commit they point at.
func peelToCommit(oid string, lookup func(string) *Obj) string {
	for i := 0; i < 16; i++ {
		obj := lookup(oid)
		if obj == nil {
			return ""
		}
		if obj.Type == "commit" {
			return oid
		}
		if obj.Type == "tag" {
			target, _ := parseTagTarget(obj.Data)
			oid = target
			continue
		}
		return ""
	}
	return ""
}

// ---------------------------------------------------------------- packfiles

// applyDelta applies a git delta to a base buffer.
func applyDelta(base, delta []byte) ([]byte, bool) {
	pos := 0
	readVarint := func() (int, bool) {
		v := 0
		shift := 0
		for {
			if pos >= len(delta) {
				return 0, false
			}
			b := delta[pos]
			pos++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, true
			}
		}
	}
	srcSize, ok := readVarint()
	if !ok || srcSize != len(base) {
		return nil, false
	}
	dstSize, ok := readVarint()
	if !ok {
		return nil, false
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			off, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, false
					}
					off |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, false
					}
					size |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, false
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, false
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, false
		}
	}
	if len(out) != dstSize {
		return nil, false
	}
	return out, true
}

// readPack parses a packfile, resolving deltas, and returns its objects by id.
// Thin-pack bases may be supplied through lookup.
func readPack(pack []byte, lookup func(string) *Obj) (map[string]*Obj, bool) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, false
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	pos := 12
	type rawObj struct {
		typ    string
		data   []byte
		ofs    int
		delta  int // 0 none, 6 ofs, 7 ref
		baseOf int
		baseID string
	}
	raws := make([]rawObj, 0, count)
	byOfs := map[int]int{}
	for i := 0; i < count; i++ {
		if pos >= len(pack)-20 {
			return nil, false
		}
		start := pos
		b := pack[pos]
		pos++
		typCode := (b >> 4) & 0x7
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= len(pack) {
				return nil, false
			}
			b = pack[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		var typ string
		var deltaKind int
		var baseOf int
		var baseID string
		switch typCode {
		case 1:
			typ = "commit"
		case 2:
			typ = "tree"
		case 3:
			typ = "blob"
		case 4:
			typ = "tag"
		case 6:
			deltaKind = 6
			// OFS_DELTA base offset: negative offset varint.
			if pos >= len(pack) {
				return nil, false
			}
			b = pack[pos]
			pos++
			off := int(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= len(pack) {
					return nil, false
				}
				b = pack[pos]
				pos++
				off = ((off + 1) << 7) | int(b&0x7f)
			}
			baseOf = start - off
		case 7:
			deltaKind = 7
			if pos+20 > len(pack) {
				return nil, false
			}
			baseID = hex.EncodeToString(pack[pos : pos+20])
			pos += 20
		default:
			return nil, false
		}
		zr, err := zlib.NewReader(bytes.NewReader(pack[pos:]))
		if err != nil {
			return nil, false
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil || int64(len(data)) != size {
			return nil, false
		}
		// Advance pos past the zlib stream: re-inflate with a counting reader.
		consumed := zlibLen(pack[pos:])
		if consumed <= 0 {
			return nil, false
		}
		pos += consumed
		raws = append(raws, rawObj{typ: typ, data: data, ofs: start, delta: deltaKind, baseOf: baseOf, baseID: baseID})
		byOfs[start] = len(raws) - 1
	}
	// Verify the trailer checksum.
	if pos+20 > len(pack) {
		return nil, false
	}
	sum := sha1.Sum(pack[:pos])
	if !bytes.Equal(sum[:], pack[pos:pos+20]) {
		return nil, false
	}
	out := map[string]*Obj{}
	var resolve func(idx int) (*Obj, bool)
	resolve = func(idx int) (*Obj, bool) {
		raw := raws[idx]
		if raw.delta == 0 {
			oid := hashObj(raw.typ, raw.data)
			obj := &Obj{Type: raw.typ, Data: raw.data}
			out[oid] = obj
			return obj, true
		}
		var base *Obj
		var ok bool
		if raw.delta == 6 {
			bi, found := byOfs[raw.baseOf]
			if !found {
				return nil, false
			}
			base, ok = resolve(bi)
		} else {
			if lookup != nil {
				base = lookup(raw.baseID)
			}
			if base == nil {
				// The base may be an earlier plain object in this pack.
				for _, o := range out {
					_ = o
				}
				// Search raw objects by id.
				for j, r2 := range raws {
					if r2.delta == 0 && hashObj(r2.typ, r2.data) == raw.baseID {
						base, ok = resolve(j)
						break
					}
				}
			}
			ok = base != nil
		}
		if !ok || base == nil {
			return nil, false
		}
		data, good := applyDelta(base.Data, raw.data)
		if !good {
			return nil, false
		}
		obj := &Obj{Type: base.Type, Data: data}
		out[hashObj(base.Type, data)] = obj
		return obj, true
	}
	for i := range raws {
		if _, ok := resolve(i); !ok {
			return nil, false
		}
	}
	return out, true
}

// zlibLen finds the length of the zlib stream at the start of buf by
// inflating it through a counting reader. The counter implements ReadByte,
// so the flate reader consumes bytes one at a time and never reads past the
// end of the stream (a plain wrapped reader would be buffered ahead).
func zlibLen(buf []byte) int {
	r := &countReader{r: bytes.NewReader(buf)}
	zr, err := zlib.NewReader(r)
	if err != nil {
		return -1
	}
	tmp := make([]byte, 32768)
	for {
		_, err := zr.Read(tmp)
		if err != nil {
			break
		}
	}
	zr.Close()
	return int(r.n)
}

type countReader struct {
	r *bytes.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// packObjectHeader encodes one pack object header (type + size varint).
func packObjectHeader(typ string, size int) []byte {
	var code byte
	switch typ {
	case "commit":
		code = 1
	case "tree":
		code = 2
	case "blob":
		code = 3
	case "tag":
		code = 4
	}
	out := []byte{byte((code << 4) | (byte(size) & 0x0f))}
	size >>= 4
	for size > 0 {
		out = append(out, byte(size&0x7f))
		size >>= 7
	}
	// Set continuation bits on all but the last byte.
	for i := 0; i < len(out)-1; i++ {
		out[i] |= 0x80
	}
	return out
}

// writePack serialises objects (plain, no deltas) into a packfile.
func writePack(objs []*Obj) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objs)))
	buf.Write(hdr[:])
	for _, o := range objs {
		buf.Write(packObjectHeader(o.Type, len(o.Data)))
		var zb bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&zb, zlib.BestSpeed)
		_, _ = zw.Write(o.Data)
		_ = zw.Close()
		buf.Write(zb.Bytes())
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// ---------------------------------------------------------------- tree surgery

// flattenTree walks a tree into path -> entry (files and subtrees as blobs
// only: directories are expanded, gitlinks skipped).
func flattenTree(treeOID string, lookup func(string) *Obj, out map[string]TreeEntry, prefix string) {
	obj := lookup(treeOID)
	if obj == nil || obj.Type != "tree" {
		return
	}
	for _, e := range parseTree(obj.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			flattenTree(e.OID, lookup, out, path)
		} else if e.Mode != "160000" {
			out[path] = e
		}
	}
}

// buildTreeFromFlat rebuilds nested trees from a flat path map, storing every
// new tree object through put and returning the root tree oid.
func buildTreeFromFlat(flat map[string]TreeEntry, put func(string, []byte) string) string {
	// Group paths by first segment, recursively.
	var build func(paths map[string]TreeEntry) string
	build = func(paths map[string]TreeEntry) string {
		dirs := map[string]map[string]TreeEntry{}
		var entries []TreeEntry
		for path, e := range paths {
			if idx := strings.IndexByte(path, '/'); idx >= 0 {
				head, tail := path[:idx], path[idx+1:]
				if dirs[head] == nil {
					dirs[head] = map[string]TreeEntry{}
				}
				dirs[head][tail] = e
			} else {
				entries = append(entries, e)
			}
		}
		names := make([]string, 0, len(dirs))
		for n := range dirs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			entries = append(entries, TreeEntry{Mode: "40000", Name: n, OID: build(dirs[n])})
		}
		return put("tree", buildTree(entries))
	}
	return build(flat)
}

// diffFlat compares two flat file maps, returning changed paths grouped by
// status: added, modified, removed.
func diffFlat(base, head map[string]TreeEntry) (added, modified, removed []string) {
	for p, he := range head {
		if be, ok := base[p]; !ok {
			added = append(added, p)
		} else if be.OID != he.OID {
			modified = append(modified, p)
		}
	}
	for p := range base {
		if _, ok := head[p]; !ok {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(removed)
	return added, modified, removed
}

// countLines counts lines in blob data (a trailing partial line counts).
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}
