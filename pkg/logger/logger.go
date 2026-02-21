// Package logger provides a structured logger built on go.uber.org/zap.
package logger

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Logger is the shared logger type used throughout the proxy.
type Logger = *zap.SugaredLogger

// Config holds parameters needed to construct a Logger.
type Config struct {
	Level      string // debug | info | warn | error
	Format     string // console | json
	OutputFile string // optional log file path (empty = stdout only)
}

// New constructs a production-ready Logger from cfg.
func New(cfg Config) (Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	outputPaths := []string{"stdout"}
	if cfg.OutputFile != "" {
		outputPaths = append(outputPaths, cfg.OutputFile)
	}

	zapCfg := zap.Config{
		Level:            zap.NewAtomicLevelAt(level),
		Development:      false,
		Encoding:         normaliseFormat(cfg.Format),
		EncoderConfig:    buildEncoderConfig(cfg.Format),
		OutputPaths:      outputPaths,
		ErrorOutputPaths: []string{"stderr"},
	}

	base, err := zapCfg.Build(zap.AddCaller(), zap.AddCallerSkip(0))
	if err != nil {
		return nil, fmt.Errorf("build zap logger: %w", err)
	}
	return base.Sugar(), nil
}

// Must is like New but panics on error. Intended for use in main().
func Must(cfg Config) Logger {
	l, err := New(cfg)
	if err != nil {
		panic(fmt.Sprintf("logger.Must: %v", err))
	}
	return l
}

func parseLevel(s string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return zapcore.DebugLevel, nil
	case "", "info":
		return zapcore.InfoLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return 0, fmt.Errorf("unknown log level %q; must be debug|info|warn|error", s)
	}
}

func normaliseFormat(f string) string {
	if strings.ToLower(f) == "json" {
		return "json"
	}
	return "console"
}

func buildEncoderConfig(format string) zapcore.EncoderConfig {
	if strings.ToLower(format) == "json" {
		cfg := zap.NewProductionEncoderConfig()
		cfg.TimeKey = "ts"
		cfg.EncodeTime = zapcore.ISO8601TimeEncoder
		return cfg
	}
	cfg := zap.NewDevelopmentEncoderConfig()
	cfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	cfg.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05.000")
	return cfg
}
