package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// git object model: objects are stored per-repository, in memory, as raw
// (type, data) pairs keyed by their SHA-1 id.
// ---------------------------------------------------------------------------

type Object struct {
	Type string // commit | tree | blob | tag
	Data []byte
}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	var hdr [24]byte
	n := copy(hdr[:], typ)
	hdr[n] = ' '
	head := strconv.AppendInt(hdr[:n+1], int64(len(data)), 10)
	head = append(head, 0)
	h.Write(head)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

var typeCode = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[byte]string{1: "commit", 2: "tree", 3: "blob", 4: "tag", 6: "ofs-delta", 7: "ref-delta"}

// ---------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------

type TreeEntry struct {
	Mode string // "100644", "100755", "120000", "40000", "160000"
	Name string
	OID  string
}

// parseTree decodes a raw tree object.
func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			break
		}
		mode := string(data[:sp])
		rest := data[sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || nul+21 > len(rest) {
			break
		}
		name := string(rest[:nul])
		oid := hex.EncodeToString(rest[nul+1 : nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
		data = rest[nul+21:]
	}
	return out
}

// buildTree encodes entries in git's canonical order: by name, with
// directories sorting as if the name ended in '/'.
func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		ka, kb := a.Name, b.Name
		if a.Mode == "40000" {
			ka += "/"
		}
		if b.Mode == "40000" {
			kb += "/"
		}
		return ka < kb
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

// flattenTree walks a tree recursively into path -> blob oid.
func flattenTree(store map[string]*Object, treeOID string, prefix string, out map[string]string) {
	obj := store[treeOID]
	if obj == nil || obj.Type != "tree" {
		return
	}
	for _, e := range parseTree(obj.Data) {
		if e.Mode == "40000" {
			flattenTree(store, e.OID, prefix+e.Name+"/", out)
		} else {
			out[prefix+e.Name] = e.OID
		}
	}
}

// buildTreeFromFiles turns a flat path -> blob oid map into nested tree
// objects, storing them, and returns the root tree oid.
func buildTreeFromFiles(store map[string]*Object, files map[string]string) string {
	type node struct {
		files map[string]string
		dirs  map[string]*node
	}
	root := &node{files: map[string]string{}, dirs: map[string]*node{}}
	for path, oid := range files {
		parts := strings.Split(path, "/")
		n := root
		for _, d := range parts[:len(parts)-1] {
			if n.dirs[d] == nil {
				n.dirs[d] = &node{files: map[string]string{}, dirs: map[string]*node{}}
			}
			n = n.dirs[d]
		}
		n.files[parts[len(parts)-1]] = oid
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []TreeEntry
		for name, oid := range n.files {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		data := buildTree(entries)
		oid := hashObject("tree", data)
		if store[oid] == nil {
			store[oid] = &Object{Type: "tree", Data: data}
		}
		return oid
	}
	return build(root)
}

// ---------------------------------------------------------------------------
// commits and tags
// ---------------------------------------------------------------------------

type Commit struct {
	Tree    string
	Parents []string
	Message string
	Author  string // full "Name <email> ts tz" line value
	Time    int64
}

func parseCommit(data []byte) *Commit {
	c := &Commit{}
	head := data
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		head = data[:i]
		c.Message = string(data[i+2:])
	} else {
		c.Message = ""
	}
	for _, line := range strings.Split(string(head), "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			c.Author = line[7:]
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64); err == nil {
					c.Time = ts
				}
			}
		}
	}
	return c
}

func buildCommit(tree string, parents []string, authorLine, message string) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p + "\n")
	}
	buf.WriteString("author " + authorLine + "\n")
	buf.WriteString("committer " + authorLine + "\n")
	buf.WriteString("\n")
	buf.WriteString(message)
	return buf.Bytes()
}

