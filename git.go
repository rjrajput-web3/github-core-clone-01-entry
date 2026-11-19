// GitForge git wire layer: pkt-lines, packfiles (read/write, deltas) and the
// smart-HTTP handlers for info/refs, upload-pack (v2 + v0) and receive-pack.
// Faithful port of the Node git.ts, except readPack uses a counting
// io.ByteReader + compress/zlib to find each entry's deflated length exactly
// (no re-deflate, no binary search).
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"sort"
	"strings"
)

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	head := []byte{hexDigit(n >> 12), hexDigit((n >> 8) & 0xf), hexDigit((n >> 4) & 0xf), hexDigit(n & 0xf)}
	out := make([]byte, 0, n)
	out = append(out, head...)
	out = append(out, payload...)
	return out
}

func hexDigit(v int) byte {
	if v < 10 {
		return byte('0' + v)
	}
	return byte('a' + v - 10)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type decodedPacket struct {
	kind    string // data | flush | delim | response-end
	payload []byte
}

func decodePktLines(buf []byte) ([]decodedPacket, int, bool) {
	var packets []decodedPacket
	off := 0
	for off+4 <= len(buf) {
		head := string(buf[off : off+4])
		ln := 0
		valid := true
		for _, c := range head {
			ln *= 16
			switch {
			case c >= '0' && c <= '9':
				ln += int(c - '0')
			case c >= 'a' && c <= 'f':
				ln += int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				ln += int(c-'A') + 10
			default:
				valid = false
			}
		}
		if !valid {
			return nil, 0, false
		}
		if ln == 0 {
			packets = append(packets, decodedPacket{kind: "flush"})
			off += 4
			break
		}
		if ln == 1 {
			packets = append(packets, decodedPacket{kind: "delim"})
			off += 4
			continue
		}
		if ln == 2 {
			packets = append(packets, decodedPacket{kind: "response-end"})
			off += 4
			continue
		}
		if ln < 4 || off+ln > len(buf) {
			return nil, 0, false
		}
		packets = append(packets, decodedPacket{kind: "data", payload: buf[off+4 : off+ln]})
		off += ln
	}
	return packets, off, true
}

var typeCode = map[GitObjectType]byte{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}
var codeType = map[byte]GitObjectType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}

func packObjectHeader(typ GitObjectType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := byte((int(code) << 4) | (remaining & 0x0f))
	remaining >>= 4
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining >>= 7
	}
	out = append(out, first)
	return out
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.DefaultCompression)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func writePack(objects []PackObj) []byte {
	// Sort by oid for byte-deterministic packs.
	sorted := make([]PackObj, len(objects))
	copy(sorted, objects)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].OID < sorted[j].OID })
	var body bytes.Buffer
	body.WriteString("PACK")
	body.Write([]byte{0, 0, 0, 2})
	body.Write([]byte{byte(len(sorted) >> 24), byte(len(sorted) >> 16), byte(len(sorted) >> 8), byte(len(sorted))})
	for _, o := range sorted {
		body.Write(packObjectHeader(o.Type, len(o.Data)))
		body.Write(deflate(o.Data))
	}
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

type PackObj struct {
	Type GitObjectType
	Data []byte
	OID  string
}

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readVarint := func() int {
		value := 0
		shift := 0
		for {
			b := delta[off]
			off++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	srcSize := readVarint()
	tgtSize := readVarint()
	if srcSize != len(base) {
		return nil, errDelta("delta source size mismatch")
	}
	var out bytes.Buffer
	total := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			cpOff, cpSize := 0, 0
			if op&0x01 != 0 {
				cpOff = int(delta[off])
				off++
			}
			if op&0x02 != 0 {
				cpOff |= int(delta[off]) << 8
				off++
			}
			if op&0x04 != 0 {
				cpOff |= int(delta[off]) << 16
				off++
			}
			if op&0x08 != 0 {
				cpOff |= int(delta[off]) << 24
				off++
			}
			if op&0x10 != 0 {
				cpSize = int(delta[off])
				off++
			}
			if op&0x20 != 0 {
				cpSize |= int(delta[off]) << 8
				off++
			}
			if op&0x40 != 0 {
				cpSize |= int(delta[off]) << 16
				off++
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			out.Write(base[cpOff : cpOff+cpSize])
			total += cpSize
		} else if op != 0 {
			out.Write(delta[off : off+int(op)])
			total += int(op)
			off += int(op)
		} else {
			return nil, errDelta("delta opcode 0")
		}
	}
	result := out.Bytes()
	if len(result) != tgtSize {
		return nil, errDelta("delta target size mismatch")
	}
	return result, nil
}

