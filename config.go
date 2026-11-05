// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Listen                   string `json:"listen"`
	StatusPath               string `json:"status_path"`
	DatabasePath             string `json:"database_path"`
	TrueNASURL               string `json:"truenas_url"`
	TrueNASAPITokenFile      string `json:"truenas_api_key_file"`
	TrueNASCertificateSHA256 string `json:"truenas_certificate_sha256"`
	MCPBearerFile            string `json:"mcp_bearer_file"`
	PollSeconds              int    `json:"poll_seconds"`
	RetentionDays            int    `json:"retention_days"`
}

func loadConfig(path string) (Config, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return Config{}, e
	}
	var c Config
	if e = json.Unmarshal(b, &c); e != nil {
		return c, e
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8765"
	}
	// Publishing a home-lab endpoint requires a separate authenticated tunnel.
	if !strings.HasPrefix(c.Listen, "127.0.0.1:") && !strings.HasPrefix(c.Listen, "[::1]:") {
		return c, errors.New("MCP listen address must be loopback-only")
	}
	if c.PollSeconds == 0 {
		c.PollSeconds = 60
	}
	if c.PollSeconds < 15 || c.PollSeconds > 3600 {
		return c, errors.New("poll_seconds must be 15..3600")
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 30
	}
	if c.RetentionDays < 1 || c.RetentionDays > 365 {
		return c, errors.New("retention_days must be 1..365")
	}
	if c.DatabasePath == "" || c.StatusPath == "" || c.MCPBearerFile == "" {
		return c, errors.New("database_path, status_path and mcp_bearer_file are required")
	}
	if c.TrueNASURL != "" {
		if !strings.HasPrefix(c.TrueNASURL, "wss://") {
			return c, errors.New("TrueNAS must use wss://")
		}
		if c.TrueNASAPITokenFile == "" {
			return c, errors.New("truenas_api_key_file required for TrueNAS")
		}
		pin := strings.ReplaceAll(strings.TrimSpace(c.TrueNASCertificateSHA256), ":", "")
		raw, e := hex.DecodeString(pin)
		if e != nil || len(raw) != sha256.Size {
			return c, errors.New("TrueNAS certificate SHA-256 pin must be 64 hexadecimal characters")
		}
		c.TrueNASCertificateSHA256 = strings.ToLower(pin)
	}
	return c, nil
}

// Credentials are never command-line arguments, environment values, logs,
// published artifacts or tool responses.
func readSecret(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must be a regular file", filepath.Base(path))
	}
	if info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("%s must be chmod 600 or stricter", filepath.Base(path))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(b))
	if len(value) < 16 {
		return "", fmt.Errorf("%s credential is too short", filepath.Base(path))
	}
	return value, nil
}
func equalToken(a, b string) bool {
	aSum, bSum := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aSum[:], bSum[:]) == 1 && len(a) == len(b)
}

type Current struct {
	Kind       string          `json:"kind"`
	SampledAt  time.Time       `json:"sampled_at"`
	ReceivedAt time.Time       `json:"received_at"`
	Data       json.RawMessage `json:"data"`
	Stale      bool            `json:"stale"`
}
type Metric struct {
	Node      string    `json:"node"`
	Name      string    `json:"name"`
	Value     float64   `json:"value"`
	Unit      string    `json:"unit"`
	Source    string    `json:"source"`
	SampledAt time.Time `json:"sampled_at"`
}
