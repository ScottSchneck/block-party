package servers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Resource graphs. Every metricSample the panel reads how much CPU time and
// memory each running server's processes use (from /proc), and how many
// players are on. Each minute becomes one point (the average CPU, the most
// memory and players); a week of points is kept per server, in memory and in
// metrics/<id>.json so a restart doesn't lose them.

const (
	metricSample = 15 * time.Second
	metricKeep   = 7 * 24 * time.Hour
	metricSave   = 10 * time.Minute
	// userHZ is the unit of /proc's CPU times; it's 100 on every Linux.
	userHZ = 100
)

// MetricPoint is one minute of a server.
type MetricPoint struct {
	T       int64   `json:"t"`       // the minute (Unix seconds)
	CPU     float64 `json:"cpu"`     // % of the whole computer, averaged over the minute
	MemMB   int     `json:"memMB"`   // the most memory used
	Players int     `json:"players"` // the most players on
}

// Metrics is what the graphs show.
type Metrics struct {
	Points []MetricPoint `json:"points"`
	// Step is the seconds each point covers.
	Step int64 `json:"step"`
	// Now: the latest reading (nil while the server isn't running).
	Now *MetricPoint `json:"now,omitempty"`
	// The computer: CPU cores and memory (MB) the container may use, and
	// the server's memory setting (Java and Hytale; 0 for Bedrock).
	Cores     int `json:"cores"`
	HostMemMB int `json:"hostMemMB"`
	LimitMB   int `json:"limitMB,omitempty"`
}

type serverMetrics struct {
	points []MetricPoint
	// The minute being gathered.
	minute    int64
	cpuSum    float64
	cpuN      int
	memMax    int
	playerMax int
	// The last reading, to work out CPU use since.
	pid     int
	ticks   uint64
	at      time.Time
	now     *MetricPoint
	changed bool
}

type metricStore struct {
	mu      sync.Mutex
	servers map[string]*serverMetrics
	dir     string
}

func newMetricStore(dataDir string) *metricStore {
	return &metricStore{servers: map[string]*serverMetrics{}, dir: filepath.Join(dataDir, "metrics")}
}

func (ms *metricStore) get(id string) *serverMetrics {
	sm := ms.servers[id]
	if sm == nil {
		sm = &serverMetrics{}
		ms.servers[id] = sm
	}
	return sm
}

// load reads saved points for the servers in ids.
func (ms *metricStore) load(ids []string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	cutoff := time.Now().Add(-metricKeep).Unix()
	for _, id := range ids {
		b, err := os.ReadFile(filepath.Join(ms.dir, id+".json"))
		if err != nil {
			continue
		}
		var rows [][4]float64
		if json.Unmarshal(b, &rows) != nil {
			continue
		}
		sm := ms.get(id)
		for _, r := range rows {
			if int64(r[0]) >= cutoff {
				sm.points = append(sm.points, MetricPoint{T: int64(r[0]), CPU: r[1], MemMB: int(r[2]), Players: int(r[3])})
			}
		}
	}
}

// save writes the points of servers that changed, and removes files of
// servers that are gone (ids are the servers there are).
func (ms *metricStore) save(ids []string) {
	ms.mu.Lock()
	type file struct {
		id   string
		data []byte
	}
	var files []file
	keep := map[string]bool{}
	for _, id := range ids {
		keep[id] = true
		sm := ms.servers[id]
		if sm == nil || !sm.changed {
			continue
		}
		sm.changed = false
		rows := make([][4]float64, len(sm.points))
		for i, p := range sm.points {
			rows[i] = [4]float64{float64(p.T), float64(int(p.CPU*10+0.5)) / 10, float64(p.MemMB), float64(p.Players)}
		}
		b, _ := json.Marshal(rows)
		files = append(files, file{id, b})
	}
	for id := range ms.servers {
		if !keep[id] {
			delete(ms.servers, id)
		}
	}
	ms.mu.Unlock()
	_ = os.MkdirAll(ms.dir, 0o755)
	for _, f := range files {
		path := filepath.Join(ms.dir, f.id+".json")
		if os.WriteFile(path+".tmp", f.data, 0o644) == nil {
			_ = os.Rename(path+".tmp", path)
		}
	}
	if entries, err := os.ReadDir(ms.dir); err == nil {
		for _, e := range entries {
			if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && !keep[id] {
				_ = os.Remove(filepath.Join(ms.dir, e.Name()))
			}
		}
	}
}

