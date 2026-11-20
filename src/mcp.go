package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// MCP over Streamable HTTP: JSON-RPC 2.0, modern era (server/discover,
// revision 2026-07-28) with the legacy initialize handshake also answered.
// The tools act on the same store as REST and GraphQL.
// ---------------------------------------------------------------------------

type mcpTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

func mcpSchema(props map[string]interface{}, required ...string) map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

func strProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Description: "Create a new repository for the authenticated user.",
		InputSchema: mcpSchema(map[string]interface{}{
			"name":        strProp("The repository name."),
			"description": strProp("A short description of the repository."),
			"private":     map[string]interface{}{"type": "boolean", "description": "Whether the repository is private."},
		}, "name"),
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpSchema(map[string]interface{}{
			"owner": strProp("The repository owner login."),
			"repo":  strProp("The repository name."),
		}, "owner", "repo"),
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpSchema(map[string]interface{}{
			"owner": strProp("The repository owner login."),
			"repo":  strProp("The repository name."),
			"title": strProp("The issue title."),
			"body":  strProp("The issue body."),
		}, "owner", "repo", "title"),
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: mcpSchema(map[string]interface{}{
			"owner": strProp("The repository owner login."),
			"repo":  strProp("The repository name."),
			"state": strProp("Filter by state: open, closed or all."),
		}, "owner", "repo"),
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request from a head branch into a base branch.",
		InputSchema: mcpSchema(map[string]interface{}{
			"owner": strProp("The repository owner login."),
			"repo":  strProp("The repository name."),
			"title": strProp("The pull request title."),
			"body":  strProp("The pull request body."),
			"head":  strProp("The head branch name."),
			"base":  strProp("The base branch name."),
		}, "owner", "repo", "title", "head", "base"),
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request (squash).",
		InputSchema: mcpSchema(map[string]interface{}{
			"owner":       strProp("The repository owner login."),
			"repo":        strProp("The repository name."),
			"pull_number": map[string]interface{}{"type": "integer", "description": "The pull request number."},
		}, "owner", "repo", "pull_number"),
	},
}

// toolsListJSON is marshalled once: two tools/list answers must be
// byte-for-byte identical.
var toolsListJSON []byte

func init() {
	toolsListJSON, _ = json.Marshal(map[string]interface{}{"tools": mcpTools})
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) handleMCP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := readAllBody(req)
	if err != nil {
		s.mcpParseError(w)
		return
	}
	var rpc rpcRequest
	if err := json.Unmarshal(body, &rpc); err != nil || rpc.Method == "" {
		s.mcpParseError(w)
		return
	}
	// notifications expect no JSON-RPC answer
	if strings.HasPrefix(rpc.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	id := rpc.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	write := func(resp rpcResponse) {
		resp.JSONRPC = "2.0"
		resp.ID = id
		data, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}
	switch rpc.Method {
	case "server/discover":
		write(rpcResponse{Result: map[string]interface{}{
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":        map[string]string{"name": "forge", "version": "1.0.0"},
			"_meta": map[string]interface{}{
				"io.modelcontextprotocol/serverInfo": map[string]string{"name": "forge", "version": "1.0.0"},
			},
		}})
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(rpc.Params, &params)
		version := params.ProtocolVersion
		supported := map[string]bool{"2026-07-28": true, "2025-11-25": true, "2025-06-18": true, "2025-03-26": true, "2024-11-05": true}
		if !supported[version] {
			version = "2025-11-25"
		}
		write(rpcResponse{Result: map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]string{"name": "forge", "version": "1.0.0"},
		}})
	case "ping":
		write(rpcResponse{Result: map[string]interface{}{}})
	case "tools/list":
		w.Header().Set("Content-Type", "application/json")
		var raw struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
		}
		raw.JSONRPC = "2.0"
		raw.ID = id
		raw.Result = toolsListJSON
		data, _ := json.Marshal(raw)
		w.Write(data)
	case "tools/call":
		s.mcpToolsCall(w, rpc, write)
	default:
		write(rpcResponse{Error: &rpcError{Code: -32601, Message: "Method not found: " + rpc.Method}})
	}
}