func parseTagTarget(data []byte) string {
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

// peelToCommit follows tag objects until a commit (or returns "" ).
func peelToCommit(store map[string]*Object, oid string) string {
	seen := map[string]bool{}
	for {
		obj := store[oid]
		if obj == nil || seen[oid] {
			return ""
		}
		seen[oid] = true
		switch obj.Type {
		case "commit":
			return oid
		case "tag":
			oid = parseTagTarget(obj.Data)
			if oid == "" {
				return ""
			}
		default:
			return ""
		}
	}
}

// ---------------------------------------------------------------------------
// reachability
// ---------------------------------------------------------------------------

// reachable returns every object reachable from the given tips.
func reachable(store map[string]*Object, tips []string) map[string]bool {
	out := map[string]bool{}
	stack := append([]string(nil), tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[oid] {
			continue
		}
		obj := store[oid]
		if obj == nil {
			continue
		}
		out[oid] = true
		switch obj.Type {
		case "commit":
			c := parseCommit(obj.Data)
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case "tree":
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.OID)
			}
		case "tag":
			if t := parseTagTarget(obj.Data); t != "" {
				stack = append(stack, t)
			}
		}
	}
	return out
}

// isAncestor reports whether anc is reachable from desc through parents.
func isAncestor(store map[string]*Object, anc, desc string) bool {
	if anc == desc {
		return true
	}
	seen := map[string]bool{}
	stack := []string{desc}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == anc {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := store[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		stack = append(stack, parseCommit(obj.Data).Parents...)
	}
	return false
}

// mergeBase finds a best common ancestor of a and b (BFS from both).
func mergeBase(store map[string]*Object, a, b string) string {
	ancA := map[string]bool{}
	queue := []string{a}
	ancA[a] = true
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		obj := store[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if !ancA[p] {
				ancA[p] = true
				queue = append(queue, p)
			}
		}
	}
	seen := map[string]bool{}
	queue = []string{b}
	seen[b] = true
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if ancA[oid] {
			return oid
		}
		obj := store[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return ""
}

// commitAncestors lists commits reachable from tip, newest first
// (by commit time, then by discovery order for ties).
func commitAncestors(store map[string]*Object, tip string) []string {
	seen := map[string]bool{}
	var all []string
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := store[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		seen[oid] = true
		all = append(all, oid)
		stack = append(stack, parseCommit(obj.Data).Parents...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		ci, cj := parseCommit(store[all[i]].Data), parseCommit(store[all[j]].Data)
		return ci.Time > cj.Time
	})
	return all
}

// ---------------------------------------------------------------------------
// fetch set computation: depth-limited closure minus have-closure, with
// shallow boundary reporting.
// ---------------------------------------------------------------------------

type fetchPlan struct {
	Send      map[string]bool // objects to put in the pack
	Shallow   []string        // boundary commits in this response
	Unshallow []string        // previously-shallow commits now internal
}

// planFetch computes what upload-pack should send.
//
//	wants: requested tip oids (any object type)
//	haves: oids the client already holds (excluded with their closure)
//	clientShallow: the client's current shallow boundary commits
//	depth: 0 = unlimited; else max commit generations from the wants
//	since: 0 = unused; else deepen-since epoch seconds
//	filter: "" | "blob:none" | "blob:limit=n" | "tree:0"
func planFetch(store map[string]*Object, wants, haves, clientShallow []string, depth int, since int64, filter string) *fetchPlan {
	// commit generation depth from wants (peeling tags)
	gen := map[string]int{}
	var queue []string
	var extraTips []string // non-commit wants (tags are peeled; trees/blobs sent directly)
	for _, w := range wants {
		obj := store[w]
		if obj == nil {
			continue
		}
		if obj.Type == "tag" {
			extraTips = append(extraTips, w)
			if c := peelToCommit(store, w); c != "" {
				if _, ok := gen[c]; !ok {
					gen[c] = 1
					queue = append(queue, c)
				}
			}
			continue
		}
		if obj.Type == "commit" {
			if _, ok := gen[w]; !ok {
				gen[w] = 1
				queue = append(queue, w)
			}
		} else {
			extraTips = append(extraTips, w)
		}
	}
	included := map[string]bool{}
	for _, t := range extraTips {
		// include the tip object itself; for a tag, walk the tag chain to
		// (but not into) the commit, which the gen walk governs. A tree or
		// blob tip brings its own closure.
		oid := t
		for {
			obj := store[oid]
			if obj == nil {
				break
			}
			if obj.Type == "tag" {
				included[oid] = true
				oid = parseTagTarget(obj.Data)
				if oid == "" {
					break
				}
				continue
			}
			if obj.Type == "commit" {
				break // handled by the gen walk
			}
			for sub := range reachable(store, []string{oid}) {
				included[sub] = true
			}
			break
		}
	}
	boundary := map[string]bool{}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		g := gen[oid]
		obj := store[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		c := parseCommit(obj.Data)
		if since > 0 && c.Time < since && g > 1 {
			boundary[oid] = true
			continue
		}
		included[oid] = true
		// the commit's tree (and its closure) rides along
		for sub := range reachable(store, []string{c.Tree}) {
			included[sub] = true
		}
		for _, p := range c.Parents {
			if _, ok := gen[p]; ok {
				continue // already scheduled within the depth budget
			}
			if depth > 0 && g+1 > depth {
				if store[p] != nil {
					boundary[oid] = true
				}
				continue
			}
			if since > 0 {
				if pobj := store[p]; pobj != nil && pobj.Type == "commit" {
					if parseCommit(pobj.Data).Time < since {
						boundary[oid] = true
						continue
					}
				}
			}
			if _, ok := gen[p]; !ok {
				gen[p] = g + 1
				queue = append(queue, p)
			}
		}
	}
	// filters
	if filter != "" {
		kind := filter
		if strings.HasPrefix(kind, "blob:limit=") {
			kind = "blob:limit"
		}
		switch kind {
		case "blob:none":
			for oid := range included {
				if store[oid].Type == "blob" {
					delete(included, oid)
				}
			}
		case "tree:0":
			for oid := range included {
				if store[oid].Type == "tree" || store[oid].Type == "blob" {
					delete(included, oid)
				}
			}
		}
	}
	// subtract the have closure — but possession does not extend past the
	// client's declared shallow boundary: a shallow commit's parents are
	// NOT held, even though the commit and its tree are.
	if len(haves) > 0 {
		shallowHave := map[string]bool{}
		for _, s := range clientShallow {
			shallowHave[s] = true
		}
		haveSet := map[string]bool{}
		seenCommit := map[string]bool{}
		stack := append([]string{}, haves...)
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			obj := store[oid]
			if obj == nil {
				continue
			}
			if obj.Type == "commit" {
				if seenCommit[oid] {
					continue
				}
				seenCommit[oid] = true
				haveSet[oid] = true
				c := parseCommit(obj.Data)
				for sub := range reachable(store, []string{c.Tree}) {
					haveSet[sub] = true
				}
				if !shallowHave[oid] {
					stack = append(stack, c.Parents...)
				}
				continue
			}
			for sub := range reachable(store, []string{oid}) {
				haveSet[sub] = true
			}
		}
		for oid := range haveSet {
			delete(included, oid)
		}
	}
	// shallow boundary: included commits with a stored parent not included
	var shallow []string
	for oid := range boundary {
		if included[oid] {
			shallow = append(shallow, oid)
		}
	}
	sort.Strings(shallow)
	// unshallow: client boundary commits that are now included with all
	// parents included too (no longer on the boundary)
	boundarySet := map[string]bool{}
	for _, s := range shallow {
		boundarySet[s] = true
	}
	var unshallow []string
	for _, cs := range clientShallow {
		if included[cs] && !boundarySet[cs] {
			// only unshallow when the commit's parents are present as well,
			// or the commit is a root
			obj := store[cs]
			if obj != nil && obj.Type == "commit" {
				parents := parseCommit(obj.Data).Parents
				allIn := true
				for _, p := range parents {
					if store[p] != nil && !included[p] {
						allIn = false
					}
				}
				if allIn {
					unshallow = append(unshallow, cs)
				}
			}
		}
	}
	sort.Strings(unshallow)
	return &fetchPlan{Send: included, Shallow: shallow, Unshallow: unshallow}
}

