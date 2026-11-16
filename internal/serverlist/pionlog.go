package serverlist

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pion/logging"
)

// pionLogs passes the WebRTC library's own logs for LAN joins to the panel's
// log, all at debug (with LOG_LEVEL=debug), so a console that can't connect
// shows whether its connection checks arrive and what happens to them.
type pionLogs struct{}

func (pionLogs) NewLogger(scope string) logging.LeveledLogger {
	return pionLogger{slog.Default().With("src", "webrtc-lan", "scope", scope)}
}

type pionLogger struct{ l *slog.Logger }

func (p pionLogger) on() bool { return p.l.Enabled(context.Background(), slog.LevelDebug) }

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
