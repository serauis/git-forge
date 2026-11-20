package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"

// ---------------------------------------------------------------------------
// pkt-lines
// ---------------------------------------------------------------------------

const hexDigits = "0123456789abcdef"

func pktLine(payload string) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, hexDigits[(n>>12)&0xf], hexDigits[(n>>8)&0xf],
		hexDigits[(n>>4)&0xf], hexDigits[n&0xf])
	out = append(out, payload...)
	return out
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

type pktReader struct {
	data []byte
	pos  int
}

// next returns (payload, kind) where kind is "data", "flush", "delim".
func (p *pktReader) next() ([]byte, string, error) {
	if p.pos >= len(p.data) {
		return nil, "eof", nil
	}
	if p.pos+4 > len(p.data) {
		return nil, "", fmt.Errorf("truncated pkt-line header")
	}
	n, err := strconv.ParseUint(string(p.data[p.pos:p.pos+4]), 16, 32)
	if err != nil {
		return nil, "", fmt.Errorf("bad pkt-line length")
	}
	switch n {
	case 0:
		p.pos += 4
		return nil, "flush", nil
	case 1:
		p.pos += 4
		return nil, "delim", nil
	case 2:
		p.pos += 4
		return nil, "response-end", nil
	case 3:
		p.pos += 4
		return nil, "data", nil // empty payload marker; treat as data
	}
	if n < 4 || p.pos+int(n) > len(p.data) {
		return nil, "", fmt.Errorf("pkt-line overruns body")
	}
	payload := p.data[p.pos+4 : p.pos+int(n)]
	p.pos += int(n)
	return payload, "data", nil
}

// ---------------------------------------------------------------------------
// routing helpers
// ---------------------------------------------------------------------------

// gitPath splits "/{owner}/{repo}.git/..." into owner, repo, rest.
func gitPath(path string) (string, string, string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	idx := strings.Index(trimmed, ".git")
	if idx < 0 {
		return "", "", "", false
	}
	ownerRepo := trimmed[:idx]
	rest := trimmed[idx+4:]
	slash := strings.Index(ownerRepo, "/")
	if slash < 0 {
		return "", "", "", false
	}
	return ownerRepo[:slash], ownerRepo[slash+1:], rest, true
}

var packCacheMu sync.Mutex

func (r *Repo) cachedPack(key string) []byte {
	packCacheMu.Lock()
	defer packCacheMu.Unlock()
	return r.packCache[key]
}

func (r *Repo) putCachedPack(key string, pack []byte) {
	packCacheMu.Lock()
	defer packCacheMu.Unlock()
	if len(r.packCache) > 64 {
		r.invalidateCaches()
	}
	r.packCache[key] = pack
}

func (r *Repo) cachedPlan(key string) *fetchPlan {
	packCacheMu.Lock()
	defer packCacheMu.Unlock()
	return r.planCache[key]
}

func (r *Repo) putCachedPlan(key string, plan *fetchPlan) {
	packCacheMu.Lock()
	defer packCacheMu.Unlock()
	if len(r.planCache) > 64 {
		r.planCache = map[string]*fetchPlan{}
	}
	r.planCache[key] = plan
}

// ---------------------------------------------------------------------------
// advertisements
// ---------------------------------------------------------------------------

