package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// The forge's store: everything in memory, one mutex. The viewer is the
// token's owner; the sealed suite's scenario fixes that login.
// ---------------------------------------------------------------------------

const (
	viewerLogin = "adaiversen"
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
	viewerID    = 1000
)

const clockBase = 1739175597 // scenario commits[0] epoch; a logical clock

type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type Comment struct {
	ID        int64
	Body      string
	CreatedAt int64
}

type Issue struct {
	GID       int64 // REST "id": a global numeric id
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	Comments  []*Comment
	CreatedAt int64
	ClosedAt  int64 // 0 = not closed

	// bodyCache memoises this issue's REST JSON under a fingerprint of
	// every field that enters it (see issueBodyKey in rest.go); any
	// mutation anywhere changes the fingerprint, so the memo can never
	// go stale.
	bodyCache    []byte
	bodyCacheKey uint64
}

type Pull struct {
	GID            int64
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Head           string // branch name
	Base           string // branch name
	Merged         bool
	MergeCommitSha string
	MergedAt       int64
	CreatedAt      int64
}

type Repo struct {
	ID            int64
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     int64

	Objects map[string]*Object
	Refs    map[string]string // full ref name -> oid
	Labels  map[string]*Label
	Issues  map[int]*Issue
	Pulls   map[int]*Pull
	Seq     int // shared issue/PR number sequence

	packCache map[string][]byte
	planCache map[string]*fetchPlan
	advCache  map[string][]byte
}

// invalidateCaches drops every derived fetch artifact; the object store or
// a ref changed, so cached packs, fetch plans and ref advertisements may
// be stale.
func (r *Repo) invalidateCaches() {
	r.packCache = map[string][]byte{}
	r.planCache = map[string]*fetchPlan{}
	r.advCache = map[string][]byte{}
}

func (r *Repo) FullName() string { return r.Owner + "/" + r.Name }

func (r *Repo) headRef() string { return "refs/heads/" + r.DefaultBranch }

func (r *Repo) tip(branch string) string { return r.Refs["refs/heads/"+branch] }

type Store struct {
	mu         sync.RWMutex
	repos      map[string]*Repo // full name -> repo
	repoSeq    int64
	issueSeq   int64 // global REST id counter for issues and pulls
	commentSeq int64
	clock      int64 // logical seconds since clockBase
	reqSeq     int64
}

func newStore() *Store {
	return &Store{repos: map[string]*Repo{}}
}

// tick advances the logical clock and returns an epoch second.
func (s *Store) tick() int64 {
	s.clock++
	return clockBase + s.clock
}

// formatTime renders the GitHub timestamp for a logical-clock epoch.
// The distinct epochs in a run are few and reads repeat the same one
// thousands of times, so the last rendering is memoised (the epoch is
// checked on every hit, and a lost store just recomputes).
var fmtTimeEpoch atomic.Int64
var fmtTimeStr atomic.Value // string

func formatTime(epoch int64) string {
	if fmtTimeEpoch.Load() == epoch {
		if v := fmtTimeStr.Load(); v != nil {
			return v.(string)
		}
	}
	str := time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
	fmtTimeStr.Store(str)
	fmtTimeEpoch.Store(epoch)
	return str
}

func (s *Store) requestID() string {
	n := atomic.AddInt64(&s.reqSeq, 1)
	v := uint64(n) * 2654435761 % (1 << 48)
	var buf [16]byte
	for i := 15; i >= 0; i-- {
		buf[i] = "0123456789abcdef"[v&0xf]
		v >>= 4
	}
	return string(buf[:])
}