type deltaError struct{ msg string }

func (e *deltaError) Error() string { return e.msg }
func errDelta(m string) error       { return &deltaError{m} }

// countingReader counts bytes consumed by the zlib reader. It implements
// io.ByteReader so compress/flate reads byte-by-byte and the count is exact
// (without ReadByte, flate would wrap it in a bufio.Reader and over-read).
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) ReadByte() (byte, error) {
	var b [1]byte
	n, err := c.r.Read(b[:])
	c.n += int64(n)
	if n == 0 {
		return 0, err
	}
	return b[0], nil
}

// inflateOne inflates exactly one zlib stream starting at buf, returning the
// inflated bytes and the number of deflated bytes consumed.
func inflateOne(buf []byte) ([]byte, int64, error) {
	cr := &countingReader{r: bytes.NewReader(buf)}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	return data, cr.n, nil
}

type PackRead struct {
	Objects []PackObj
	OK      bool
	Error   string
}

func readPack(buf []byte, lookup func(oid string) *StoredObject) PackRead {
	fail := func(msg string) PackRead { return PackRead{OK: false, Error: msg} }
	if len(buf) < 32 {
		return fail("pack too short")
	}
	if string(buf[0:4]) != "PACK" {
		return fail("bad pack magic")
	}
	version := int(buf[4])<<24 | int(buf[5])<<16 | int(buf[6])<<8 | int(buf[7])
	if version != 2 && version != 3 {
		return fail("pack version " + itoa(version))
	}
	count := int(buf[8])<<24 | int(buf[9])<<16 | int(buf[10])<<8 | int(buf[11])
	trailer := hex.EncodeToString(buf[len(buf)-20:])
	sum := sha1.Sum(buf[:len(buf)-20])
	if hex.EncodeToString(sum[:]) != trailer {
		return fail("pack checksum mismatch")
	}
	end := len(buf) - 20

	type rawEntry struct {
		typ      string // commit|tree|blob|tag|ofs-delta|ref-delta
		data     []byte
		baseOID  string
		baseOff  int
		startOff int
	}
	var raw []rawEntry
	off := 12
	for i := 0; i < count; i++ {
		if off >= end {
			return fail("pack truncated")
		}
		startOff := off
		b := buf[off]
		off++
		typeCode := (b >> 4) & 0x07
		size := int(b & 0x0f)
		shift := 4
		for b&0x80 != 0 {
			b = buf[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		if typeCode == 6 {
			ob := buf[off]
			off++
			offset := int(ob & 0x7f)
			for ob&0x80 != 0 {
				ob = buf[off]
				off++
				offset = ((offset + 1) << 7) | int(ob&0x7f)
			}
			data, n, err := inflateOne(buf[off:end])
			if err != nil || len(data) != size {
				return fail("bad ofs-delta inflate")
			}
			off += int(n)
			raw = append(raw, rawEntry{typ: "ofs-delta", data: data, baseOff: startOff - offset, startOff: startOff})
		} else if typeCode == 7 {
			baseOID := hex.EncodeToString(buf[off : off+20])
			off += 20
			data, n, err := inflateOne(buf[off:end])
			if err != nil || len(data) != size {
				return fail("bad ref-delta inflate")
			}
			off += int(n)
			raw = append(raw, rawEntry{typ: "ref-delta", data: data, baseOID: baseOID, startOff: startOff})
		} else {
			typ, ok := codeType[typeCode]
			if !ok {
				return fail("unknown pack type " + itoa(int(typeCode)))
			}
			data, n, err := inflateOne(buf[off:end])
			if err != nil || len(data) != size {
				return fail("bad object inflate")
			}
			off += int(n)
			raw = append(raw, rawEntry{typ: string(typ), data: data, startOff: startOff})
		}
	}

	resolved := make(map[int]PackObj)
	byStart := make(map[int]int)
	for i, e := range raw {
		byStart[e.startOff] = i
	}
	var objects []PackObj
	var resolve func(index, depth int) *PackObj
	resolve = func(index, depth int) *PackObj {
		if depth > 64 {
			return nil
		}
		if hit, ok := resolved[index]; ok {
			h := hit
			return &h
		}
		entry := raw[index]
		if entry.typ == "ofs-delta" || entry.typ == "ref-delta" {
			var base *PackObj
			if entry.typ == "ofs-delta" {
				if baseIdx, ok := byStart[entry.baseOff]; ok {
					if r := resolve(baseIdx, depth+1); r != nil {
						base = r
					}
				}
			} else {
				inPack := -1
				for i, e := range raw {
					if e.typ != "ofs-delta" && e.typ != "ref-delta" && hashObject(GitObjectType(e.typ), e.data) == entry.baseOID {
						inPack = i
						break
					}
				}
				if inPack >= 0 {
					if r := resolve(inPack, depth+1); r != nil {
						base = r
					}
				} else if lookup != nil {
					if stored := lookup(entry.baseOID); stored != nil {
						base = &PackObj{Type: stored.Type, Data: stored.Data, OID: entry.baseOID}
					}
				}
			}
			if base == nil {
				return nil
			}
			data, err := applyDelta(base.Data, entry.data)
			if err != nil {
				return nil
			}
			obj := PackObj{Type: base.Type, Data: data, OID: hashObject(base.Type, data)}
			resolved[index] = obj
			objects = append(objects, obj)
			return &obj
		}
		obj := PackObj{Type: GitObjectType(entry.typ), Data: entry.data, OID: hashObject(GitObjectType(entry.typ), entry.data)}
		resolved[index] = obj
		objects = append(objects, obj)
		return &obj
	}
	for i := range raw {
		if resolve(i, 0) == nil {
			return fail("unresolvable delta")
		}
	}
	return PackRead{Objects: objects, OK: true}
}

type GitResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

var notFoundJSON = []byte("{\"message\":\"Not Found\"}\n")
var badRequestJSON = []byte("{\"message\":\"Bad Request\"}\n")
const agentStr = "GitForge/1.0"

func v2Advertisement() []byte {
	return bytes.Join([][]byte{
		pktLine([]byte("version 2\n")),
		pktLine([]byte("ls-refs=unborn\n")),
		pktLine([]byte("fetch=shallow wait-for-done\n")),
		pktLine([]byte("object-format=sha1\n")),
		pktLine([]byte("agent=" + agentStr + "\n")),
		pktFlush,
	}, nil)
}

var v2Adv = v2Advertisement()

type refLine struct {
	name string
	oid  string
}

func sortedRefs(repo *Repo) []refLine {
	out := make([]refLine, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		out = append(out, refLine{name, oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func v0Advertisement(s *Store, repo *Repo, service string) []byte {
	var caps string
	if service == "git-upload-pack" {
		caps = "side-band-64k multi_ack_detailed thin-pack ofs-delta shallow symref=HEAD:refs/heads/" + repo.DefaultBranch + " agent=" + agentStr
	} else {
		caps = "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=" + agentStr
	}
	var parts [][]byte
	parts = append(parts, pktLine([]byte("# service="+service+"\n")), pktFlush)
	refs := sortedRefs(repo)
	tip, hasTip := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if len(refs) == 0 {
		parts = append(parts, pktLine([]byte(ZeroOID+" capabilities^{}\x00"+caps+"\n")))
	} else {
		first := true
		emit := func(line string) {
			if first {
				parts = append(parts, pktLine([]byte(line+"\x00"+caps+"\n")))
				first = false
			} else {
				parts = append(parts, pktLine([]byte(line+"\n")))
			}
		}
		if hasTip {
			emit(tip + " HEAD")
		}
		for _, ref := range refs {
			emit(ref.oid + " " + ref.name)
			if obj := s.getObject(ref.oid); obj != nil && obj.Type == ObjTag && obj.Tag != nil {
				peeled := s.peelToCommit(ref.oid)
				parts = append(parts, pktLine([]byte(peeled+" "+ref.name+"^{}\n")))
			}
		}
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

func handleInfoRefs(s *Store, owner, name, service string, wantV2 bool) GitResponse {
	repo := s.getRepo(owner, name)
	if repo == nil {
		return GitResponse{404, "application/json", notFoundJSON}
	}
	if service == "git-upload-pack" {
		if wantV2 {
			if repo.AdvV2 == nil {
				repo.AdvV2 = v2Adv
			}
			return GitResponse{200, "application/x-git-upload-pack-advertisement", repo.AdvV2}
		}
		if repo.AdvV0Up == nil {
			repo.AdvV0Up = v0Advertisement(s, repo, "git-upload-pack")
		}
		return GitResponse{200, "application/x-git-upload-pack-advertisement", repo.AdvV0Up}
	}
	if service == "git-receive-pack" {
		if repo.AdvV0Recv == nil {
			repo.AdvV0Recv = v0Advertisement(s, repo, "git-receive-pack")
		}
		return GitResponse{200, "application/x-git-receive-pack-advertisement", repo.AdvV0Recv}
	}
	return GitResponse{404, "application/json", notFoundJSON}
}

func sidebandPack(pack []byte) [][]byte {
	var out [][]byte
	for off := 0; off < len(pack); off += 32768 {
		end := off + 32768
		if end > len(pack) {
			end = len(pack)
		}
		chunk := make([]byte, 0, end-off+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, pack[off:end]...)
		out = append(out, pktLine(chunk))
	}
	if len(pack) == 0 {
		out = append(out, pktLine([]byte{1}))
	}
	return out
}

type fetchArgs struct {
	wants   []string
	haves   []string
	shallow []string
	depth   int
	hasDepth bool
	filter  string
}

func handleUploadPack(s *Store, owner, name string, body []byte, isV2 bool) GitResponse {
	repo := s.getRepo(owner, name)
	if repo == nil {
		return GitResponse{404, "application/json", notFoundJSON}
	}
	packets, _, ok := decodePktLines(body)
	if !ok {
		return GitResponse{400, "application/json", badRequestJSON}
	}
	var lines []string
	for _, p := range packets {
		if p.kind == "data" {
			lines = append(lines, string(p.payload))
		}
	}
	resultType := "application/x-git-upload-pack-result"

	if !isV2 {
		args := fetchArgs{}
		for _, line := range lines {
			text := strings.TrimSuffix(line, "\n")
			switch {
			case strings.HasPrefix(text, "want "):
				args.wants = append(args.wants, strings.SplitN(text[5:], " ", 2)[0])
			case strings.HasPrefix(text, "have "):
				args.haves = append(args.haves, strings.TrimSpace(text[5:]))
			case strings.HasPrefix(text, "shallow "):
				args.shallow = append(args.shallow, strings.TrimSpace(text[8:]))
			case strings.HasPrefix(text, "deepen "):
				args.depth, args.hasDepth = atoi(text[7:]), true
			}
		}
		res := s.collectForFetch(args.wants, args.haves, args.shallow, args.depth, args.hasDepth)
		var objs []PackObj
		for _, o := range res.Objects {
			objs = append(objs, PackObj{Type: o.Type, Data: o.Data, OID: hashObject(o.Type, o.Data)})
		}
		pack := writePack(objs)
		var parts [][]byte
		ack := ""
		for _, h := range args.haves {
			if s.getObject(h) != nil {
				ack = h
				break
			}
		}
		if ack != "" {
			parts = append(parts, pktLine([]byte("ACK "+ack+"\n")))
		} else {
			parts = append(parts, pktLine([]byte("NAK\n")))
		}
		for _, sh := range res.Shallow {
			parts = append(parts, pktLine([]byte("shallow "+sh+"\n")))
		}
		for _, u := range res.Unshallow {
			parts = append(parts, pktLine([]byte("unshallow "+u+"\n")))
		}
		parts = append(parts, sidebandPack(pack)...)
		parts = append(parts, pktFlush)
		return GitResponse{200, resultType, bytes.Join(parts, nil)}
	}

	command := ""
	if len(lines) > 0 {
		command = strings.TrimSuffix(lines[0], "\n")
	}
	if command == "command=ls-refs" {
		peel, symrefs, unborn := false, false, false
		var prefixes []string
		for _, line := range lines[1:] {
			text := strings.TrimSuffix(line, "\n")
			switch {
			case text == "peel":
				peel = true
			case text == "symrefs":
				symrefs = true
			case text == "unborn":
				unborn = true
			case strings.HasPrefix(text, "ref-prefix "):
				prefixes = append(prefixes, text[11:])
			}
		}
		cacheKey := "ls:" + strings.Join(prefixes, ",") + "|" + b2i(peel) + b2i(symrefs) + b2i(unborn)
		if cached, ok := repo.FetchCache[cacheKey]; ok {
			return GitResponse{200, resultType, cached}
		}
		var parts [][]byte
		refs := sortedRefs(repo)
		tip, hasTip := repo.Refs["refs/heads/"+repo.DefaultBranch]
		keep := func(refName string) bool {
			if len(prefixes) == 0 {
				return true
			}
			for _, p := range prefixes {
				if strings.HasPrefix(refName, p) {
					return true
				}
			}
			return false
		}
		if len(refs) == 0 {
			if unborn {
				parts = append(parts, pktLine([]byte("unborn HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n")))
			}
		} else {
			if symrefs && hasTip {
				showHead := len(prefixes) == 0
				if !showHead {
					for _, p := range prefixes {
						if strings.HasPrefix("HEAD", p) {
							showHead = true
							break
						}
					}
				}
				if showHead {
					parts = append(parts, pktLine([]byte(tip+" HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n")))
				}
			}
			for _, ref := range refs {
				if !keep(ref.name) {
					continue
				}
				line := ref.oid + " " + ref.name
				if peel {
					if obj := s.getObject(ref.oid); obj != nil && obj.Type == ObjTag && obj.Tag != nil {
						line += " peeled:" + s.peelToCommit(ref.oid)
					}
				}
				parts = append(parts, pktLine([]byte(line+"\n")))
			}
		}
		parts = append(parts, pktFlush)
		out := bytes.Join(parts, nil)
		repo.FetchCache[cacheKey] = out
		return GitResponse{200, resultType, out}
	}

	if command == "command=fetch" {
		args := fetchArgs{}
		for _, line := range lines[1:] {
			text := strings.TrimSuffix(line, "\n")
			switch {
			case strings.HasPrefix(text, "want "):
				args.wants = append(args.wants, strings.TrimSpace(text[5:]))
			case strings.HasPrefix(text, "have "):
				args.haves = append(args.haves, strings.TrimSpace(text[5:]))
			case strings.HasPrefix(text, "shallow "):
				args.shallow = append(args.shallow, strings.TrimSpace(text[8:]))
			case strings.HasPrefix(text, "deepen "):
				args.depth, args.hasDepth = atoi(text[7:]), true
			case strings.HasPrefix(text, "filter "):
				args.filter = strings.TrimSpace(text[7:])
			}
		}
		if len(args.wants) == 0 {
			parts := [][]byte{
				pktLine([]byte("acknowledgments\n")),
				pktLine([]byte("NAK\n")),
				pktDelim,
				pktLine([]byte("packfile\n")),
			}
			parts = append(parts, sidebandPack(writePack(nil))...)
			parts = append(parts, pktFlush)
			return GitResponse{200, resultType, bytes.Join(parts, nil)}
		}
		cacheKey := "fetch:" + strings.Join(args.wants, ",") + "|" + strings.Join(args.haves, ",") + "|" + itoaDepth(args.depth, args.hasDepth) + "|" + strings.Join(args.shallow, ",")
		if cached, ok := repo.FetchCache[cacheKey]; ok {
			return GitResponse{200, resultType, cached}
		}
		res := s.collectForFetch(args.wants, args.haves, args.shallow, args.depth, args.hasDepth)
		var objs []PackObj
		for _, o := range res.Objects {
			objs = append(objs, PackObj{Type: o.Type, Data: o.Data, OID: hashObject(o.Type, o.Data)})
		}
		pack := writePack(objs)
		parts := [][]byte{pktLine([]byte("acknowledgments\n"))}
		ack := ""
		for _, h := range args.haves {
			if s.getObject(h) != nil {
				ack = h
				break
			}
		}
		if ack != "" {
			parts = append(parts, pktLine([]byte("ACK "+ack+"\n")), pktLine([]byte("ready\n")))
		} else {
			parts = append(parts, pktLine([]byte("NAK\n")))
		}
		parts = append(parts, pktDelim)
		if len(res.Shallow) > 0 || len(res.Unshallow) > 0 {
			parts = append(parts, pktLine([]byte("shallow-info\n")))
			for _, sh := range res.Shallow {
				parts = append(parts, pktLine([]byte("shallow "+sh+"\n")))
			}
			for _, u := range res.Unshallow {
				parts = append(parts, pktLine([]byte("unshallow "+u+"\n")))
			}
			parts = append(parts, pktDelim)
		}
		parts = append(parts, pktLine([]byte("packfile\n")))
		parts = append(parts, sidebandPack(pack)...)
		parts = append(parts, pktFlush)
		out := bytes.Join(parts, nil)
		repo.FetchCache[cacheKey] = out
		return GitResponse{200, resultType, out}
	}

	return GitResponse{400, "application/json", badRequestJSON}
}

func itoaDepth(d int, has bool) string {
	if !has {
		return ""
	}
	return itoa(d)
}

func b2i(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func atoi(s string) int {
	n := 0
	s = strings.TrimSpace(s)
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func handleReceivePack(s *Store, owner, name string, body []byte) GitResponse {
	repo := s.getRepo(owner, name)
	if repo == nil {
		return GitResponse{404, "application/json", notFoundJSON}
	}
	resultType := "application/x-git-receive-pack-result"
	packets, rest, ok := decodePktLines(body)
	if !ok {
		return GitResponse{400, "application/json", badRequestJSON}
	}
	var dataLines []string
	for _, p := range packets {
		if p.kind == "data" {
			dataLines = append(dataLines, string(p.payload))
		}
	}
	var commands []pushCommand
	sideband := false
	for i, line := range dataLines {
		text := strings.TrimSuffix(line, "\n")
		if nul := strings.Index(text, "\x00"); nul >= 0 {
			if i == 0 {
				for _, cap := range strings.Split(text[nul+1:], " ") {
					if cap == "side-band-64k" {
						sideband = true
					}
				}
			}
			text = text[:nul]
		}
		parts := strings.Split(text, " ")
		if len(parts) < 3 {
			continue
		}
		commands = append(commands, pushCommand{parts[0], parts[1], strings.Join(parts[2:], " ")})
	}
	if len(commands) == 0 {
		return GitResponse{400, "application/json", badRequestJSON}
	}

	var reportLines []string
	packBytes := body[rest:]
	needsPack := false
	for _, c := range commands {
		if c.newOID != ZeroOID {
			needsPack = true
		}
	}
	unpackOK := true
	unpackDetail := "ok"
	if needsPack {
		if len(packBytes) == 0 {
			unpackOK = false
			unpackDetail = "missing pack"
		} else {
			read := readPack(packBytes, s.getObject)
			if !read.OK {
				unpackOK = false
				unpackDetail = read.Error
			} else {
				for _, obj := range read.Objects {
					s.putObject(obj.Type, obj.Data)
				}
			}
		}
	}
	if unpackOK {
		for _, cmd := range commands {
			current, hasCurrent := repo.Refs[cmd.ref]
			if cmd.newOID == ZeroOID {
				if !hasCurrent {
					reportLines = append(reportLines, "ng "+cmd.ref+" remote ref does not exist")
				} else if cmd.oldOID != ZeroOID && cmd.oldOID != current {
					reportLines = append(reportLines, "ng "+cmd.ref+" non-fast-forward")
				} else {
					s.setRef(repo, cmd.ref, "", true)
					reportLines = append(reportLines, "ok "+cmd.ref)
				}
				continue
			}
			if s.getObject(cmd.newOID) == nil {
				reportLines = append(reportLines, "ng "+cmd.ref+" object not found")
				continue
			}
			if !hasCurrent {
				if cmd.oldOID != ZeroOID {
					reportLines = append(reportLines, "ng "+cmd.ref+" non-fast-forward")
					continue
				}
				s.setRef(repo, cmd.ref, cmd.newOID, false)
				reportLines = append(reportLines, "ok "+cmd.ref)
				continue
			}
			if cmd.oldOID == ZeroOID {
				reportLines = append(reportLines, "ng "+cmd.ref+" already exists")
				continue
			}
			if cmd.oldOID != current {
				reportLines = append(reportLines, "ng "+cmd.ref+" non-fast-forward")
				continue
			}
			if !s.isAncestor(current, cmd.newOID) {
				reportLines = append(reportLines, "ng "+cmd.ref+" non-fast-forward")
				continue
			}
			s.setRef(repo, cmd.ref, cmd.newOID, false)
			reportLines = append(reportLines, "ok "+cmd.ref)
		}
	}
	textLines := []string{"unpack " + map[bool]string{true: "ok", false: unpackDetail}[unpackOK]}
	textLines = append(textLines, reportLines...)
	var framed []byte
	for _, l := range textLines {
		framed = append(framed, pktLine([]byte(l+"\n"))...)
	}
	var out []byte
	if sideband {
		var chunks [][]byte
		for off := 0; off < len(framed); off += 32768 {
			end := off + 32768
			if end > len(framed) {
				end = len(framed)
			}
			chunk := make([]byte, 0, end-off+1)
			chunk = append(chunk, 1)
			chunk = append(chunk, framed[off:end]...)
			chunks = append(chunks, pktLine(chunk))
		}
		chunks = append(chunks, pktFlush)
		out = bytes.Join(chunks, nil)
	} else {
		out = append(append([]byte{}, framed...), pktFlush...)
	}
	return GitResponse{200, resultType, out}
}
