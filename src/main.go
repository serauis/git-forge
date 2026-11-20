package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The forge: one process. It binds an ephemeral port, registers it with
// the suite ($MAKEFUN_ENV_URL/register), serves git smart HTTP + REST v3 +
// GraphQL + MCP from one in-memory store, long-polls $MAKEFUN_ENV_URL/done
// and exits 0 when the suite is over. Without MAKEFUN_ENV_URL it serves
// standalone (local development); FORGE_PORT pins the port.
// ---------------------------------------------------------------------------

type Server struct {
	store *Store
	token string
}

func readAllBody(req *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(req.Body, 1<<26))
}

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	// The trial pod gives the forge 2 CPUs (shared with the suite); Go 1.23
	// does not honour cgroup CPU limits, so pin the scheduler to the pod.
	maxProcs := 2
	if v := os.Getenv("FORGE_GOMAXPROCS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxProcs = n
		}
	}
	runtime.GOMAXPROCS(maxProcs)
srv := &Server{store: newStore(), token: token}

	port := 0
	if v := os.Getenv("FORGE_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stderr, "forge listening on port %d\n", actualPort)

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL != "" {
		go registerAndPollDone(envURL, actualPort)
	}
	if err := srv.serveListener(ln); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func registerAndPollDone(envURL string, port int) {
	client := &http.Client{Timeout: 30 * time.Second}
	// register (the first register wins; retry briefly at boot)
	for attempt := 0; attempt < 30; attempt++ {
		body, _ := json.Marshal(map[string]int{"port": port})
		resp, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				break
			}
			if resp.StatusCode == 409 {
				break // already registered
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	// long-poll /done until the suite is over
	for {
		resp, err := client.Get(envURL + "/done")
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		var state struct {
			Done bool `json:"done"`
		}
		json.NewDecoder(resp.Body).Decode(&state)
		resp.Body.Close()
		if state.Done {
			// give in-flight responses a breath, then exit 0
			time.Sleep(100 * time.Millisecond)
			os.Exit(0)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	if path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
		return
	}
	// authentication: every other surface needs the suite's bearer token
	if s.token != "" && req.Header.Get("Authorization") != "Bearer "+s.token {
		if strings.HasPrefix(path, "/api/") {
			s.writeErr(w, 401, "Unauthorized")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			w.Write([]byte(`{"message":"Unauthorized"}`))
		}
		return
	}
	switch {
	case path == "/mcp":
		s.handleMCP(w, req)
	case path == "/api/graphql":
		s.store.mu.Lock()
		s.handleGraphQL(w, req)
		s.store.mu.Unlock()
	case strings.HasPrefix(path, "/api/v3"):
		s.serveREST(w, req)
	case strings.Contains(path, ".git/"):
		s.serveGit(w, req)
	default:
		if strings.HasPrefix(path, "/api/") {
			s.writeErr(w, 404, "Not Found")
		} else {
			w.WriteHeader(404)
		}
	}
}

func (s *Server) serveGit(w http.ResponseWriter, req *http.Request) {
	owner, repoName, rest, ok := gitPath(req.URL.Path)
	if !ok {
		w.WriteHeader(404)
		return
	}
	switch {
	case rest == "/info/refs" && req.Method == http.MethodGet:
		s.handleInfoRefs(w, req, owner, repoName)
	case rest == "/git-upload-pack" && req.Method == http.MethodPost:
		s.handleUploadPack(w, req, owner, repoName)
	case rest == "/git-receive-pack" && req.Method == http.MethodPost:
		s.handleReceivePack(w, req, owner, repoName)
	default:
		w.WriteHeader(404)
	}
}

// serveREST dispatches /api/v3 routes. GETs run under a read lock,
// mutations under the write lock.
func (s *Server) serveREST(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
	} else {
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
	}
	rest := strings.TrimPrefix(req.URL.Path, "/api/v3")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		if req.Method == http.MethodGet {
			s.restRoot(w, req)
		} else {
			s.writeErr(w, 404, "Not Found")
		}
		return
	}
	segs := strings.Split(rest, "/")
	switch segs[0] {
	case "meta":
		if len(segs) == 1 && req.Method == http.MethodGet {
			s.restMeta(w, req)
			return
		}
	case "user":
		if len(segs) == 1 && req.Method == http.MethodGet {
			s.restUser(w, req)
			return
		}
		if len(segs) == 2 && segs[1] == "repos" {
			if req.Method == http.MethodGet {
				s.restListUserRepos(w, req)
				return
			}
			if req.Method == http.MethodPost {
				s.restCreateRepo(w, req)
				return
			}
		}
	case "repos":
		if len(segs) >= 3 {
			s.serveRepoRoutes(w, req, segs[1], segs[2], segs[3:])
			return
		}
	}
	s.writeErr(w, 404, "Not Found")
}

func (s *Server) serveRepoRoutes(w http.ResponseWriter, req *http.Request, owner, name string, tail []string) {
	r := s.store.repo(owner, name)
	if r == nil {
		s.writeErr(w, 404, "Repository not found")
		return
	}
	if len(tail) == 0 {
		switch req.Method {
		case http.MethodGet:
			s.restGetRepo(w, req, r)
		case http.MethodPatch:
			s.restPatchRepo(w, req, r)
		case http.MethodDelete:
			s.restDeleteRepo(w, req, r)
		default:
			s.writeErr(w, 404, "Not Found")
		}
		return
	}
	switch tail[0] {
	case "branches":
		if len(tail) == 1 && req.Method == http.MethodGet {
			s.restListBranches(w, req, r)
			return
		}
		if len(tail) >= 2 && req.Method == http.MethodGet {
			s.restGetBranch(w, req, r, strings.Join(tail[1:], "/"))
			return
		}
	case "commits":
		if len(tail) == 1 && req.Method == http.MethodGet {
			s.restListCommits(w, req, r)
			return
		}
	case "git":
		if len(tail) >= 2 && tail[1] == "refs" {
			if len(tail) == 2 {
				if req.Method == http.MethodPost {
					s.restCreateRef(w, req, r)
					return
				}
				if req.Method == http.MethodGet {
					s.restListRefs(w, req, r)
					return
				}
			}
			if len(tail) >= 3 && req.Method == http.MethodGet {
				ref := strings.Join(tail[2:], "/")
				if !strings.HasPrefix(ref, "refs/") {
					ref = "refs/" + ref
				}
				s.restGetRef(w, req, r, ref)
				return
			}
		}
	case "contents":
		path := strings.Join(tail[1:], "/")
		switch req.Method {
		case http.MethodGet:
			s.restGetContents(w, req, r, path)
			return
		case http.MethodPut:
			s.restPutContents(w, req, r, path)
			return
		case http.MethodDelete:
			s.restDeleteContents(w, req, r, path)
			return
		}
	case "labels":
		if len(tail) == 1 {
			if req.Method == http.MethodPost {
				s.restCreateLabel(w, req, r)
				return
			}
			if req.Method == http.MethodGet {
				s.restListLabels(w, req, r)
				return
			}
		}
	case "issues":
		if len(tail) == 1 {
			if req.Method == http.MethodPost {
				s.restCreateIssue(w, req, r)
				return
			}
			if req.Method == http.MethodGet {
				s.restListIssues(w, req, r)
				return
			}
		}
		if len(tail) >= 2 {
			number, err := strconv.Atoi(tail[1])
			if err != nil {
				s.writeErr(w, 404, "Not Found")
				return
			}
			if len(tail) == 2 {
				if req.Method == http.MethodGet {
					s.restGetIssue(w, req, r, number)
					return
				}
				if req.Method == http.MethodPatch {
					s.restPatchIssue(w, req, r, number)
					return
				}
			}
			if len(tail) == 3 && tail[2] == "comments" {
				if req.Method == http.MethodPost {
					s.restCreateComment(w, req, r, number)
					return
				}
				if req.Method == http.MethodGet {
					s.restListComments(w, req, r, number)
					return
				}
			}
		}
	case "pulls":
		if len(tail) == 1 {
			if req.Method == http.MethodPost {
				s.restCreatePull(w, req, r)
				return
			}
			if req.Method == http.MethodGet {
				s.restListPulls(w, req, r)
				return
			}
		}
		if len(tail) >= 2 {
			number, err := strconv.Atoi(tail[1])
			if err != nil {
				s.writeErr(w, 404, "Not Found")
				return
			}
			if len(tail) == 2 && req.Method == http.MethodGet {
				s.restGetPull(w, req, r, number)
				return
			}
			if len(tail) == 3 && tail[2] == "files" && req.Method == http.MethodGet {
				s.restPullFiles(w, req, r, number)
				return
			}
			if len(tail) == 3 && tail[2] == "merge" && req.Method == http.MethodPut {
				s.restMergePull(w, req, r, number)
				return
			}
		}
	}
	s.writeErr(w, 404, "Not Found")
}
