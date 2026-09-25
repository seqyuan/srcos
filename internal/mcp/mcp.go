// Package mcp is the MCP (Model Context Protocol) front-end of the platform
// surface (ADR-019).
//
// Why this exists: the platform's selling point is "探索用 AI，执行用 SRCOS" —
// an agent explores, and the deterministic execution is SRCOS's. An agent can
// only explore what it can ask about, and the protocol it asks with is MCP. So
// the gateway itself speaks it: one HTTP endpoint, no extra process, no second
// deployment to keep in sync (ADR-019's "复用单二进制").
//
// Three decisions shape this package:
//
//   - **Two halves, one authority.** The read tools answer from package inspect
//     and are always offered. The write tools (submit / cancel / run_flow)
//     answer from package execute and are offered only to a token carrying the
//     submit scope. Both packages are shared with the REST API, so the two
//     front-ends cannot disagree about a decision (ADR-018: 一份实现，多个前端).
//   - **Authorization is two-dimensional.** The submit scope says a program may
//     drive execution; the token's optional submit allowlist narrows it per
//     tool; the owner's Grant policy remains the outer bound. Nothing here is a
//     security boundary of its own — it all funnels into execute.
//   - **Authenticated by agent token.** A browser session is not accepted: MCP
//     is the surface for programs, and a program's credential is a token
//     (ADR-019). Each call is audited with the token's identity.
//
// Transport: Streamable HTTP, one POST per JSON-RPC message, answered with
// application/json. The server is stateless — it issues no Mcp-Session-Id —
// which is what makes "the gateway is a single binary you can restart" true
// for agents as well as for browsers.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/execute"
	"github.com/seqyuan/srcos/internal/inspect"
)

const (
	// Endpoint is the path the gateway reserves for this server. It is listed
	// in the README's reserved-path table, like every other gateway route.
	Endpoint = "/mcp"

	// ProtocolVersion is the MCP revision this server speaks.
	ProtocolVersion = "2025-06-18"

	// maxRequestBytes bounds a request body. A tool call is a handful of
	// fields; anything larger is a mistake or an attack, and either way the
	// answer is the same.
	maxRequestBytes = 1 << 20
)

// supportedVersions are the revisions accepted in `initialize`. An unknown
// version is not an error: the spec has the server answer with the version it
// does speak, and the client decides whether it can work with that.
var supportedVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Server answers MCP requests for one deployment.
type Server struct {
	// Version is reported as serverInfo.version (the gateway build).
	Version string
	// Reader is the read-only view every tool answers from.
	Reader *inspect.Reader
	// Tokens authenticates the bearer credential. It is required: /mcp exists
	// for programs, and a program's credential is an agent token (ADR-019).
	// It is an Authenticator rather than a single store because instance
	// credentials live in a different file from user-issued ones (A1).
	Tokens agenttoken.Authenticator
	// Exec is the write path the phase-2 tools call. Nil means this deployment
	// cannot execute, and the write tools are then not offered at all.
	Exec *execute.Controller

	tools  []tool
	byName map[string]tool
}

// NewServer builds the server and its tool registry.
func NewServer(version string, reader *inspect.Reader, tokens agenttoken.Authenticator, exec *execute.Controller) *Server {
	s := &Server{Version: version, Reader: reader, Tokens: tokens, Exec: exec}
	s.tools = append(readTools(), writeTools()...)
	s.byName = make(map[string]tool, len(s.tools))
	for _, t := range s.tools {
		s.byName[t.name] = t
	}
	return s
}

// ─────────────────────────────────────────────────────────────────────────
// JSON-RPC
// ─────────────────────────────────────────────────────────────────────────

// request is one JSON-RPC 2.0 message. A missing id makes it a notification.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ServeHTTP implements the Streamable HTTP transport for one message.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// POST only. GET (a server-initiated SSE stream) and DELETE (session
	// termination) are optional in the spec, and this server is stateless, so
	// it says so instead of holding a stream open for nothing.
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, nil, codeInvalidRequest,
			"the SRCOS MCP endpoint is stateless Streamable HTTP: POST one JSON-RPC message per request")
		return
	}
	// The spec requires Origin validation; a cross-origin page must not be able
	// to drive an agent token the browser happens to hold.
	if !auth.SameOriginRequest(r) {
		writeError(w, http.StatusForbidden, nil, codeInvalidRequest, "cross-origin request rejected")
		return
	}

	identity, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if !identity.Has(agenttoken.ScopeRead) {
		writeError(w, http.StatusForbidden, nil, codeInvalidRequest,
			"this agent token cannot read: the read scope is required")
		return
	}

	// A limited read rather than http.MaxBytesReader: the latter may write a
	// 413 of its own, and this handler wants every answer — including the
	// refusal — to be one JSON-RPC message.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, nil, codeParseError, "could not read the request body: "+err.Error())
		return
	}
	if len(body) > maxRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, nil, codeInvalidRequest,
			fmt.Sprintf("request body larger than %d bytes", maxRequestBytes))
		return
	}

	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		// A batch is not a parse error, but this revision does not support it:
		// say which, so a client author is not sent hunting for a syntax bug.
		if len(strings.TrimSpace(string(body))) > 0 && strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			writeError(w, http.StatusBadRequest, nil, codeInvalidRequest,
				"JSON-RPC batching was removed in MCP 2025-06-18; send one message per request")
			return
		}
		writeError(w, http.StatusBadRequest, nil, codeParseError, "invalid JSON: "+err.Error())
		return
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		writeError(w, http.StatusBadRequest, req.ID, codeInvalidRequest, `jsonrpc must be "2.0"`)
		return
	}

	// A notification is answered with 202 and no body, whatever it says.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		if req.Method == "notifications/initialized" {
			log.Printf("[srcos] mcp %s initialized", identity.Describe())
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rerr := s.dispatch(r.Context(), identity, req.Method, req.Params)
	if rerr != nil {
		writeError(w, http.StatusOK, req.ID, rerr.Code, rerr.Message)
		return
	}
	writeResult(w, req.ID, result)
}