func sortedRefNames(refs map[string]string) []string {
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (r *Repo) cachedAdv(key string) []byte {
	packCacheMu.Lock()
	defer packCacheMu.Unlock()
	return r.advCache[key]
}

func (r *Repo) putCachedAdv(key string, b []byte) {
	packCacheMu.Lock()
	defer packCacheMu.Unlock()
	r.advCache[key] = b
}

func (s *Server) handleInfoRefs(w http.ResponseWriter, req *http.Request, owner, repoName string) {
	service := req.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		http.Error(w, "unknown service", http.StatusBadRequest)
		return
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.store.repo(owner, repoName)
	if repo == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("repository not found"))
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	wantV2 := service == "git-upload-pack" && strings.Contains(req.Header.Get("Git-Protocol"), "version=2")
	// the advertisement is a pure function of the repo's refs and default
	// branch; every push pays one of these, every clone one or two
	advKey := service + "|v0"
	if wantV2 {
		advKey = service + "|v2"
	}
	if cached := repo.cachedAdv(advKey); cached != nil {
		w.Write(cached)
		return
	}
	var buf bytes.Buffer
	buf.Write(pktLine("# service=" + service + "\n"))
	buf.Write(pktFlush())
	if wantV2 {
		buf.Write(pktLine("version 2\n"))
		buf.Write(pktLine("agent=forge/1.0\n"))
		buf.Write(pktLine("ls-refs=unborn\n"))
		buf.Write(pktLine("fetch=shallow filter wait-for-done\n"))
		buf.Write(pktLine("object-format=sha1\n"))
		buf.Write(pktFlush())
	} else if service == "git-upload-pack" {
		caps := "side-band-64k multi_ack_detailed thin-pack ofs-delta shallow agent=forge/1.0 symref=HEAD:" + repo.headRef()
		writeV0Advertisement(&buf, repo, caps)
	} else {
		caps := "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"
		writeV0Advertisement(&buf, repo, caps)
	}
	repo.putCachedAdv(advKey, buf.Bytes())
	w.Write(buf.Bytes())
}

func writeV0Advertisement(buf *bytes.Buffer, repo *Repo, caps string) {
	headTarget := repo.headRef()
	headOID, hasHead := repo.Refs[headTarget]
	if len(repo.Refs) == 0 {
		buf.Write(pktLine(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		first := true
		if hasHead {
			buf.Write(pktLine(headOID + " HEAD\x00" + caps + "\n"))
			first = false
		}
		for _, name := range sortedRefNames(repo.Refs) {
			if first {
				buf.Write(pktLine(repo.Refs[name] + " " + name + "\x00" + caps + "\n"))
				first = false
			} else {
				buf.Write(pktLine(repo.Refs[name] + " " + name + "\n"))
			}
			if obj := repo.Objects[repo.Refs[name]]; obj != nil && obj.Type == "tag" {
				if c := peelToCommit(repo.Objects, repo.Refs[name]); c != "" {
					buf.Write(pktLine(c + " " + name + "^{}\n"))
				}
			}
		}
		if first {
			// refs exist but HEAD target is not among them (shouldn't happen)
			buf.Write(pktLine(zeroOID + " capabilities^{}\x00" + caps + "\n"))
		}
	}
	buf.Write(pktFlush())
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

type fetchArgs struct {
	wants    []string
	haves    []string
	shallow  []string
	depth    int
	since    int64
	filter   string
	done     bool
	haveSent bool
}

func (s *Server) handleUploadPack(w http.ResponseWriter, req *http.Request, owner, repoName string) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<26))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.store.repo(owner, repoName)
	if repo == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	pr := &pktReader{data: body}
	first, kind, err := pr.next()
	if err != nil || kind != "data" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("malformed pkt-line"))
		return
	}
	firstLine := strings.TrimRight(string(first), "\n")
	if strings.HasPrefix(firstLine, "command=ls-refs") {
		s.serveLsRefs(w, repo, pr)
		return
	}
	if strings.HasPrefix(firstLine, "command=fetch") {
		s.serveFetchV2(w, repo, pr)
		return
	}
	if strings.HasPrefix(firstLine, "want ") {
		s.serveFetchV0(w, repo, pr, firstLine, body)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	w.Write([]byte("unknown upload-pack command"))
}

// readArgLines reads pkt-lines until flush/delim/eof into trimmed lines.
func readLines(pr *pktReader, first *string) ([]string, error) {
	var lines []string
	if first != nil {
		lines = append(lines, *first)
	}
	for {
		payload, kind, err := pr.next()
		if err != nil {
			return lines, err
		}
		if kind == "flush" || kind == "eof" || kind == "response-end" {
			return lines, nil
		}
		if kind == "delim" {
			continue
		}
		lines = append(lines, strings.TrimRight(string(payload), "\n"))
	}
}

