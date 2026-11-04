package servers

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Overnight upkeep. When the owner turns it on, each night in a three-hour
// window the panel updates servers to a new Bedrock release and (if asked)
// restarts running servers, but only a server nobody is playing on. A
// server with players is tried again each minute until the window ends,
// then the next night. Each server gets at most one of these a night.

// Nightly is the owner's choice of overnight upkeep.
type Nightly struct {
	AutoUpdate   bool   `json:"autoUpdate"`
	DailyRestart bool   `json:"dailyRestart"`
	At           string `json:"at"` // "04:00": when the window starts, panel time
}

// nightlyWindow is how long after At the panel keeps trying.
const nightlyWindow = 3 * time.Hour

// NightlyPlan returns the overnight settings (off, at 04:00, by default).
func (m *Manager) NightlyPlan() Nightly {
	n := m.Settings().Nightly
	if n == nil {
		return Nightly{At: "04:00"}
	}
	if _, _, ok := clock(n.At); !ok {
		n.At = "04:00"
	}
	return *n
}

// SetNightlyPlan saves the overnight settings.
func (m *Manager) SetNightlyPlan(n Nightly) error {
	h, mi, ok := clock(n.At)
	if !ok {
		return errors.New("pick a time like 04:00")
	}
	n.At = fmt.Sprintf("%02d:%02d", h, mi)
	return m.changeSettings(func(s *Settings) { s.Nightly = &n })
}

func clock(at string) (h, m int, ok bool) {
	hs, ms, found := strings.Cut(at, ":")
	if !found {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	return h, m, err1 == nil && err2 == nil && h >= 0 && h < 24 && m >= 0 && m < 60 && len(ms) == 2
}

// windowAt returns the start of the window now is in, if it's in one.
func windowAt(at string, now time.Time) (time.Time, bool) {
	h, mi, ok := clock(at)
	if !ok {
		return time.Time{}, false
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
	if now.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	return start, now.Sub(start) < nightlyWindow
}

// versionNewer reports whether Bedrock version a is newer than b.
func versionNewer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// HoldVersion reports whether the server is kept on its version (skipped by
// overnight updates).
func (s *Server) HoldVersion() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta.HoldVersion
}

// SetHoldVersion keeps the server on its version, or lets overnight
// updates update it.
func (s *Server) SetHoldVersion(hold bool) error {
	s.mu.Lock()
	s.meta.HoldVersion = hold
	s.mu.Unlock()
	return s.saveMeta()
}

// nightly does a server's overnight upkeep if it's due.
func (s *Server) nightly(now time.Time, plan Nightly, latest string) {
	if !plan.AutoUpdate && !plan.DailyRestart {
		return
	}
	start, in := windowAt(plan.At, now)
	if !in {
		return
	}
	night := start.Format("2006-01-02")
	s.mu.Lock()
	skip := s.deleted || s.meta.LastNightly == night || s.busy || s.backupActive || s.coldBackup ||
		len(s.players) > 0 || s.state == StateImporting || s.state == StateRestoring || s.state == StateError
	version, preview, hold := s.meta.Version, s.meta.Preview, s.meta.HoldVersion
	java, build, hytale := s.isJava(), s.meta.Build, s.isHytale()
	paper := java && s.loaderLocked() == LoaderPaper && !s.meta.KeepVersion
	consoles := s.consolesOnLocked()
	consolesBehind := s.consolePluginsBehindLocked()
	running := s.cmd != nil && s.state == StateRunning
	upFor := time.Since(s.startedAt)
	s.mu.Unlock()
	if skip {
		return
	}
	behind := !preview && version != "" && versionNewer(latest, version)
	known := latest != ""
	if paper {
		r := s.m.LatestPaper(consoles)
		known, behind = r.Version != "", javaNewer(r, version, build)
		latest = r.label()
	} else if java {
		// Other types and chosen versions aren't moved on overnight (only
		// restarted, and their console support kept current).
		known, behind = true, false
	}
	if hytale {
		r := s.m.LatestHytale()
		known, behind = r.Version != "", hytaleNewer(r.Version, version)
		latest = r.Version
	}
	if plan.AutoUpdate && !known {
		return // the newest version isn't known yet: don't restart instead of updating
	}
	switch {
	case plan.AutoUpdate && !hold && behind:
		s.markNightly(night)
		what := "Bedrock " + latest
		if java {
			what = "Minecraft Java " + latest
		}
		if hytale {
			what = "Hytale " + latest
		}
		s.say(fmt.Sprintf("Updating to %s overnight (nobody is playing).", what))
		if err := s.UpdateOvernight(); err != nil {
			s.say("The overnight update didn't start: " + err.Error())
		} else {
			s.m.log.Info("overnight update", "id", s.meta.ID, "from", version, "to", latest)
		}
	case plan.AutoUpdate && consolesBehind:
		// New console support (Geyser keeps up with Bedrock updates, which
		// consoles install by themselves). A held server keeps its version.
		s.markNightly(night)
		s.say("Updating console support overnight (nobody is playing).")
		if err := s.UpdateOvernight(); err != nil {
			s.say("The overnight update didn't start: " + err.Error())
		} else {
			s.m.log.Info("overnight console support update", "id", s.meta.ID)
		}
	case plan.DailyRestart && running && upFor > time.Hour:
		s.markNightly(night)
		s.say("Restarting overnight (nobody is playing).")
		s.m.log.Info("overnight restart", "id", s.meta.ID)
		if !s.m.beginWork() {
			return
		}
		go func() {
			defer s.m.work.Done()
			if err := s.Restart(); err != nil {
				s.say("The overnight restart didn't finish: " + err.Error())
				s.m.log.Warn("overnight restart didn't finish; the server may be stopped", "id", s.meta.ID, "error", err)
			}
		}()
	}
}

func (s *Server) markNightly(night string) {
	s.mu.Lock()
	s.meta.LastNightly = night
	s.mu.Unlock()
	if err := s.saveMeta(); err != nil {
		s.m.log.Warn("could not save server details", "id", s.meta.ID, "error", err)
	}
}
