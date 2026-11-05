// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type statusEntry struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}
type statusSnapshot struct {
	CheckedAt    string        `json:"checked_at"`
	CheckedEpoch int64         `json:"checked_epoch"`
	Machines     []statusEntry `json:"machines"`
	Services     []statusEntry `json:"services"`
}

func collectStatus(c Config, s *Store) {
	b, err := os.ReadFile(c.StatusPath)
	if err != nil {
		s.RecordError("status", err)
		return
	}
	var snapshot statusSnapshot
	if err = json.Unmarshal(b, &snapshot); err != nil {
		s.RecordError("status", err)
		return
	}
	if snapshot.CheckedEpoch <= 0 {
		s.RecordError("status", errors.New("snapshot has no valid timestamp"))
		return
	}
	at := time.Unix(snapshot.CheckedEpoch, 0).UTC()
	if time.Since(at) > 5*time.Minute || at.After(time.Now().Add(time.Minute)) {
		s.RecordError("status", fmt.Errorf("status snapshot is stale or in the future; checked_at=%s", at.Format(time.RFC3339)))
		return
	}
	if err = s.Put("status", at, snapshot, nil); err != nil {
		s.RecordError("status", err)
		return
	}
	changes := map[string]string{}
	for _, m := range snapshot.Machines {
		changes["machine:"+m.Name] = m.Status
	}
	for _, svc := range snapshot.Services {
		changes["service:"+svc.Name] = svc.Status
	}
	if err = s.RecordChanges(changes); err != nil {
		s.RecordError("changes", err)
	}
}

func cpuTicks() (idle, total uint64, err error) {
	f, e := os.Open("/proc/stat")
	if e != nil {
		return 0, 0, e
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	if !scan.Scan() {
		return 0, 0, errors.New("empty /proc/stat")
	}
	fields := strings.Fields(scan.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, errors.New("invalid /proc/stat")
	}
	var values []uint64
	for _, v := range fields[1:] {
		n, e := strconv.ParseUint(v, 10, 64)
		if e != nil {
			return 0, 0, e
		}
		values = append(values, n)
		total += n
	}
	idle = values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return idle, total, nil
}
func memoryAvailable() (total, available uint64, err error) {
	f, e := os.Open("/proc/meminfo")
	if e != nil {
		return 0, 0, e
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		a := strings.Fields(scan.Text())
		if len(a) < 2 {
			continue
		}
		n, e := strconv.ParseUint(a[1], 10, 64)
		if e != nil {
			continue
		}
		switch a[0] {
		case "MemTotal:":
			total = n * 1024
		case "MemAvailable:":
			available = n * 1024
		}
	}
	if total == 0 {
		return 0, 0, errors.New("MemTotal missing")
	}
	return total, available, scan.Err()
}
func diskFree(path string) (total, available uint64, err error) {
	var st syscall.Statfs_t
	if e := syscall.Statfs(path, &st); e != nil {
		return 0, 0, e
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), nil
}

type zbookCollector struct{ previousIdle, previousTotal uint64 }

func (z *zbookCollector) run(s *Store) {
	now := time.Now().UTC()
	idle, total, err := cpuTicks()
	if err != nil {
		s.RecordError("zbook", err)
		return
	}
	values := map[string]any{"source": "local Linux procfs"}
	metrics := []Metric{}
	if z.previousTotal > 0 && total > z.previousTotal && idle >= z.previousIdle {
		usage := 100 * (1 - float64(idle-z.previousIdle)/float64(total-z.previousTotal))
		if usage < 0 {
			usage = 0
		}
		if usage > 100 {
			usage = 100
		}
		values["cpu_percent"] = usage
		metrics = append(metrics, Metric{"zbook", "cpu_percent", usage, "percent", "Linux procfs", now})
	}
	z.previousIdle, z.previousTotal = idle, total
	totalMem, available, err := memoryAvailable()
	if err != nil {
		s.RecordError("zbook", err)
		return
	}
	values["memory_total_bytes"] = totalMem
	values["memory_available_bytes"] = available
	metrics = append(metrics, Metric{"zbook", "memory_available_bytes", float64(available), "bytes", "Linux procfs", now})
	for _, name := range []string{"/", "/var"} {
		capacity, free, e := diskFree(name)
		if e != nil {
			continue
		}
		key := "root"
		if name == "/var" {
			key = "var"
		}
		values[key+"_disk_total_bytes"] = capacity
		values[key+"_disk_available_bytes"] = free
		metrics = append(metrics, Metric{"zbook", key + "_disk_available_bytes", float64(free), "bytes", "Linux statfs", now})
	}
	if err = s.Put("zbook", now, values, metrics); err != nil {
		s.RecordError("zbook", err)
	}
}

func collectorLoop(ctx context.Context, c Config, s *Store, ready chan<- struct{}) {
	z := &zbookCollector{}
	run := func() {
		collectStatus(c, s)
		z.run(s)
		nasCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
		collectTrueNAS(nasCtx, c, s)
		cancel()
		s.Prune()
	}
	run()
	close(ready)
	ticker := time.NewTicker(time.Duration(c.PollSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