// reading is one look at a server.
type reading struct {
	pid     int // the process group (0: not running)
	ticks   uint64
	memMB   int
	players int
}

// record adds a reading taken at now.
func (ms *metricStore) record(id string, r reading, now time.Time) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	sm := ms.get(id)
	minute := now.Unix() / 60 * 60
	if sm.cpuN > 0 && minute != sm.minute {
		sm.closeMinute()
	}
	if r.pid == 0 {
		sm.pid, sm.now = 0, nil
		return
	}
	cpu := -1.0
	if r.pid == sm.pid && !sm.at.IsZero() && r.ticks >= sm.ticks {
		if secs := now.Sub(sm.at).Seconds(); secs > 0 {
			cores := float64(runtime.NumCPU())
			cpu = float64(r.ticks-sm.ticks) / userHZ / secs / cores * 100
			if cpu > 100 {
				cpu = 100
			}
		}
	}
	sm.pid, sm.ticks, sm.at = r.pid, r.ticks, now
	if cpu < 0 {
		return // the first reading of a process: nothing to compare with yet
	}
	sm.now = &MetricPoint{T: now.Unix(), CPU: cpu, MemMB: r.memMB, Players: r.players}
	if sm.cpuN == 0 {
		sm.minute = minute
	}
	sm.cpuSum += cpu
	sm.cpuN++
	sm.memMax = max(sm.memMax, r.memMB)
	sm.playerMax = max(sm.playerMax, r.players)
}

// closeMinute turns the minute gathered so far into a point.
func (sm *serverMetrics) closeMinute() {
	sm.points = append(sm.points, MetricPoint{T: sm.minute, CPU: sm.cpuSum / float64(sm.cpuN), MemMB: sm.memMax, Players: sm.playerMax})
	cutoff := sm.minute - int64(metricKeep/time.Second)
	drop := 0
	for drop < len(sm.points) && sm.points[drop].T < cutoff {
		drop++
	}
	sm.points = sm.points[drop:]
	sm.cpuSum, sm.cpuN, sm.memMax, sm.playerMax = 0, 0, 0, 0
	sm.changed = true
}

// metricsFor returns the points since since, combined so there are at most
// about maxPoints.
func (ms *metricStore) metricsFor(id string, since time.Time, maxPoints int) ([]MetricPoint, int64, *MetricPoint) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	sm := ms.servers[id]
	if sm == nil {
		return []MetricPoint{}, 60, nil
	}
	var now *MetricPoint
	if sm.now != nil {
		c := *sm.now
		now = &c
	}
	step := stepFor(time.Since(since), maxPoints)
	return downsample(sm.points, since.Unix(), step), step, now
}

// stepFor is how many seconds each point covers when span is shown in at
// most maxPoints: a whole number of minutes.
func stepFor(span time.Duration, maxPoints int) int64 {
	minutes := int64(span/time.Minute+time.Duration(maxPoints)-1) / int64(maxPoints)
	return max(1, minutes) * 60
}

// downsample combines points from since on into buckets of step seconds
// (average CPU, most memory and players).
func downsample(points []MetricPoint, since, step int64) []MetricPoint {
	out := []MetricPoint{}
	var cur MetricPoint
	n := 0
	for _, p := range points {
		if p.T < since {
			continue
		}
		b := p.T / step * step
		if n > 0 && b != cur.T {
			cur.CPU /= float64(n)
			out = append(out, cur)
			n = 0
		}
		if n == 0 {
			cur = MetricPoint{T: b}
		}
		cur.CPU += p.CPU
		cur.MemMB = max(cur.MemMB, p.MemMB)
		cur.Players = max(cur.Players, p.Players)
		n++
	}
	if n > 0 {
		cur.CPU /= float64(n)
		out = append(out, cur)
	}
	return out
}

