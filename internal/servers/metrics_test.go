package servers

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStatFields(t *testing.T) {
	// A program name with spaces and a bracket must not shift the fields.
	line := "4242 (java (x) y) S 1 4240 4240 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 30 0 1000 5000000 2560 18446744073709551615"
	f, ok := statFields([]byte(line))
	if !ok || f.pgrp != 4240 || f.utime != 250 || f.stime != 50 || f.rss != 2560 {
		t.Fatalf("got %+v %v", f, ok)
	}
	if _, ok := statFields([]byte("garbage")); ok {
		t.Fatal("garbage should not parse")
	}
}

func TestGroupUsageOfThisTest(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	ticks, mem, err := groupUsage("/proc", syscall.Getpgrp())
	if err != nil || mem <= 0 {
		t.Fatalf("ticks %d mem %d err %v", ticks, mem, err)
	}
	if _, _, err := groupUsage("/proc", 1<<30); err == nil {
		t.Fatal("a missing process group should be an error")
	}
}

func TestRecordMakesMinutePoints(t *testing.T) {
	ms := newMetricStore(t.TempDir())
	base := time.Unix(1_800_000_000/60*60, 0)
	// First reading: nothing to compare with yet.
	ms.record("a", reading{pid: 10, ticks: 0, memMB: 500, players: 1}, base)
	if pts, _, now := ms.metricsFor("a", base.Add(-time.Hour), 360); len(pts) != 0 || now != nil {
		t.Fatalf("points %v now %v", pts, now)
	}
	ms.record("a", reading{pid: 10, ticks: 150, memMB: 700, players: 2}, base.Add(15*time.Second))
	ms.record("a", reading{pid: 10, ticks: 300, memMB: 600, players: 1}, base.Add(30*time.Second))
	// The next minute closes the first.
	ms.record("a", reading{pid: 10, ticks: 300, memMB: 600, players: 0}, base.Add(60*time.Second))
	sm := ms.servers["a"]
	if len(sm.points) != 1 {
		t.Fatalf("points %+v", sm.points)
	}
	p := sm.points[0]
	if p.T != base.Unix() || p.MemMB != 700 || p.Players != 2 || p.CPU <= 0 {
		t.Fatalf("point %+v", p)
	}
	// A stopped server has no current reading, and a new process starts afresh.
	ms.record("a", reading{}, base.Add(75*time.Second))
	if sm.now != nil || sm.pid != 0 {
		t.Fatal("a stopped server has no current reading")
	}
}

func TestDownsample(t *testing.T) {
	var pts []MetricPoint
	for i := int64(0); i < 10; i++ {
		pts = append(pts, MetricPoint{T: 6000 + i*60, CPU: float64(i), MemMB: int(100 + i), Players: int(i % 3)})
	}
	out := downsample(pts, 6000+2*60, 300) // 5-minute buckets from minute 2
	if len(out) != 2 || out[0].T != 6000 || out[1].T != 6300 {
		t.Fatalf("got %+v", out)
	}
	// Bucket 6000 holds minutes 2-4: CPU 2,3,4.
	if out[0].CPU != 3 || out[0].MemMB != 104 || out[0].Players != 2 {
		t.Fatalf("first bucket %+v", out[0])
	}
	if stepFor(7*24*time.Hour, 360) != 1680 || stepFor(time.Hour, 360) != 60 {
		t.Fatal("steps")
	}
}

func TestMetricsSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	ms := newMetricStore(dir)
	now := time.Now().Unix() / 60 * 60
	ms.get("a").points = []MetricPoint{{T: now - 120, CPU: 12.34, MemMB: 900, Players: 3}, {T: now - int64(8*24*3600), CPU: 1}}
	ms.get("a").changed = true
	ms.get("gone").changed = true
	_ = os.MkdirAll(dir+"/metrics", 0o755)
	_ = os.WriteFile(dir+"/metrics/old.json", []byte("[]"), 0o644)
	ms.save([]string{"a"})
	if _, err := os.Stat(dir + "/metrics/old.json"); err == nil {
		t.Fatal("a removed server's graph should be deleted")
	}
	b, _ := os.ReadFile(dir + "/metrics/a.json")
	if !strings.Contains(string(b), "12.3") {
		t.Fatalf("saved %s", b)
	}
	back := newMetricStore(dir)
	back.load([]string{"a"})
	pts := back.servers["a"].points
	if len(pts) != 1 || pts[0].MemMB != 900 || pts[0].Players != 3 {
		t.Fatalf("loaded %+v (points older than a week are dropped)", pts)
	}
}