// splitV2Args separates capability lines (before delim) from argument lines.
func splitV2Args(pr *pktReader) (args []string, err error) {
	// capability lines until delim (or flush)
	for {
		payload, kind, err := pr.next()
		if err != nil {
			return nil, err
		}
		if kind == "delim" {
			break
		}
		if kind == "flush" || kind == "eof" {
			return nil, nil
		}
		// capability line: ignore contents
		_ = payload
	}
	for {
		payload, kind, err := pr.next()
		if err != nil {
			return args, err
		}
		if kind == "flush" || kind == "eof" || kind == "response-end" {
			return args, nil
		}
		if kind == "delim" {
			continue
		}
		args = append(args, strings.TrimRight(string(payload), "\n"))
	}
}

func (s *Server) serveLsRefs(w http.ResponseWriter, repo *Repo, pr *pktReader) {
	args, err := splitV2Args(pr)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-prefix "))
		}
	}
	match := func(name string) bool {
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
	headTarget := repo.headRef()
	headOID, hasHead := repo.Refs[headTarget]
	if match("HEAD") {
		if hasHead {
			line := headOID + " HEAD"
			if symrefs {
				line += " symref-target:" + headTarget
			}
			buf.Write(pktLine(line + "\n"))
		} else if unborn {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:" + headTarget
			}
			buf.Write(pktLine(line + "\n"))
		}
	}
	for _, name := range sortedRefNames(repo.Refs) {
		if !match(name) {
			continue
		}
		oid := repo.Refs[name]
		line := oid + " " + name
		if peel {
			if obj := repo.Objects[oid]; obj != nil && obj.Type == "tag" {
				if c := peelToCommit(repo.Objects, oid); c != "" {
					line += " peeled:" + c
				}
			}
		}
		buf.Write(pktLine(line + "\n"))
	}
	buf.Write(pktFlush())
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Write(buf.Bytes())
}

func parseFetchArgs(lines []string) fetchArgs {
	var fa fetchArgs
	for _, a := range lines {
		switch {
		case strings.HasPrefix(a, "want "):
			fa.wants = append(fa.wants, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "have "):
			fa.haves = append(fa.haves, strings.TrimSpace(a[5:]))
			fa.haveSent = true
		case strings.HasPrefix(a, "shallow "):
			fa.shallow = append(fa.shallow, strings.TrimSpace(a[8:]))
		case strings.HasPrefix(a, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(a[7:])); err == nil {
				fa.depth = n
			}
		case strings.HasPrefix(a, "deepen-since "):
			if n, err := strconv.ParseInt(strings.TrimSpace(a[13:]), 10, 64); err == nil {
				fa.since = n
			}
		case strings.HasPrefix(a, "filter "):
			fa.filter = strings.TrimSpace(a[7:])
		case a == "done":
			fa.done = true
		}
	}
	return fa
}

func fetchCacheKey(fa fetchArgs) string {
	h := sha1.New()
	ws := append([]string(nil), fa.wants...)
	hs := append([]string(nil), fa.haves...)
	sort.Strings(ws)
	sort.Strings(hs)
	fmt.Fprintf(h, "w:%s|h:%s|d:%d|s:%d|f:%s", strings.Join(ws, ","), strings.Join(hs, ","), fa.depth, fa.since, fa.filter)
	return hex.EncodeToString(h.Sum(nil))
}

// buildFetchPack returns the pack bytes for a fetch (cached). The plan is
// a pure function of the object store and the fetch arguments, so it is
// cached alongside the pack and invalidated with it.
func (s *Server) buildFetchPack(repo *Repo, fa fetchArgs) ([]byte, *fetchPlan) {
	key := fetchCacheKey(fa)
	pack := repo.cachedPack(key)
	plan := repo.cachedPlan(key)
	if pack != nil && plan != nil {
		return pack, plan
	}
	if plan == nil {
		plan = planFetch(repo.Objects, fa.wants, fa.haves, fa.shallow, fa.depth, fa.since, fa.filter)
		repo.putCachedPlan(key, plan)
	}
	if pack == nil {
		pack = writePack(repo.Objects, plan.Send)
		repo.putCachedPack(key, pack)
	}
	return pack, plan
}

