package serverlist

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pion/logging"
)

// pionLogs passes the WebRTC library's connection-setup logs (ICE: checks
// sent and received, pairs found, state changes) for LAN joins to the
// panel's log at debug (with LOG_LEVEL=debug), so a console that can't
// connect shows why. Other parts (SCTP logs every packet, mDNS every
// device on the network) stay quiet.
type pionLogs struct{}

func (pionLogs) NewLogger(scope string) logging.LeveledLogger {
	return pionLogger{l: slog.Default().With("src", "webrtc-lan", "scope", scope), quiet: scope != "ice"}
}

type pionLogger struct {
	l     *slog.Logger
	quiet bool
}

func (p pionLogger) on() bool {
	return !p.quiet && p.l.Enabled(context.Background(), slog.LevelDebug)
}

func (p pionLogger) log(msg string) {
	if p.on() {
		p.l.Debug(msg)
	}
}

func (p pionLogger) logf(format string, args ...any) {
	if p.on() {
		p.l.Debug(fmt.Sprintf(format, args...))
	}
}

func (p pionLogger) Trace(msg string)                  { p.log(msg) }
func (p pionLogger) Tracef(format string, args ...any) { p.logf(format, args...) }
func (p pionLogger) Debug(msg string)                  { p.log(msg) }
func (p pionLogger) Debugf(format string, args ...any) { p.logf(format, args...) }
func (p pionLogger) Info(msg string)                   { p.log(msg) }
func (p pionLogger) Infof(format string, args ...any)  { p.logf(format, args...) }
func (p pionLogger) Warn(msg string)                   { p.log(msg) }
func (p pionLogger) Warnf(format string, args ...any)  { p.logf(format, args...) }
func (p pionLogger) Error(msg string)                  { p.log(msg) }
func (p pionLogger) Errorf(format string, args ...any) { p.logf(format, args...) }