// ---------------------------------------------------------------------------
// packfiles
// ---------------------------------------------------------------------------

// zlib streams are created per pack entry; pooling the reader/writer
// state avoids re-initialising the flate machinery (huffman tables,
// window buffers) for every object.
var zlibReaders sync.Pool
var zlibWriters sync.Pool

type zlibResetter interface{ Reset(io.Reader, []byte) error }

func acquireZlibReader(r io.Reader) (io.ReadCloser, error) {
	if v := zlibReaders.Get(); v != nil {
		zr := v.(io.ReadCloser)
		if rs, ok := zr.(zlibResetter); ok {
			if err := rs.Reset(r, nil); err == nil {
				return zr, nil
			}
		}
	}
	return zlib.NewReader(r)
}

func acquireZlibWriter(w io.Writer) *zlib.Writer {
	if v := zlibWriters.Get(); v != nil {
		zw := v.(*zlib.Writer)
		zw.Reset(w)
		return zw
	}
	zw, _ := zlib.NewWriterLevel(w, zlib.DefaultCompression)
	return zw
}

// writePack serialises the given objects (sorted for determinism) as a
// version-2 pack with no deltas.
func writePack(store map[string]*Object, oids map[string]bool) []byte {
	list := make([]string, 0, len(oids))
	for oid := range oids {
		if store[oid] != nil {
			list = append(list, oid)
		}
	}
	sort.Strings(list)
	var buf bytes.Buffer
	buf.WriteString("PACK")
	buf.Write([]byte{0, 0, 0, 2})
	n := len(list)
	buf.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	for _, oid := range list {
		obj := store[oid]
		writePackObjectHeader(&buf, typeCode[obj.Type], len(obj.Data))
		zw := acquireZlibWriter(&buf)
		zw.Write(obj.Data)
		zw.Close()
		zlibWriters.Put(zw)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func writePackObjectHeader(buf *bytes.Buffer, code byte, size int) {
	b := byte(code<<4) | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		buf.WriteByte(b | 0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	buf.WriteByte(b)
}

// readPack parses a pack into the store (or a scratch map when store is a
// plain map), resolving ofs/ref deltas. Returns the object ids added.
func readPack(data []byte, into map[string]*Object, resolveBase func(oid string) *Object) ([]string, error) {
	if len(data) < 12+20 {
		return nil, errors.New("pack too short")
	}
	if string(data[:4]) != "PACK" {
		return nil, errors.New("bad pack signature")
	}
	version := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("pack version %d", version)
	}
	count := int(data[8])<<24 | int(data[9])<<16 | int(data[10])<<8 | int(data[11])
	body := data[:len(data)-20]
	trailer := data[len(data)-20:]
	sum := sha1.Sum(body)
	if !bytes.Equal(sum[:], trailer) {
		return nil, errors.New("pack checksum mismatch")
	}
	pos := 12
	type pending struct {
		kind string // ofs | ref
		ofs  int
		ref  string
		typ  string
		data []byte // delta payload
	}
	var order []string
	byOffset := map[int]string{}
	var pend []pending
	add := func(typ string, payload []byte) string {
		oid := hashObject(typ, payload)
		if into[oid] == nil {
			into[oid] = &Object{Type: typ, Data: payload}
			order = append(order, oid)
		}
		return oid
	}
	for i := 0; i < count; i++ {
		if pos >= len(body) {
			return nil, errors.New("pack truncated")
		}
		start := pos
		b := body[pos]
		pos++
		code := (b >> 4) & 0x7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = body[pos]
			pos++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		typ := codeType[code]
		var baseOID string
		var baseOfs int
		switch code {
		case 6: // ofs-delta
			b = body[pos]
			pos++
			off := int(b & 0x7f)
			for b&0x80 != 0 {
				b = body[pos]
				pos++
				off = ((off + 1) << 7) | int(b&0x7f)
			}
			baseOfs = start - off
		case 7: // ref-delta
			if pos+20 > len(body) {
				return nil, errors.New("pack truncated at ref-delta base")
			}
			baseOID = hex.EncodeToString(body[pos : pos+20])
			pos += 20
		}
		// inflate exactly this entry's zlib stream: a bytes.Reader is an
		// io.ByteReader, so zlib consumes precisely the stream's bytes.
		br := bytes.NewReader(body[pos:])
		zr, err := acquireZlibReader(br)
		if err != nil {
			return nil, fmt.Errorf("zlib: %w", err)
		}
		var pb bytes.Buffer
		if size > 0 && size <= 1<<28 {
			pb.Grow(size) // the entry header declares the inflated size
		}
		_, err = io.Copy(&pb, zr)
		if err != nil {
			zr.Close()
			return nil, fmt.Errorf("inflate: %w", err)
		}
		zlibReaders.Put(zr)
		payload := pb.Bytes()
		consumed := len(body[pos:]) - br.Len()
		pos += consumed
		if len(payload) != size {
			return nil, fmt.Errorf("pack entry size mismatch: header %d, inflated %d", size, len(payload))
		}
		switch code {
		case 6:
			base, ok := byOffset[baseOfs]
			if !ok {
				pend = append(pend, pending{kind: "ofs", ofs: baseOfs, data: payload})
				byOffset[start] = ""
				continue
			}
			baseObj := into[base]
			if baseObj == nil {
				return nil, errors.New("ofs-delta base missing")
			}
			out, err := applyDelta(baseObj.Data, payload)
			if err != nil {
				return nil, err
			}
			byOffset[start] = add(baseObj.Type, out)
		case 7:
			baseObj := into[baseOID]
			if baseObj == nil && resolveBase != nil {
				baseObj = resolveBase(baseOID)
			}
			if baseObj == nil {
				pend = append(pend, pending{kind: "ref", ref: baseOID, data: payload})
				byOffset[start] = ""
				continue
			}
			out, err := applyDelta(baseObj.Data, payload)
			if err != nil {
				return nil, err
			}
			byOffset[start] = add(baseObj.Type, out)
		default:
			byOffset[start] = add(typ, payload)
		}
	}
	// resolve any out-of-order deltas (shouldn't happen in valid packs,
	// which order bases first, but be liberal)
	for len(pend) > 0 {
		progress := false
		var next []pending
		for _, p := range pend {
			var baseObj *Object
			if p.kind == "ofs" {
				if oid, ok := byOffset[p.ofs]; ok && oid != "" {
					baseObj = into[oid]
				}
			} else {
				baseObj = into[p.ref]
				if baseObj == nil && resolveBase != nil {
					baseObj = resolveBase(p.ref)
				}
			}
			if baseObj == nil {
				next = append(next, p)
				continue
			}
			out, err := applyDelta(baseObj.Data, p.data)
			if err != nil {
				return nil, err
			}
			add(baseObj.Type, out)
			progress = true
		}
		if !progress {
			return nil, errors.New("unresolvable delta in pack")
		}
		pend = next
	}
	return order, nil
}

// applyDelta applies a git delta payload to a base object's data.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		v := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[pos]
			pos++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size %d, expected %d", len(base), srcSize)
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
			off, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					off |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					size |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out.Write(base[off : off+size])
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta insert out of range")
			}
			out.Write(delta[pos : pos+int(op)])
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if out.Len() != dstSize {
		return nil, fmt.Errorf("delta result size %d, expected %d", out.Len(), dstSize)
	}
	return out.Bytes(), nil
}
