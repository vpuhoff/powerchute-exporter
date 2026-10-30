// Package daemon connects the exporter lifecycle to the operating system.
package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
)

const ServiceName = "PowerChuteExporter"

// Runner calls ready after initialization and stops when its context is canceled.
type Runner func(context.Context, func(), *slog.Logger) int

var ErrNotRunning = errors.New("service is not running")

func consoleLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(os.Stderr, nil)) }