// groupUsage adds up the CPU time (in ticks) and memory (MB) of every
// process in process group pgid, from /proc.
func groupUsage(proc string, pgid int) (ticks uint64, memMB int, err error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return 0, 0, err
	}
	page := uint64(os.Getpagesize())
	var rss uint64
	found := false
	for _, e := range entries {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		b, err := os.ReadFile(filepath.Join(proc, e.Name(), "stat"))
		if err != nil {
			continue // it ended meanwhile
		}
		f, ok := statFields(b)
		if !ok || f.pgrp != pgid {
			continue
		}
		found = true
		ticks += f.utime + f.stime
		rss += f.rss * page
	}
	if !found {
		return 0, 0, errors.New("no such process")
	}
	return ticks, int(rss >> 20), nil
}

type procStat struct {
	pgrp         int
	utime, stime uint64
	rss          uint64 // pages
}

// statFields reads /proc/<pid>/stat. The program name, in brackets, can
// hold spaces and brackets, so the fields are counted after the last ")".
func statFields(b []byte) (procStat, bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return procStat{}, false
	}
	f := strings.Fields(string(b[i+1:]))
	// After the name: state(0) ppid(1) pgrp(2) ... utime(11) stime(12) ... rss(21)
	if len(f) < 22 {
		return procStat{}, false
	}
	var s procStat
	var err error
	if s.pgrp, err = strconv.Atoi(f[2]); err != nil {
		return procStat{}, false
	}
	s.utime, _ = strconv.ParseUint(f[11], 10, 64)
	s.stime, _ = strconv.ParseUint(f[12], 10, 64)
	s.rss, _ = strconv.ParseUint(f[21], 10, 64)
	return s, true
}

// hostMemoryMB is the memory the container may use: the computer's, or the
// container's limit when it has a smaller one.
func hostMemoryMB() int {
	total := 0
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
				kb, _ := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")))
				total = kb >> 10
				break
			}
		}
		f.Close()
	}
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil && n > 0 {
			if mb := int(n >> 20); total == 0 || mb < total {
				total = mb
			}
		}
	}
	return total
}

// RunMetrics reads each server's use every metricSample until the panel
// shuts down.
func (m *Manager) RunMetrics() {
	ids := func() []string {
		var out []string
		for _, s := range m.all() {
			out = append(out, s.ID())
		}
		return out
	}
	m.metrics.load(ids())
	go func() {
		t := time.NewTicker(metricSample)
		defer t.Stop()
		lastSave := time.Now()
		for {
			select {
			case <-m.ctx.Done():
				return
			case now := <-t.C:
				for _, s := range m.all() {
					m.metrics.record(s.ID(), s.reading(), now)
				}
				if now.Sub(lastSave) >= metricSave {
					m.metrics.save(ids())
					lastSave = now
				}
			}
		}
	}()
}

// saveMetrics writes the graphs' points (at shutdown).
func (m *Manager) saveMetrics() {
	var ids []string
	for _, s := range m.all() {
		ids = append(ids, s.ID())
	}
	m.metrics.save(ids)
}

// reading looks at the server's processes now.
func (s *Server) reading() reading {
	s.mu.Lock()
	if s.cmd == nil || s.cmd.Process == nil {
		s.mu.Unlock()
		return reading{}
	}
	pid := s.cmd.Process.Pid
	players := len(s.players)
	s.mu.Unlock()
	ticks, mem, err := groupUsage("/proc", pid)
	if err != nil {
		return reading{}
	}
	return reading{pid: pid, ticks: ticks, memMB: mem, players: players}
}

// Metrics returns the server's graphs since since.
func (s *Server) Metrics(since time.Time) Metrics {
	points, step, now := s.m.metrics.metricsFor(s.ID(), since, 360)
	out := Metrics{Points: points, Step: step, Now: now, Cores: runtime.NumCPU(), HostMemMB: hostMemoryMB()}
	s.mu.Lock()
	if s.isJava() || s.isHytale() {
		out.LimitMB = s.meta.MemoryMB
		if out.LimitMB == 0 {
			out.LimitMB = defaultMemoryMB
			if s.isHytale() {
				out.LimitMB = hytaleMemoryMB
			}
		}
	}
	s.mu.Unlock()
	return out
}
