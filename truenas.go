// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"net/http"
	"strings"
	"time"
)

type truenasClient struct {
	conn   *websocket.Conn
	nextID int
}
type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (t *truenasClient) call(ctx context.Context, method string, params any, out any) error {
	t.nextID++
	id := t.nextID
	if err := wsjson.Write(ctx, t.conn, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return fmt.Errorf("send %s: %w", method, err)
	}
	for {
		var resp rpcResponse
		if err := wsjson.Read(ctx, t.conn, &resp); err != nil {
			return fmt.Errorf("receive %s: %w", method, err)
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: JSON-RPC error %d", method, resp.Error.Code)
		}
		if len(resp.Result) == 0 {
			return fmt.Errorf("%s returned no result", method)
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("%s invalid response: %w", method, err)
		}
		return nil
	}
}
func dialTrueNAS(ctx context.Context, c Config) (*truenasClient, error) {
	key, err := readSecret(c.TrueNASAPITokenFile)
	if err != nil {
		return nil, fmt.Errorf("TrueNAS key not available: %w", err)
	}
	expected, err := hex.DecodeString(c.TrueNASCertificateSHA256)
	if err != nil {
		return nil, errors.New("invalid TrueNAS TLS pin")
	}
	// The stock TrueNAS certificate is self-signed with CN=localhost. Authenticate
	// it using a *mandatory* exact leaf-certificate pin, including expiry checks,
	// instead of allowing a TLS connection without any identity verification.
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) != 1 && len(cs.PeerCertificates) < 1 {
				return errors.New("TrueNAS TLS certificate missing")
			}
			cert := cs.PeerCertificates[0]
			digest := sha256.Sum256(cert.Raw)
			if len(expected) != len(digest) || !equalDigest(expected, digest[:]) {
				return errors.New("TrueNAS TLS certificate fingerprint mismatch")
			}
			now := time.Now()
			if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
				return errors.New("TrueNAS TLS certificate expired or not yet valid")
			}
			return nil
		},
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: nil}, Timeout: 18 * time.Second}
	conn, _, err := websocket.Dial(ctx, c.TrueNASURL, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, fmt.Errorf("cannot open pinned TrueNAS WebSocket: %w", err)
	}
	conn.SetReadLimit(8 << 20)
	tc := &truenasClient{conn: conn}
	var authenticated bool
	ctxAuth, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err = tc.call(ctxAuth, "auth.login_with_api_key", []any{key}, &authenticated); err != nil {
		conn.CloseNow()
		return nil, err
	}
	if !authenticated {
		conn.CloseNow()
		return nil, errors.New("TrueNAS API key authentication rejected")
	}
	return tc, nil
}
func equalDigest(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
func (t *truenasClient) close() { _ = t.conn.Close(websocket.StatusNormalClosure, "done") }

type reportGraph struct {
	Name   string              `json:"name"`
	Legend []string            `json:"legend"`
	Data   [][]json.RawMessage `json:"data"`
}

func latestReportPoint(g reportGraph) (time.Time, map[string]float64, bool) {
	// Future-filled zero rows appear at the end of the TrueNAS reporting data.
	// Select a timestamped sample with meaningful data, never take the last row
	// blindly; missing fields and trailing placeholders are not zero readings.
	for i := len(g.Data) - 1; i >= 0; i-- {
		row := g.Data[i]
		if len(row) < 2 {
			continue
		}
		var ts int64
		if json.Unmarshal(row[0], &ts) != nil || ts <= 0 || ts > time.Now().Add(30*time.Second).Unix() {
			continue
		}
		values := map[string]float64{}
		meaningful := false
		for j := 1; j < len(row) && j < len(g.Legend); j++ {
			var f *float64
			if json.Unmarshal(row[j], &f) != nil || f == nil {
				continue
			}
			values[g.Legend[j]] = *f
			if *f != 0 {
				meaningful = true
			}
		}
		if meaningful && time.Since(time.Unix(ts, 0)) < 4*time.Minute {
			return time.Unix(ts, 0).UTC(), values, true
		}
	}
	return time.Time{}, nil, false
}

func collectTrueNAS(ctx context.Context, c Config, s *Store) {
	if c.TrueNASURL == "" {
		return
	}
	tc, err := dialTrueNAS(ctx, c)
	if err != nil {
		for _, kind := range []string{"truenas_metrics", "truenas_pool", "truenas_apps", "truenas_alerts"} {
			s.RecordError(kind, err)
		}
		return
	}
	defer tc.close()
	call := func(method string, params any, out any) error {
		req, cancel := context.WithTimeout(ctx, 13*time.Second)
		defer cancel()
		return tc.call(req, method, params, out)
	}
	var graphs []reportGraph
	err = call("reporting.get_data", []any{[]map[string]string{{"name": "cpu"}, {"name": "memory"}, {"name": "arcsize"}}, map[string]any{"unit": "HOUR", "page": 1}}, &graphs)
	if err != nil {
		s.RecordError("truenas_metrics", err)
	} else {
		metrics := []Metric{}
		summary := map[string]any{}
		sampled := time.Time{}
		for _, g := range graphs {
			at, vals, ok := latestReportPoint(g)
			if !ok {
				continue
			}
			summary[g.Name] = map[string]any{"sampled_at": at, "values": vals}
			if sampled.IsZero() || at.Before(sampled) {
				sampled = at
			}
			switch g.Name {
			case "cpu":
				if v, ok := vals["cpu"]; ok {
					metrics = append(metrics, Metric{"truenas", "cpu_percent", v, "percent", "TrueNAS reporting", at})
				}
			case "memory":
				if v, ok := vals["available"]; ok {
					metrics = append(metrics, Metric{"truenas", "memory_available_bytes", v, "bytes", "TrueNAS reporting", at})
				}
			case "arcsize":
				if v, ok := vals["size"]; ok {
					metrics = append(metrics, Metric{"truenas", "arc_bytes", v, "bytes", "TrueNAS reporting", at})
				}
			}
		}
		if len(metrics) == 0 {
			s.RecordError("truenas_metrics", errors.New("TrueNAS reporting returned no fresh metrics"))
		} else if e := s.Put("truenas_metrics", sampled, summary, metrics); e != nil {
			s.RecordError("truenas_metrics", e)
		}
	}
	var pools []struct {
		Name      string `json:"name"`
		Status    string `json:"status"`
		Healthy   bool   `json:"healthy"`
		Warning   bool   `json:"warning"`
		Size      int64  `json:"size"`
		Allocated int64  `json:"allocated"`
		Free      int64  `json:"free"`
		Scan      struct {
			Function string `json:"function"`
			State    string `json:"state"`
			Errors   int    `json:"errors"`
		} `json:"scan"`
	}
	err = call("pool.query", []any{}, &pools)
	if err != nil {
		s.RecordError("truenas_pool", err)
	} else {
		now := time.Now().UTC()
		entries := make([]map[string]any, 0, len(pools))
		metrics := []Metric{}
		changes := map[string]string{}
		for _, p := range pools {
			entries = append(entries, map[string]any{"name": p.Name, "status": p.Status, "healthy": p.Healthy, "warning": p.Warning, "size_bytes": p.Size, "allocated_bytes": p.Allocated, "free_bytes": p.Free, "last_scan": p.Scan})
			metrics = append(metrics, Metric{"truenas", "pool_" + p.Name + "_free_bytes", float64(p.Free), "bytes", "TrueNAS pool.query", now})
			changes["pool:"+p.Name] = p.Status
		}
		if e := s.Put("truenas_pool", now, entries, metrics); e != nil {
			s.RecordError("truenas_pool", e)
		} else {
			_ = s.RecordChanges(changes)
		}
	}
	var apps []struct {
		Name                  string  `json:"name"`
		State                 string  `json:"state"`
		HumanVersion          string  `json:"human_version"`
		Version               string  `json:"version"`
		LatestVersion         *string `json:"latest_version"`
		UpgradeAvailable      bool    `json:"upgrade_available"`
		ImageUpdatesAvailable bool    `json:"image_updates_available"`
	}
	err = call("app.query", []any{}, &apps)
	if err != nil {
		s.RecordError("truenas_apps", err)
	} else {
		now := time.Now().UTC()
		entries := make([]map[string]any, 0, len(apps))
		changes := map[string]string{}
		for _, a := range apps {
			entries = append(entries, map[string]any{
				"name": a.Name, "state": a.State,
				"human_version": a.HumanVersion, "chart_version": a.Version,
				"latest_chart_version":    a.LatestVersion,
				"upgrade_available":       a.UpgradeAvailable,
				"image_updates_available": a.ImageUpdatesAvailable,
			})
			changes["app:"+a.Name] = a.State
		}
		if e := s.Put("truenas_apps", now, entries, nil); e != nil {
			s.RecordError("truenas_apps", e)
		} else {
			_ = s.RecordChanges(changes)
		}
	}
	var alerts []struct {
		Level     string `json:"level"`
		Source    string `json:"source"`
		Formatted string `json:"formatted"`
		DateTime  struct {
			Date int64 `json:"$date"`
		} `json:"datetime"`
	}
	err = call("alert.list", []any{}, &alerts)
	if err != nil {
		s.RecordError("truenas_alerts", err)
	} else {
		now := time.Now().UTC()
		entries := make([]map[string]any, 0, len(alerts))
		for _, a := range alerts {
			at := time.UnixMilli(a.DateTime.Date).UTC()
			desc := strings.TrimSpace(a.Formatted)
			if len(desc) > 300 {
				desc = desc[:300]
			}
			entries = append(entries, map[string]any{
				"level": a.Level, "source": a.Source, "message": desc, "raised_at": at,
				// TrueNAS may preserve old un-dismissed CRITICAL alerts even when the
				// underlying pool has recovered; never assert this is a live fault.
				"historical":         now.Sub(at) > 24*time.Hour,
				"currently_verified": false,
			})
		}
		if e := s.Put("truenas_alerts", now, entries, nil); e != nil {
			s.RecordError("truenas_alerts", e)
		}
	}
}