func (s *Server) mcpParseError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`))
}

type toolResult struct {
	Content           []map[string]string `json:"content"`
	StructuredContent interface{}         `json:"structuredContent,omitempty"`
	IsError           bool                `json:"isError,omitempty"`
}

func toolOK(v interface{}) toolResult {
	data, _ := json.Marshal(v)
	return toolResult{
		Content:           []map[string]string{{"type": "text", "text": string(data)}},
		StructuredContent: v,
	}
}

func toolErr(format string, args ...interface{}) toolResult {
	return toolResult{
		Content: []map[string]string{{"type": "text", "text": fmt.Sprintf(format, args...)}},
		IsError: true,
	}
}

func (s *Server) mcpToolsCall(w http.ResponseWriter, rpc rpcRequest, write func(rpcResponse)) {
	var params struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(rpc.Params, &params); err != nil {
		write(rpcResponse{Error: &rpcError{Code: -32602, Message: "Invalid params"}})
		return
	}
	var tool *mcpTool
	for i := range mcpTools {
		if mcpTools[i].Name == params.Name {
			tool = &mcpTools[i]
			break
		}
	}
	if tool == nil {
		write(rpcResponse{Error: &rpcError{Code: -32602, Message: "Unknown tool: " + params.Name}})
		return
	}
	args := params.Arguments
	if args == nil {
		args = map[string]interface{}{}
	}
	// required-argument validation
	if required, ok := tool.InputSchema["required"].([]string); ok {
		for _, name := range required {
			if _, present := args[name]; !present {
				write(rpcResponse{Error: &rpcError{Code: -32602, Message: "Missing required argument: " + name}})
				return
			}
		}
	}
	s.store.lockWrite()
	defer s.store.mu.Unlock()
	write(rpcResponse{Result: s.runTool(tool.Name, args)})
}

func argString(args map[string]interface{}, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argNumber(args map[string]interface{}, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

func (s *Server) runTool(name string, args map[string]interface{}) toolResult {
	st := s.store
	switch name {
	case "create_repo":
		repoName := argString(args, "name")
		if st.repo(viewerLogin, repoName) != nil {
			return toolErr("repository %s already exists", repoName)
		}
		private := false
		if v, ok := args["private"].(bool); ok {
			private = v
		}
		r := st.createRepo(viewerLogin, repoName, argString(args, "description"), private)
		return toolOK(repoJSON(r))
	case "get_repo":
		r := st.repo(argString(args, "owner"), argString(args, "repo"))
		if r == nil {
			return toolErr("repository not found")
		}
		return toolOK(repoJSON(r))
	case "create_issue":
		r := st.repo(argString(args, "owner"), argString(args, "repo"))
		if r == nil {
			return toolErr("repository not found")
		}
		iss, errMsg := s.createIssue(r, argString(args, "title"), argString(args, "body"), nil)
		if errMsg != "" {
			return toolErr("%s", errMsg)
		}
		return toolOK(issueJSON(r, iss))
	case "list_issues":
		r := st.repo(argString(args, "owner"), argString(args, "repo"))
		if r == nil {
			return toolErr("repository not found")
		}
		state := argString(args, "state")
		if state == "" {
			state = "open"
		}
		out := []issueView{}
		numbers := make([]int, 0, len(r.Issues))
		for n := range r.Issues {
			numbers = append(numbers, n)
		}
		sortInts(numbers)
		for _, n := range numbers {
			iss := r.Issues[n]
			if state != "all" && iss.State != state {
				continue
			}
			out = append(out, issueJSON(r, iss))
		}
		return toolOK(out)
	case "create_pull_request":
		r := st.repo(argString(args, "owner"), argString(args, "repo"))
		if r == nil {
			return toolErr("repository not found")
		}
		p, errMsg, _ := s.createPull(r, argString(args, "title"), argString(args, "body"), argString(args, "head"), argString(args, "base"))
		if errMsg != "" {
			return toolErr("%s", errMsg)
		}
		return toolOK(pullJSON(r, p))
	case "merge_pull_request":
		r := st.repo(argString(args, "owner"), argString(args, "repo"))
		if r == nil {
			return toolErr("repository not found")
		}
		number, ok := argNumber(args, "pull_number")
		if !ok {
			return toolErr("pull_number is required")
		}
		p := r.Pulls[number]
		if p == nil {
			return toolErr("pull request not found")
		}
		if _, status, msg := s.mergePull(r, p); status != 200 {
			return toolErr("%s", msg)
		}
		return toolOK(pullJSON(r, p))
	}
	return toolErr("unknown tool")
}

func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
