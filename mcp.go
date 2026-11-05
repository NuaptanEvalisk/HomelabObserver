// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}
func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}
func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
func toolList() []map[string]any {
	empty := objectSchema(map[string]any{})
	return []map[string]any{
		{"name": "get_overview", "title": "Homelab Overview", "description": "Read-only summary of monitored hosts, services, TrueNAS pools and collection health. Includes timestamps and stale flags; never treats historical TrueNAS alerts as current faults.", "inputSchema": empty},
		{"name": "list_nodes", "title": "List Homelab Nodes", "description": "Return monitored nodes and their reachability, distinguishing intentionally powered-off machines from failures.", "inputSchema": empty},
		{"name": "get_node_details", "title": "Node Details", "description": "Get latest read-only status and metrics for one node. For status-page nodes use the name returned by list_nodes; special IDs are zbook and truenas.", "inputSchema": objectSchema(map[string]any{"node": map[string]any{"type": "string", "minLength": 1}}, "node")},
		{"name": "get_alerts", "title": "Alerts", "description": "Return source-reported alerts with timestamps, staleness and historical flags. Old stored CRITICAL alerts do not prove present problems.", "inputSchema": empty},
		{"name": "get_recent_changes", "title": "Recent Changes", "description": "List timestamped transitions in node, service, ZFS pool and app status.", "inputSchema": objectSchema(map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}})},
		{"name": "get_metric_history", "title": "Metric History", "description": "Read recent time-series observations. Allowed node IDs: zbook, truenas. Metrics include cpu_percent, memory_available_bytes, arc_bytes, root_disk_available_bytes, var_disk_available_bytes and pool_<pool>_free_bytes.", "inputSchema": objectSchema(map[string]any{"node": map[string]any{"type": "string", "enum": []string{"zbook", "truenas"}}, "metric": map[string]any{"type": "string", "minLength": 1}, "hours": map[string]any{"type": "integer", "minimum": 1, "maximum": 168}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}, "node", "metric")},
	}
}
func jsonData(c *Current) any {
	if c == nil {
		return map[string]any{"stale": true, "available": false}
	}
	return c
}
func readCurrent(s *Store, kind string, age time.Duration) *Current {
	c, _ := s.Current(kind, age)
	return c
}
func detailForName(s *Store, name string) (map[string]any, error) {
	out := map[string]any{"node": name, "queried_at": time.Now().UTC()}
	switch name {
	case "zbook":
		out["metrics"] = jsonData(readCurrent(s, "zbook", 4*time.Minute))
		out["status"] = jsonData(readCurrent(s, "status", 2*time.Minute))
	case "truenas":
		for _, kind := range []string{"truenas_metrics", "truenas_pool", "truenas_apps", "truenas_alerts"} {
			age := 4 * time.Minute
			if kind == "truenas_pool" || kind == "truenas_apps" {
				age = 5 * time.Minute
			}
			out[kind] = jsonData(readCurrent(s, kind, age))
		}
	default:
		snap := readCurrent(s, "status", 2*time.Minute)
		if snap == nil {
			return nil, errors.New("no status snapshot available")
		}
		var status statusSnapshot
		if err := json.Unmarshal(snap.Data, &status); err != nil {
			return nil, err
		}
		for _, entry := range append(status.Machines, status.Services...) {
			if entry.Name == name {
				out["status"] = entry.Status
				out["detail"] = entry.Detail
				out["expected_off"] = entry.Status == "powered_off"
				out["sampled_at"] = snap.SampledAt
				out["stale"] = snap.Stale
				return out, nil
			}
		}
		return nil, fmt.Errorf("unknown node or service: %q", name)
	}
	return out, nil
}
func listNodes(s *Store) map[string]any {
	snap := readCurrent(s, "status", 2*time.Minute)
	nodes := make([]map[string]any, 0)
	if snap != nil {
		var status statusSnapshot
		if json.Unmarshal(snap.Data, &status) == nil {
			for _, item := range status.Machines {
				nodes = append(nodes, map[string]any{"node": item.Name, "status": item.Status, "detail": item.Detail, "expected_off": item.Status == "powered_off", "sampled_at": snap.SampledAt, "stale": snap.Stale})
			}
		}
	}
	return map[string]any{"nodes": nodes, "special_nodes": []string{"zbook", "truenas"}, "source": "ZBook existing status snapshot", "stale": snap == nil || snap.Stale}
}
func (s *Store) callTool(name string, args json.RawMessage) (any, error) {
	var options map[string]json.RawMessage
	if len(args) != 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &options); err != nil {
			return nil, errors.New("invalid arguments object")
		}
	}
	getText := func(k string) string { var v string; _ = json.Unmarshal(options[k], &v); return v }
	getInt := func(k string, def int) int {
		var v int
		if json.Unmarshal(options[k], &v) != nil {
			return def
		}
		return v
	}
	switch name {
	case "get_overview":
		return map[string]any{"queried_at": time.Now().UTC(), "sources": s.Latest(), "reachability": listNodes(s), "note": "Data is periodic, not live. Interpret stale=true as unknown, not zero; powered_off means intentionally offline."}, nil
	case "list_nodes":
		return listNodes(s), nil
	case "get_node_details":
		node := getText("node")
		if node == "" {
			return nil, errors.New("node is required")
		}
		return detailForName(s, node)
	case "get_alerts":
		return map[string]any{"truenas": jsonData(readCurrent(s, "truenas_alerts", 4*time.Minute)), "note": "TrueNAS may retain historic CRITICAL alerts. Correlate with latest pool and app status before calling an alert active."}, nil
	case "get_recent_changes":
		limit := getInt("limit", 30)
		if limit < 1 || limit > 100 {
			return nil, errors.New("limit must be 1..100")
		}
		changes, err := s.RecentChanges(limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"changes": changes, "queried_at": time.Now().UTC()}, nil
	case "get_metric_history":
		node, metric := getText("node"), getText("metric")
		if node != "zbook" && node != "truenas" {
			return nil, errors.New("node must be zbook or truenas")
		}
		allowed := map[string]bool{"cpu_percent": true, "memory_available_bytes": true, "arc_bytes": true, "root_disk_available_bytes": true, "var_disk_available_bytes": true}
		// Storage pools are named by TrueNAS; prevent arbitrary unbounded names.
		if !allowed[metric] && !(node == "truenas" && strings.HasPrefix(metric, "pool_") && strings.HasSuffix(metric, "_free_bytes") && len(metric) < 80) {
			return nil, errors.New("unsupported metric")
		}
		hours, limit := getInt("hours", 24), getInt("limit", 500)
		if hours < 1 || hours > 168 || limit < 1 || limit > 1000 {
			return nil, errors.New("hours or limit out of range")
		}
		data, err := s.History(node, metric, hours, limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"node": node, "metric": metric, "points": data, "queried_at": time.Now().UTC(), "note": "No interpolation of missing data"}, nil
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}
func handler(s *Store, authToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "cross-origin requests forbidden", http.StatusForbidden)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") ||
			!equalToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), authToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="HomelabObserver"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
		if err != nil {
			http.Error(w, "request body too large or unreadable", 400)
			return
		}
		var req mcpRequest
		if err = json.Unmarshal(b, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
			writeRPC(w, rpcError(nil, -32600, "invalid JSON-RPC request"))
			return
		}
		version := r.Header.Get("MCP-Protocol-Version")
		modern := version == "2026-07-28"
		if modern {
			if r.Header.Get("Mcp-Method") != req.Method {
				writeRPC(w, rpcError(req.ID, -32600, "Mcp-Method header mismatch"))
				return
			}
		}
		if len(req.ID) == 0 {
			if strings.HasPrefix(req.Method, "notifications/") {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeRPC(w, rpcError(nil, -32600, "missing request id"))
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			advertised := "2025-11-25"
			if modern {
				advertised = "2026-07-28"
			}
			result = map[string]any{"protocolVersion": advertised,
				"capabilities": map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":   map[string]any{"name": "HomelabObserver", "version": "0.1.0"}}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			result = map[string]any{"tools": toolList()}
			if modern {
				result.(map[string]any)["resultType"] = "complete"
			}
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(req.Params, &params) != nil || params.Name == "" {
				writeRPC(w, rpcError(req.ID, -32602, "missing tool name"))
				return
			}
			if modern && r.Header.Get("Mcp-Name") != params.Name {
				writeRPC(w, rpcError(req.ID, -32600, "Mcp-Name header mismatch"))
				return
			}
			value, callErr := s.callTool(params.Name, params.Arguments)
			if callErr != nil {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": callErr.Error()}}}
			} else {
				text, _ := json.Marshal(value)
				result = map[string]any{"isError": false,
					"content":           []any{map[string]any{"type": "text", "text": string(text)}},
					"structuredContent": value}
			}
			if modern {
				result.(map[string]any)["resultType"] = "complete"
			}
		default:
			writeRPC(w, rpcError(req.ID, -32601, "method not found"))
			return
		}
		writeRPC(w, rpcResult(req.ID, result))
	})
	return mux
}
func writeRPC(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(v)
}
func parseInt(s string, def int) int {
	v, e := strconv.Atoi(s)
	if e != nil {
		return def
	}
	return v
}