func (s *Store) createRepo(owner, name, description string, private bool) *Repo {
	s.repoSeq++
	r := &Repo{
		ID:            s.repoSeq,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		CreatedAt:     s.tick(),
		Objects:       map[string]*Object{},
		Refs:          map[string]string{},
		Labels:        map[string]*Label{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		packCache:     map[string][]byte{},
		planCache:     map[string]*fetchPlan{},
		advCache:      map[string][]byte{},
	}
	s.repos[r.FullName()] = r
	return r
}

func (s *Store) repo(owner, name string) *Repo {
	return s.repos[owner+"/"+name]
}

func (s *Store) viewerRepos() []*Repo {
	var out []*Repo
	for _, r := range s.repos {
		if r.Owner == viewerLogin {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) nextIssueGID() int64  { s.issueSeq++; return s.issueSeq }
func (s *Store) nextCommentID() int64 { s.commentSeq++; return s.commentSeq }

// authorLine is the git identity the forge commits under (the viewer),
// stamped with the logical clock.
func (s *Store) authorLine() string {
	return fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, s.tick())
}

// ---------------------------------------------------------------------------
// contents-API commits
// ---------------------------------------------------------------------------

// commitFile writes content at path on branch (creating the branch from the
// default branch's tip when it does not exist yet) and returns the new
// commit oid and the blob oid. It assumes the caller validated sha rules.
func (s *Store) commitFile(r *Repo, branch, path string, content []byte, message string) (string, string) {
	ref := "refs/heads/" + branch
	parent, exists := r.Refs[ref]
	if !exists {
		// a contents write naming a fresh branch creates it from the
		// default branch's current tip
		parent = r.Refs[r.headRef()]
	}
	files := map[string]string{}
	if parent != "" {
		if pc := r.Objects[parent]; pc != nil {
			flattenTree(r.Objects, parseCommit(pc.Data).Tree, "", files)
		}
	}
	blobOID := hashObject("blob", content)
	if r.Objects[blobOID] == nil {
		r.Objects[blobOID] = &Object{Type: "blob", Data: append([]byte(nil), content...)}
	}
	files[path] = blobOID
	treeOID := buildTreeFromFiles(r.Objects, files)
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	data := buildCommit(treeOID, parents, s.authorLine(), ensureNL(message))
	commitOID := hashObject("commit", data)
	if r.Objects[commitOID] == nil {
		r.Objects[commitOID] = &Object{Type: "commit", Data: data}
	}
	r.Refs[ref] = commitOID
	r.invalidateCaches()
	return commitOID, blobOID
}

// deleteFile removes path from branch with a commit; returns the commit oid.
func (s *Store) deleteFile(r *Repo, branch, path, message string) string {
	ref := "refs/heads/" + branch
	parent := r.Refs[ref]
	files := map[string]string{}
	if pc := r.Objects[parent]; pc != nil {
		flattenTree(r.Objects, parseCommit(pc.Data).Tree, "", files)
	}
	delete(files, path)
	treeOID := buildTreeFromFiles(r.Objects, files)
	data := buildCommit(treeOID, []string{parent}, s.authorLine(), ensureNL(message))
	commitOID := hashObject("commit", data)
	if r.Objects[commitOID] == nil {
		r.Objects[commitOID] = &Object{Type: "commit", Data: data}
	}
	r.Refs[ref] = commitOID
	r.invalidateCaches()
	return commitOID
}

func ensureNL(m string) string {
	if strings.HasSuffix(m, "\n") {
		return m
	}
	return m + "\n"
}

// fileAt returns (blobOID, content, true) for path at the given commit.
func fileAt(r *Repo, commitOID, path string) (string, []byte, bool) {
	obj := r.Objects[commitOID]
	if obj == nil || obj.Type != "commit" {
		return "", nil, false
	}
	files := map[string]string{}
	flattenTree(r.Objects, parseCommit(obj.Data).Tree, "", files)
	oid, ok := files[path]
	if !ok {
		return "", nil, false
	}
	blob := r.Objects[oid]
	if blob == nil {
		return "", nil, false
	}
	return oid, blob.Data, true
}

// treeFiles returns the flat path -> blob oid map at a commit (empty when
// the commit does not exist).
func treeFiles(r *Repo, commitOID string) map[string]string {
	files := map[string]string{}
	if obj := r.Objects[commitOID]; obj != nil && obj.Type == "commit" {
		flattenTree(r.Objects, parseCommit(obj.Data).Tree, "", files)
	}
	return files
}

// ---------------------------------------------------------------------------
// pull request diffs and squash merges
// ---------------------------------------------------------------------------

type FileChange struct {
	Path      string
	Status    string // added | modified | removed
	Additions int
	Deletions int
}

// diffTrees computes per-file changes between two commits' trees.
func diffTrees(r *Repo, baseCommit, headCommit string) []FileChange {
	baseFiles := treeFiles(r, baseCommit)
	headFiles := treeFiles(r, headCommit)
	paths := map[string]bool{}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	var out []FileChange
	for p := range paths {
		bOID, inBase := baseFiles[p]
		hOID, inHead := headFiles[p]
		if inBase && inHead && bOID == hOID {
			continue
		}
		fc := FileChange{Path: p}
		var bData, hData []byte
		if inBase {
			bData = r.Objects[bOID].Data
		}
		if inHead {
			hData = r.Objects[hOID].Data
		}
		switch {
		case !inBase:
			fc.Status = "added"
		case !inHead:
			fc.Status = "removed"
		default:
			fc.Status = "modified"
		}
		fc.Additions, fc.Deletions = lineDiff(bData, hData)
		out = append(out, fc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// lineDiff counts added/removed lines with a simple LCS over lines.
func lineDiff(a, b []byte) (add, del int) {
	aLines := splitLines(a)
	bLines := splitLines(b)
	// LCS dynamic program; inputs are small (suite files are ~2 KiB).
	n, m := len(aLines), len(bLines)
	if n*m > 4_000_000 {
		// degenerate fallback for very large files: count all lines
		return m, n
	}
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if aLines[i] == bLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	lcs := dp[0][0]
	return m - lcs, n - lcs
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.Split(string(b), "\n")
	if s[len(s)-1] == "" {
		s = s[:len(s)-1]
	}
	return s
}

// squashMerge performs the three-way per-file merge of head into base.
// Returns (treeOID, conflictPath, ok).
func squashTree(r *Repo, baseTip, headTip string) (string, bool) {
	mb := mergeBase(r.Objects, baseTip, headTip)
	baseFiles := treeFiles(r, baseTip)
	headFiles := treeFiles(r, headTip)
	mbFiles := treeFiles(r, mb)
	paths := map[string]bool{}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	for p := range mbFiles {
		paths[p] = true
	}
	merged := map[string]string{}
	for p := range paths {
		bOID, inB := baseFiles[p]
		hOID, inH := headFiles[p]
		mOID, inM := mbFiles[p]
		same := func(x string, inX bool, y string, inY bool) bool { return inX == inY && (!inX || x == y) }
		switch {
		case same(bOID, inB, hOID, inH):
			if inB {
				merged[p] = bOID
			}
		case same(mOID, inM, bOID, inB): // only head changed it
			if inH {
				merged[p] = hOID
			}
		case same(mOID, inM, hOID, inH): // only base changed it
			if inB {
				merged[p] = bOID
			}
		default:
			return "", false
		}
	}
	return buildTreeFromFiles(r.Objects, merged), true
}

// mergePullSquash merges the pull's head into its base with one commit.
// Returns (mergeCommitOID, ok) — ok=false means a content conflict.
func (s *Store) mergePullSquash(r *Repo, p *Pull) (string, bool) {
	baseTip := r.tip(p.Base)
	headTip := r.tip(p.Head)
	if baseTip == "" || headTip == "" {
		return "", false
	}
	treeOID, ok := squashTree(r, baseTip, headTip)
	if !ok {
		return "", false
	}
	data := buildCommit(treeOID, []string{baseTip}, s.authorLine(), ensureNL(p.Title))
	commitOID := hashObject("commit", data)
	if r.Objects[commitOID] == nil {
		r.Objects[commitOID] = &Object{Type: "commit", Data: data}
	}
	r.Refs["refs/heads/"+p.Base] = commitOID
	r.invalidateCaches()
	return commitOID, true
}

// pullMergeable reports "MERGEABLE" or "CONFLICTING" for an open pull.
func (s *Store) pullMergeable(r *Repo, p *Pull) string {
	baseTip := r.tip(p.Base)
	headTip := r.tip(p.Head)
	if baseTip == "" || headTip == "" {
		return "UNKNOWN"
	}
	if _, ok := squashTree(r, baseTip, headTip); ok {
		return "MERGEABLE"
	}
	return "CONFLICTING"
}

// pullFiles lists the pull's changed files against the merge base.
func (s *Store) pullFiles(r *Repo, p *Pull) []FileChange {
	baseTip := r.tip(p.Base)
	headTip := r.tip(p.Head)
	mb := mergeBase(r.Objects, baseTip, headTip)
	if mb == "" {
		mb = baseTip
	}
	return diffTrees(r, mb, headTip)
}