func sidebandChunks(buf *bytes.Buffer, pack []byte) {
	const maxData = 65515 // 65520 pkt-line max, minus 4 length bytes and 1 band byte
	for len(pack) > 0 {
		n := maxData
		if len(pack) < n {
			n = len(pack)
		}
		chunk := make([]byte, 0, n+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, pack[:n]...)
		pack = pack[n:]
		m := len(chunk) + 4
		buf.Write([]byte{hexDigits[(m>>12)&0xf], hexDigits[(m>>8)&0xf],
			hexDigits[(m>>4)&0xf], hexDigits[m&0xf]})
		buf.Write(chunk)
	}
}

func (s *Server) serveFetchV2(w http.ResponseWriter, repo *Repo, pr *pktReader) {
	argLines, err := splitV2Args(pr)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	fa := parseFetchArgs(argLines)
	var buf bytes.Buffer
	if len(fa.wants) == 0 {
		// nothing to fetch: an empty packfile section (null pack)
		buf.Write(pktLine("packfile\n"))
		buf.Write(pktFlush())
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Write(buf.Bytes())
		return
	}
	pack, plan := s.buildFetchPack(repo, fa)
	if fa.haveSent {
		buf.Write(pktLine("acknowledgments\n"))
		acked := false
		for _, h := range fa.haves {
			if repo.Objects[h] != nil {
				buf.Write(pktLine("ACK " + h + "\n"))
				acked = true
			}
		}
		if !acked {
			buf.Write(pktLine("NAK\n"))
		}
		buf.Write(pktFlush())
	}
	if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
		buf.Write(pktLine("shallow-info\n"))
		for _, sh := range plan.Shallow {
			buf.Write(pktLine("shallow " + sh + "\n"))
		}
		for _, un := range plan.Unshallow {
			buf.Write(pktLine("unshallow " + un + "\n"))
		}
		buf.Write(pktFlush())
	}
	buf.Write(pktLine("packfile\n"))
	sidebandChunks(&buf, pack)
	buf.Write(pktFlush())
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Write(buf.Bytes())
}

func (s *Server) serveFetchV0(w http.ResponseWriter, repo *Repo, pr *pktReader, firstLine string, body []byte) {
	// first section: want lines (+ shallow/deepen/filter), already have the
	// first want line in hand; read until flush.
	var fa fetchArgs
	lines := []string{stripCaps(firstLine)}
	for {
		payload, kind, err := pr.next()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if kind == "flush" || kind == "eof" {
			break
		}
		if kind == "data" {
			lines = append(lines, strings.TrimRight(string(payload), "\n"))
		}
	}
	// second section: have lines + done
	for {
		payload, kind, err := pr.next()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if kind == "flush" || kind == "eof" {
			break
		}
		if kind == "data" {
			lines = append(lines, strings.TrimRight(string(payload), "\n"))
		}
	}
	fa = parseFetchArgs(lines)
	var buf bytes.Buffer
	if len(fa.wants) == 0 {
		buf.Write(pktLine("NAK\n"))
		buf.Write(pktFlush())
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Write(buf.Bytes())
		return
	}
	pack, plan := s.buildFetchPack(repo, fa)
	for _, sh := range plan.Shallow {
		buf.Write(pktLine("shallow " + sh + "\n"))
	}
	for _, un := range plan.Unshallow {
		buf.Write(pktLine("unshallow " + un + "\n"))
	}
	acked := false
	for _, h := range fa.haves {
		if repo.Objects[h] != nil {
			buf.Write(pktLine("ACK " + h + " common\n"))
			acked = true
		}
	}
	if !acked {
		buf.Write(pktLine("NAK\n"))
	} else {
		// final ACK without "common" for the last have, like git
	}
	sidebandChunks(&buf, pack)
	buf.Write(pktFlush())
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Write(buf.Bytes())
}

func stripCaps(line string) string {
	if i := strings.IndexByte(line, 0); i >= 0 {
		return line[:i]
	}
	return line
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	Old string
	New string
	Ref string
}