// dispatch runs one request method.
func (s *Server) dispatch(ctx context.Context, identity agenttoken.Identity, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.initialize(params), nil

	case "ping":
		// The spec's liveness check: an empty result.
		return map[string]any{}, nil

	case "tools/list":
		log.Printf("[srcos] mcp %s tools/list", identity.Describe())
		return s.listTools(identity), nil

	case "tools/call":
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params: " + err.Error()}
		}
		t, known := s.byName[call.Name]
		if !known {
			return nil, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
				"unknown tool %q (call tools/list for the catalogue)", call.Name)}
		}
		log.Printf("[srcos] mcp %s tools/call %s", identity.Describe(), t.name)

		// A write tool on a deployment without a write path, or on a token
		// without the submit scope, is refused here as well as inside execute:
		// the listing already hides it, and a hidden tool must not be reachable
		// by guessing its name.
		if t.write && (s.Exec == nil || !identity.Has(agenttoken.ScopeSubmit)) {
			return textResult("this tool needs an agent token with the \"submit\" scope"+
				" (and a gateway with the write path configured)", true), nil
		}

		// A tool *execution* failure is a result with isError, not a protocol
		// error: the agent asked a well-formed question and the platform
		// answered "no, because …". A malformed call is a protocol error
		// (handled above).
		out, err := t.call(ctx, env{user: identity.User, ident: identity, reader: s.Reader, exec: s.Exec}, call.Arguments)
		if err != nil {
			return textResult(err.Error(), true), nil
		}
		return textResult(render(out), false), nil

	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: fmt.Sprintf(
			"method %q is not implemented by this server (it exposes tools only; the second phase adds submit)", method)}
	}
}

// initialize negotiates the protocol version and declares the capabilities.
func (s *Server) initialize(params json.RawMessage) map[string]any {
	version := ProtocolVersion
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &init); err == nil && supportedVersions[init.ProtocolVersion] {
		version = init.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			// No listChanged: the catalogue is a directory scan, and SRCOS does
			// not push notifications on this transport.
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{"name": "srcos", "version": s.Version},
		"instructions": "SRCOS is a deterministic execution backend: tools are work.sh + a typed interface, " +
			"and an instance is one run of one tool. Read surface: list tools, browse the storage roots a " +
			"tool declares, inspect instances, tail logs, list artifacts and read text files. With the " +
			"\"submit\" scope you can also submit a run, cancel one, and run a flow. " +
			"Every answer is scoped to the user the agent token belongs to; a tool's interface comes from " +
			"srcos_describe_tool, and the storages a path parameter may use come from srcos_list_storages.",
	}
}

// ─────────────────────────────────────────────────────────────────────────
// authentication
// ─────────────────────────────────────────────────────────────────────────

// authenticate resolves the bearer token. On failure it writes the 401 itself.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (agenttoken.Identity, bool) {
	if s.Tokens == nil {
		// A deployment that never issued a token cannot serve agents; saying so
		// beats an empty answer that looks like a policy decision.
		writeError(w, http.StatusServiceUnavailable, nil, codeInternalError,
			"agent tokens are not configured on this gateway, so MCP is unavailable")
		return agenttoken.Identity{}, false
	}
	identity, err := s.Tokens.Authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="srcos"`)
		msg := err.Error()
		if errors.Is(err, agenttoken.ErrNoCredential) {
			msg = "MCP requires an agent token: Authorization: Bearer srcos_<id>.<secret>"
		}
		writeError(w, http.StatusUnauthorized, nil, codeInvalidRequest, msg)
		return agenttoken.Identity{}, false
	}
	return identity, true
}

// ─────────────────────────────────────────────────────────────────────────
// wire helpers
// ─────────────────────────────────────────────────────────────────────────

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		writeError(w, http.StatusOK, id, codeInternalError, "could not encode the result: "+err.Error())
		return
	}
	writeMessage(w, http.StatusOK, response{JSONRPC: "2.0", ID: id, Result: raw})
}

func writeError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	writeMessage(w, status, response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	})
}

func writeMessage(w http.ResponseWriter, status int, msg response) {
	body, err := json.Marshal(msg)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}
