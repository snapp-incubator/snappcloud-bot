package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/version"
)

// The bot as an MCP server (streamable HTTP, stateless, JSON responses), so
// other agents can use it as a tool. Same authentication, scoping, limits and
// cleaning as the REST API: the caller's bearer token is their identity.

const mcpDefaultProtocol = "2025-03-26"

var mcpProtocols = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

const askDescription = "Ask the cluster assistant a question: about workloads, networking, metrics, " +
	"alerts, or how to do something on the platform. It investigates with read-only access, scoped to " +
	"the namespaces your token can see, and returns a finished Markdown answer. It never changes " +
	"anything — apply what it suggests yourself. Name the cluster and namespace in the question, ask " +
	"one thing per call, and pass the previous exchange in `history` for a follow-up. Set `review` for " +
	"an answer you will act on unread: it is re-checked against its evidence first (slower). A call " +
	"can take several minutes."

const whoamiDescription = "Your identity and the clusters and namespaces your token can reach. " +
	"Call it first: these are the only targets `ask` can answer about."

var mcpTools = []map[string]any{
	{
		"name":        "ask",
		"description": askDescription,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{"type": "string", "description": "The question, naming its cluster and namespace."},
				"history":  map[string]any{"type": "string", "description": "The previous exchange, for a follow-up."},
				"review":   map[string]any{"type": "boolean", "description": "Re-check the answer against its evidence before returning."},
			},
			"required": []string{"question"},
		},
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
	},
	{
		"name":        "whoami",
		"description": whoamiDescription,
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
	},
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (h *Handler) mcp(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	if len(req.ID) == 0 { // a notification: acknowledged, never answered
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		pv := mcpDefaultProtocol
		if mcpProtocols[p.ProtocolVersion] {
			pv = p.ProtocolVersion
		}
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "snappcloud-bot", "version": version.String()},
			"instructions":    whoamiDescription,
		}, nil)
	case "ping":
		writeRPC(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		writeRPC(w, req.ID, map[string]any{"tools": mcpTools}, nil)
	case "tools/call":
		h.mcpCall(w, r, req)
	default:
		writeRPC(w, req.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

// mcpCall runs a tool. Caller failures (token, limits, a failed answer) come
// back as tool errors the calling agent can read and act on; only a malformed
// request is a protocol error.
func (h *Handler) mcpCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Question string `json:"question"`
			History  string `json:"history"`
			Review   bool   `json:"review"`
		} `json:"arguments"`
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPC(w, req.ID, nil, &rpcError{Code: -32602, Message: "invalid params"})
		return
	}
	reqID := newReqID()
	meta := map[string]any{"requestId": reqID}

	switch p.Name {
	case "ask":
		run := func() map[string]any {
			res := h.ask(r.Context(), bearer(r), queryRequest{
				Query: p.Arguments.Question, History: p.Arguments.History, Review: p.Arguments.Review,
			}, reqID)
			if res.status != 0 {
				return toolResult(res.err, true, meta)
			}
			return toolResult(res.answer, false, meta)
		}
		if acceptsSSE(r) {
			h.stream(w, r, req.ID, p.Meta.ProgressToken, run)
			return
		}
		writeRPC(w, req.ID, run(), nil)
	case "whoami":
		id, scope, status, msg := h.authenticate(r.Context(), bearer(r), h.log.With("req", reqID))
		if status != 0 {
			writeRPC(w, req.ID, toolResult(msg, true, meta), nil)
			return
		}
		b, _ := json.Marshal(whoamiResponse{User: id.User, Region: id.Region, Clusters: clusterMap(scope)})
		writeRPC(w, req.ID, toolResult(string(b), false, meta), nil)
	default:
		writeRPC(w, req.ID, nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name})
	}
}

// keepalive is how often a streamed call sends something while it works.
// HTTP clients abort a response whose headers or next bytes take too long
// (Node's default is five minutes), and an investigation can take longer.
var keepalive = 15 * time.Second

func acceptsSSE(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// stream answers a slow call as server-sent events: headers at once, a
// progress notification (or comment, without a progress token) every
// keepalive, then the result.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, id, progressToken json.RawMessage, run func() map[string]any) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeRPC(w, id, run(), nil)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	done := make(chan map[string]any, 1)
	go func() { done <- run() }()
	tick := time.NewTicker(keepalive)
	defer tick.Stop()
	start := time.Now()
	for {
		select {
		case res := <-done:
			writeEvent(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
			fl.Flush()
			return
		case <-tick.C:
			if len(progressToken) > 0 {
				secs := int(time.Since(start).Seconds())
				writeEvent(w, map[string]any{"jsonrpc": "2.0", "method": "notifications/progress",
					"params": map[string]any{"progressToken": progressToken, "progress": secs,
						"message": fmt.Sprintf("investigating (%ds)", secs)}})
			} else {
				_, _ = io.WriteString(w, ": keepalive\n\n")
			}
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeEvent(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
}

func toolResult(text string, isErr bool, meta map[string]any) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
		"_meta":   meta,
	}
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	writeJSON(w, http.StatusOK, resp)
}

// mcpMethodNotAllowed: no server-initiated stream and no sessions to delete.
func mcpMethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", http.MethodPost)
	w.WriteHeader(http.StatusMethodNotAllowed)
}
