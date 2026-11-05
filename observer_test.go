// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := openStore(filepath.Join(t.TempDir(), "state", "observer.db"), 30)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestStoreAndStatusChanges(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if e := s.Put("zbook", now, map[string]any{"cpu_percent": 2}, []Metric{{"zbook", "cpu_percent", 2, "percent", "test", now}}); e != nil {
		t.Fatal(e)
	}
	c, e := s.Current("zbook", time.Minute)
	if e != nil || c.Stale {
		t.Fatalf("current: %+v %v", c, e)
	}
	h, e := s.History("zbook", "cpu_percent", 24, 100)
	if e != nil || len(h) != 1 {
		t.Fatalf("history: %d %v", len(h), e)
	}
	_ = s.RecordChanges(map[string]string{"machine:storage": "operational"})
	_ = s.RecordChanges(map[string]string{"machine:storage": "powered_off"})
	changes, e := s.RecentChanges(30)
	if e != nil || len(changes) != 1 {
		t.Fatalf("changes: %v, %v", changes, e)
	}
	if changes[0]["current"] != "powered_off" {
		t.Fatalf("wrong status transition %v", changes[0])
	}
	s.RecordError("truenas_metrics", os.ErrNotExist)
	if len(s.Latest()["collection_errors"].(map[string]any)) != 1 {
		t.Fatal("error missing")
	}
}
func TestSecureConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	good := Config{Listen: "127.0.0.1:8765", StatusPath: "/tmp/status.json", DatabasePath: "/tmp/status.db", MCPBearerFile: "/tmp/bearer"}
	b, _ := json.Marshal(good)
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := loadConfig(path); e != nil {
		t.Fatal(e)
	}
	good.Listen = "0.0.0.0:8765"
	b, _ = json.Marshal(good)
	_ = os.WriteFile(path, b, 0600)
	if _, e := loadConfig(path); e == nil {
		t.Fatal("non-loopback MCP should be refused")
	}
	secret := filepath.Join(dir, "secret")
	_ = os.WriteFile(secret, []byte("very-long-secret-value\n"), 0644)
	if _, e := readSecret(secret); e == nil {
		t.Fatal("world-readable key should be refused")
	}
	_ = os.Chmod(secret, 0600)
	v, e := readSecret(secret)
	if e != nil || v != "very-long-secret-value" {
		t.Fatalf("secret read: %v", e)
	}
	if equalToken(v, v+"x") || !equalToken(v, v) {
		t.Fatal("constant-time token comparison failed")
	}
}
func TestMCPAuthenticationAndTools(t *testing.T) {
	s := testStore(t)
	secret := "7e5a66010841502018c7f219cc403bb4e235e71ebc761d4a21b42cef0d523d00"
	srv := httptest.NewServer(handler(s, secret))
	defer srv.Close()
	post := func(token, method, name, version string) (int, map[string]any) {
		t.Helper()
		params := map[string]any{}
		if name != "" {
			params["name"] = name
			params["arguments"] = map[string]any{}
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if version != "" {
			req.Header.Set("MCP-Protocol-Version", version)
		}
		if version == "2026-07-28" {
			req.Header.Set("Mcp-Method", method)
			if name != "" {
				req.Header.Set("Mcp-Name", name)
			}
		}
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if status, _ := post("", "tools/list", "", ""); status != 401 {
		t.Fatal("unauthenticated MCP allowed")
	}
	if status, _ := post("wrong-token", "tools/list", "", ""); status != 401 {
		t.Fatal("invalid bearer accepted")
	}
	status, result := post(secret, "tools/list", "", "2026-07-28")
	if status != 200 {
		t.Fatalf("modern MCP list: %v", result)
	}
	list := result["result"].(map[string]any)
	if list["resultType"] != "complete" || len(list["tools"].([]any)) != 6 {
		t.Fatalf("expected six read-only tools: %v", list)
	}
	status, result = post(secret, "tools/call", "get_overview", "2026-07-28")
	if status != 200 || result["result"].(map[string]any)["resultType"] != "complete" {
		t.Fatal(result)
	}
	status, result = post(secret, "initialize", "", "")
	if status != 200 || result["result"].(map[string]any)["protocolVersion"] != "2025-11-25" {
		t.Fatal(result)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Origin", "https://untrusted.example")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("Origin not rejected")
	}
}
func TestReportSelectsNonPlaceholderSample(t *testing.T) {
	ts := time.Now().Unix() - 5
	makeRow := func(t int64, values ...any) []json.RawMessage {
		out := []json.RawMessage{}
		for _, value := range append([]any{t}, values...) {
			b, _ := json.Marshal(value)
			out = append(out, b)
		}
		return out
	}
	g := reportGraph{Name: "cpu", Legend: []string{"time", "cpu", "cpu0"}, Data: [][]json.RawMessage{
		makeRow(ts, 3, 2), makeRow(ts+1, 0, 0), makeRow(ts+2, 0, 0),
	}}
	when, values, ok := latestReportPoint(g)
	if !ok || when.Unix() != ts || values["cpu"] != 3 {
		t.Fatalf("invalid reporting point %v %v %v", when, values, ok)
	}
}
func TestCertificatePinSanity(t *testing.T) {
	fingerprint := sha256.Sum256([]byte("data"))
	pin := hex.EncodeToString(fingerprint[:])
	if len(pin) != 64 || !equalDigest(fingerprint[:], fingerprint[:]) {
		t.Fatal("hash comparison failed")
	}
	changed := sha256.Sum256([]byte("other"))
	if equalDigest(fingerprint[:], changed[:]) {
		t.Fatal("wrong certificate trusted")
	}
}
