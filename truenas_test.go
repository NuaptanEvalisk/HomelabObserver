// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise real TLS pinning, the 25.10 login method, JSON-RPC framing and
// data extraction without requiring a user's TrueNAS credentials in CI.
func TestMockTrueNASWebSocketCollector(t *testing.T) {
	key := "mock-test-api-key-that-must-stay-local"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/current" {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for {
			var req struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err = wsjson.Read(r.Context(), conn, &req); err != nil {
				return
			}
			var result any
			switch req.Method {
			case "auth.login_with_api_key":
				var params []string
				_ = json.Unmarshal(req.Params, &params)
				result = len(params) == 1 && params[0] == key
			case "reporting.get_data":
				ts := time.Now().Unix() - 5
				result = []map[string]any{
					{"name": "cpu", "legend": []string{"time", "cpu", "cpu0"}, "data": []any{[]any{ts, 3.5, 4}, []any{ts + 1, 0, 0}}},
					{"name": "memory", "legend": []string{"time", "available"}, "data": []any{[]any{ts, float64(2 << 30)}, []any{ts + 1, 0}}},
					{"name": "arcsize", "legend": []string{"time", "size"}, "data": []any{[]any{ts, float64(4 << 30)}, []any{ts + 1, 0}}},
				}
			case "pool.query":
				result = []map[string]any{{"name": "pool0", "status": "ONLINE", "healthy": true, "size": 1000, "free": 400, "allocated": 600}}
			case "app.query":
				result = []map[string]any{{"name": "media-app", "state": "RUNNING",
					"human_version": "3.0.1", "version": "1.0.3", "latest_version": "1.1.0",
					"upgrade_available": true, "image_updates_available": true}}
			case "alert.list":
				result = []map[string]any{{"level": "CRITICAL", "source": "VolumeStatus", "formatted": "old stored alert",
					"datetime": map[string]int64{"$date": time.Now().Add(-40 * 24 * time.Hour).UnixMilli()}}}
			default:
				_ = wsjson.Write(r.Context(), conn, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
				continue
			}
			if err = wsjson.Write(r.Context(), conn, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	cert := server.Certificate()
	digest := sha256.Sum256(cert.Raw)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{TrueNASURL: "wss" + strings.TrimPrefix(server.URL, "https") + "/api/current",
		TrueNASAPITokenFile: keyFile, TrueNASCertificateSHA256: hex.EncodeToString(digest[:])}
	s := testStore(t)
	collectTrueNAS(context.Background(), cfg, s)
	for _, kind := range []string{"truenas_pool", "truenas_metrics", "truenas_apps", "truenas_alerts"} {
		current, err := s.Current(kind, 2*time.Minute)
		if err != nil || current.Stale {
			t.Errorf("%s missing or stale: %+v %v", kind, current, err)
		}
	}
	appState, err := s.Current("truenas_apps", 2*time.Minute)
	if err != nil || !strings.Contains(string(appState.Data), `"human_version":"3.0.1"`) ||
		!strings.Contains(string(appState.Data), `"upgrade_available":true`) {
		t.Fatal("app version or upgrade availability missing")
	}
	points, err := s.History("truenas", "cpu_percent", 1, 100)
	if err != nil || len(points) != 1 {
		t.Fatalf("missing CPU data: %v %v", points, err)
	}
	alert, _ := s.Current("truenas_alerts", time.Minute)
	if alert == nil || !strings.Contains(string(alert.Data), `"historical":true`) {
		t.Fatal("old alert was incorrectly reported as current")
	}
	bad := cfg
	bad.TrueNASCertificateSHA256 = strings.Repeat("0", 64)
	_, err = dialTrueNAS(context.Background(), bad)
	if err == nil {
		t.Fatal("incorrect TLS pin was accepted")
	}
	// Unauthorized should not create fresh samples or leak credentials.
	bad = cfg
	_ = os.WriteFile(keyFile, []byte("wrong-api-token-long-enough\n"), 0600)
	_, err = dialTrueNAS(context.Background(), bad)
	if err == nil {
		t.Fatal("incorrect TrueNAS API token accepted")
	}
	if strings.Contains(err.Error(), "wrong-api-token") {
		t.Fatal("credential leaked via error")
	}
}
