// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Store struct {
	db            *sql.DB
	retentionDays int
}

func openStore(path string, days int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	old := syscall.Umask(0077)
	defer syscall.Umask(old)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	st := &Store{db: db, retentionDays: days}
	for _, query := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE IF NOT EXISTS current (kind TEXT PRIMARY KEY, sampled_at INTEGER NOT NULL, received_at INTEGER NOT NULL, data TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS metrics (sampled_at INTEGER NOT NULL, node TEXT NOT NULL, name TEXT NOT NULL, value REAL NOT NULL, unit TEXT NOT NULL, source TEXT NOT NULL, PRIMARY KEY (sampled_at,node,name))",
		"CREATE INDEX IF NOT EXISTS metrics_lookup ON metrics(node,name,sampled_at)",
		"CREATE TABLE IF NOT EXISTS changes (ts INTEGER NOT NULL, item TEXT NOT NULL, old_status TEXT NOT NULL, new_status TEXT NOT NULL)",
		"CREATE INDEX IF NOT EXISTS changes_lookup ON changes(ts)",
		"CREATE TABLE IF NOT EXISTS errors (kind TEXT PRIMARY KEY, at INTEGER NOT NULL, message TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS item_states(item TEXT PRIMARY KEY,status TEXT NOT NULL)",
	} {
		if _, err = db.Exec(query); err != nil {
			db.Close()
			return nil, fmt.Errorf("SQLite: %w", err)
		}
	}
	_ = os.Chmod(path, 0600)
	return st, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Put(kind string, sampled time.Time, payload any, metrics []Metric) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO current(kind,sampled_at,received_at,data) VALUES(?,?,?,?) ON CONFLICT(kind) DO UPDATE SET sampled_at=excluded.sampled_at,received_at=excluded.received_at,data=excluded.data", kind, sampled.Unix(), now.Unix(), string(b)); err != nil {
		return err
	}
	for _, m := range metrics {
		if _, err = tx.Exec("INSERT OR REPLACE INTO metrics(sampled_at,node,name,value,unit,source) VALUES(?,?,?,?,?,?)", m.SampledAt.Unix(), m.Node, m.Name, m.Value, m.Unit, m.Source); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM errors WHERE kind=?", kind); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) RecordError(kind string, e error) {
	if e == nil {
		return
	}
	msg := e.Error()
	if len(msg) > 180 {
		msg = msg[:180]
	}
	_, _ = s.db.Exec("INSERT INTO errors(kind,at,message) VALUES(?,?,?) ON CONFLICT(kind) DO UPDATE SET at=excluded.at,message=excluded.message", kind, time.Now().Unix(), msg)
}
func (s *Store) Current(kind string, maxAge time.Duration) (*Current, error) {
	var sampled, received int64
	var data []byte
	err := s.db.QueryRow("SELECT sampled_at,received_at,data FROM current WHERE kind=?", kind).Scan(&sampled, &received, &data)
	if err != nil {
		return nil, err
	}
	c := &Current{Kind: kind, SampledAt: time.Unix(sampled, 0).UTC(), ReceivedAt: time.Unix(received, 0).UTC(), Data: json.RawMessage(data)}
	c.Stale = time.Since(c.SampledAt) > maxAge || time.Since(c.ReceivedAt) > maxAge || c.SampledAt.After(time.Now().Add(2*time.Minute))
	return c, nil
}
func (s *Store) Latest() map[string]any {
	result := make(map[string]any)
	for _, kind := range []string{"status", "zbook", "truenas_metrics", "truenas_pool", "truenas_apps", "truenas_alerts"} {
		maxAge := 4 * time.Minute
		if kind == "truenas_pool" || kind == "truenas_apps" {
			maxAge = 5 * time.Minute
		}
		c, err := s.Current(kind, maxAge)
		if err == nil {
			result[kind] = c
		} else {
			result[kind] = map[string]any{"available": false, "stale": true}
		}
	}
	rows, err := s.db.Query("SELECT kind,at,message FROM errors")
	if err == nil {
		errs := map[string]any{}
		for rows.Next() {
			var kind, message string
			var ts int64
			if rows.Scan(&kind, &ts, &message) == nil {
				errs[kind] = map[string]any{"at": time.Unix(ts, 0).UTC(), "message": message}
			}
		}
		rows.Close()
		result["collection_errors"] = errs
	}
	return result
}
func (s *Store) RecordChanges(items map[string]string) error {
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for item, status := range items {
		var old string
		err = tx.QueryRow("SELECT status FROM item_states WHERE item=?", item).Scan(&old)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && old != status {
			if _, err = tx.Exec("INSERT INTO changes(ts,item,old_status,new_status) VALUES(?,?,?,?)", now.Unix(), item, old, status); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("INSERT INTO item_states(item,status) VALUES(?,?) ON CONFLICT(item) DO UPDATE SET status=excluded.status", item, status); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) RecentChanges(limit int) ([]map[string]any, error) {
	if limit < 1 || limit > 100 {
		limit = 30
	}
	rows, err := s.db.Query("SELECT ts,item,old_status,new_status FROM changes ORDER BY ts DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var ts int64
		var item, old, next string
		if err = rows.Scan(&ts, &item, &old, &next); err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"at": time.Unix(ts, 0).UTC(), "item": item, "previous": old, "current": next})
	}
	return result, rows.Err()
}
func (s *Store) History(node, metric string, hours, limit int) ([]map[string]any, error) {
	if hours < 1 || hours > 168 {
		hours = 24
	}
	if limit < 1 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(context.Background(), "SELECT sampled_at,value,unit,source FROM metrics WHERE node=? AND name=? AND sampled_at>=? ORDER BY sampled_at DESC LIMIT ?", node, metric, time.Now().Add(-time.Duration(hours)*time.Hour).Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var ts int64
		var val float64
		var unit, source string
		if err = rows.Scan(&ts, &val, &unit, &source); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"at": time.Unix(ts, 0).UTC(), "value": val, "unit": unit, "source": source})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}
func (s *Store) Prune() {
	cutoff := time.Now().AddDate(0, 0, -s.retentionDays).Unix()
	_, _ = s.db.Exec("DELETE FROM metrics WHERE sampled_at<?", cutoff)
	_, _ = s.db.Exec("DELETE FROM changes WHERE ts<?", cutoff)
}