func (s *Server) handleReceivePack(w http.ResponseWriter, req *http.Request, owner, repoName string) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<26))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	s.store.lockWrite()
	defer s.store.mu.Unlock()
	repo := s.store.repo(owner, repoName)
	if repo == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	pr := &pktReader{data: body}
	var commands []pushCommand
	sideband := false
	first := true
	for {
		payload, kind, err := pr.next()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("malformed pkt-line"))
			return
		}
		if kind == "flush" || kind == "eof" {
			break
		}
		if kind != "data" {
			continue
		}
		line := string(payload)
		if first {
			first = false
			if i := strings.IndexByte(line, 0); i >= 0 {
				caps := line[i+1:]
				if strings.Contains(caps, "side-band-64k") {
					sideband = true
				}
				line = line[:i]
			}
		}
		line = strings.TrimRight(line, "\n")
		fields := strings.Fields(line)
		if len(fields) != 3 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("malformed ref update"))
			return
		}
		commands = append(commands, pushCommand{Old: fields[0], New: fields[1], Ref: fields[2]})
	}
	rest := body[pr.pos:]
	needPack := false
	for _, c := range commands {
		if c.New != zeroOID {
			needPack = true
		}
	}
	unpackOK := true
	unpackDetail := "ok"
	if needPack {
		if len(rest) == 0 {
			unpackOK = false
			unpackDetail = "no pack sent"
		} else {
			// unpack into a small overlay, resolving delta bases against
			// the repo, then merge only the new objects: copying the whole
			// object map per push is O(repo) and dominates this path once
			// the repository grows.
			overlay := map[string]*Object{}
			resolveBase := func(oid string) *Object { return repo.Objects[oid] }
			if _, err := readPack(rest, overlay, resolveBase); err != nil {
				unpackOK = false
				unpackDetail = err.Error()
			} else {
				for k, v := range overlay {
					repo.Objects[k] = v
				}
			}
		}
	}
	type result struct {
		ref    string
		ok     bool
		reason string
	}
	var results []result
	if unpackOK && len(commands) > 0 {
		for _, c := range commands {
			current := repo.Refs[c.Ref]
			res := result{ref: c.Ref, ok: true}
			switch {
			case c.New == zeroOID: // delete
				if current == "" && c.Old != zeroOID {
					res.ok, res.reason = false, "reference does not exist"
				} else if current != "" && c.Old != current {
					res.ok, res.reason = false, "stale ref value"
				}
			case c.Old == zeroOID: // create
				if current != "" {
					res.ok, res.reason = false, "reference already exists"
				} else if repo.Objects[c.New] == nil {
					res.ok, res.reason = false, "invalid object"
				}
			default: // update
				if current == "" {
					res.ok, res.reason = false, "reference does not exist"
				} else if c.Old != current {
					res.ok, res.reason = false, "stale ref value"
				} else if repo.Objects[c.New] == nil {
					res.ok, res.reason = false, "invalid object"
				} else if !isAncestor(repo.Objects, current, c.New) {
					res.ok, res.reason = false, "non-fast-forward"
				}
			}
			if res.ok {
				if c.New == zeroOID {
					delete(repo.Refs, c.Ref)
				} else {
					repo.Refs[c.Ref] = c.New
				}
			}
			results = append(results, res)
		}
		repo.invalidateCaches()
	}
	// report-status
	var report bytes.Buffer
	if unpackOK {
		report.Write(pktLine("unpack ok\n"))
	} else {
		report.Write(pktLine("unpack " + unpackDetail + "\n"))
	}
	for _, res := range results {
		if res.ok {
			report.Write(pktLine("ok " + res.ref + "\n"))
		} else {
			report.Write(pktLine("ng " + res.ref + " " + res.reason + "\n"))
		}
	}
	report.Write(pktFlush())
	var out bytes.Buffer
	if sideband {
		sidebandChunks(&out, report.Bytes())
		out.Write(pktFlush())
	} else {
		out.Write(report.Bytes())
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Write(out.Bytes())
}
