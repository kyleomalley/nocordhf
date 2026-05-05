// Package logging configures a zap logger that writes syslog-style entries to
// ./nocordhf.log and optionally to stderr (debug builds).
package logging

import (
	"fmt"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const LogFile = "nocordhf.log"

var L *zap.SugaredLogger

// Init opens (or creates) ./nocordhf.log and wires up zap. Equivalent to
// InitFile(debug, buildID, LogFile); kept for callers that don't need a
// custom log path.
func Init(debug bool, buildID string) error {
	return InitFile(debug, buildID, LogFile)
}

// InitFile is like Init but writes to the named file in the working
// directory (e.g. "nocordhf.log"). Multi-binary repos use this so each
// app gets its own log and they don't interleave.
// If debug is true, log output also goes to stderr at DEBUG level.
// buildID is stamped on every log line. Call Close() on shutdown.
func InitFile(debug bool, buildID, logPath string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	// syslog-style time format: Jan _2 15:04:05
	timeEncoder := zapcore.TimeEncoderOfLayout("Jan 02 15:04:05")

	fileEnc := zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
		TimeKey:          "ts",
		LevelKey:         "level",
		NameKey:          "logger",
		CallerKey:        "caller",
		MessageKey:       "msg",
		EncodeTime:       timeEncoder,
		EncodeLevel:      syslogLevel,
		EncodeCaller:     zapcore.ShortCallerEncoder,
		EncodeDuration:   zapcore.StringDurationEncoder,
		ConsoleSeparator: " ",
	})

	fileLevel := zapcore.InfoLevel
	if debug {
		fileLevel = zapcore.DebugLevel
	}

	cores := []zapcore.Core{
		zapcore.NewCore(fileEnc, zapcore.AddSync(f), fileLevel),
	}

	if debug {
		stderrEnc := zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
			TimeKey:          "ts",
			LevelKey:         "level",
			MessageKey:       "msg",
			CallerKey:        "caller",
			EncodeTime:       timeEncoder,
			EncodeLevel:      syslogLevel,
			EncodeCaller:     zapcore.ShortCallerEncoder,
			EncodeDuration:   zapcore.StringDurationEncoder,
			ConsoleSeparator: " ",
		})
		cores = append(cores, zapcore.NewCore(stderrEnc, zapcore.AddSync(os.Stderr), zapcore.DebugLevel))
	}

	base := zap.New(
		zapcore.NewTee(cores...),
		zap.AddCaller(),
		zap.AddCallerSkip(0),
		zap.Fields(zap.String("b", buildID)),
	)
	L = base.Sugar()
	return nil
}

// Close flushes buffered log entries.
func Close() {
	if L != nil {
		_ = L.Sync()
	}
}

// NewFileLogger constructs a standalone *zap.SugaredLogger that
// writes ONLY to the named file at the requested level — independent
// of the package-global L. Used by features that want a dedicated
// tail-able log (the MeshCore client's wire trace lives in
// nocordhf-meshcore.log so the operator can `tail -f` it without
// hunting through the main app log).
//
// The returned logger is safe to share across goroutines. buildID
// stamps every line so multi-process / multi-build setups stay
// distinguishable.
func NewFileLogger(path, buildID string, level zapcore.Level) (*zap.SugaredLogger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", path, err)
	}
	enc := zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
		TimeKey:          "ts",
		LevelKey:         "level",
		MessageKey:       "msg",
		EncodeTime:       zapcore.TimeEncoderOfLayout("Jan 02 15:04:05"),
		EncodeLevel:      syslogLevel,
		EncodeDuration:   zapcore.StringDurationEncoder,
		ConsoleSeparator: " ",
	})
	core := zapcore.NewCore(enc, zapcore.AddSync(f), level)
	return zap.New(core, zap.Fields(zap.String("b", buildID))).Sugar(), nil
}

// syslogLevel formats levels as syslog severity words, uppercase padded.
func syslogLevel(l zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	switch l {
	case zapcore.DebugLevel:
		enc.AppendString("DEBUG")
	case zapcore.InfoLevel:
		enc.AppendString("INFO ")
	case zapcore.WarnLevel:
		enc.AppendString("WARN ")
	case zapcore.ErrorLevel:
		enc.AppendString("ERR  ")
	case zapcore.FatalLevel:
		enc.AppendString("CRIT ")
	default:
		enc.AppendString("INFO ")
	}
}
