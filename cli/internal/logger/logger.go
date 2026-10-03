// Package logger provides logging configuration and access for the CLI.
//
// This package wraps charmbracelet/log to provide consistent logging across
// the CLI. It supports configurable log levels and JSON output format for
// machine parsing.
//
// # Setup
//
// Call Setup early in the application lifecycle:
//
//	log.Setup(cmd.ErrOrStderr(), "debug", false) // debug level, text format
//	log.Setup(cmd.ErrOrStderr(), "info", true)   // info level, JSON format
//
// # Usage
//
//	log := logger.GetLogger()
//	log.Debug("detailed information")
//	log.Info("general information")
//	log.Error("error occurred")
package logger

import (
	"io"

	charmlog "charm.land/log/v2"

	"github.com/getarcaneapp/arcane/cli/v2/internal/output"
)

// Setup configures the global logger with the specified level and format.
// Valid log levels are: debug, info, warn, error, fatal.
// If an invalid level is provided, it defaults to info.
// When jsonFormat is true, logs are output as JSON for machine parsing.
func Setup(w io.Writer, level string, jsonFormat bool) {
	lvl, err := charmlog.ParseLevel(level)
	if err != nil {
		lvl = charmlog.InfoLevel
	}

	log := charmlog.Default()
	log.SetLevel(lvl)
	log.SetOutput(output.CoordinatedWriter(w))
	log.SetReportTimestamp(true)

	if jsonFormat {
		log.SetFormatter(charmlog.JSONFormatter)
	} else {
		log.SetFormatter(charmlog.TextFormatter)
	}
}

// GetLogger returns the global logger instance.
// The logger should be configured via Setup before use.
func GetLogger() *charmlog.Logger {
	return charmlog.Default()
}
