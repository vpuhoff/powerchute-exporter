//go:build windows

package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

type eventWriter struct{ log *eventlog.Log }

func (w eventWriter) Write(p []byte) (int, error) {
	var entry struct {
		Level string `json:"level"`
	}
	_ = json.Unmarshal(p, &entry)
	message := strings.TrimSpace(string(p))
	var err error
	switch entry.Level {
	case "ERROR":
		err = w.log.Error(1, message)
	case "WARN":
		err = w.log.Warning(1, message)
	default:
		err = w.log.Info(1, message)
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func Run(ctx context.Context, runner Runner) int {
	isService, err := svc.IsWindowsService()
	if err != nil {
		consoleLogger().Error("cannot detect Windows service mode")
		return 1
	}
	if !isService {
		return runner(ctx, func() {}, consoleLogger())
	}
	events, err := eventlog.Open(ServiceName)
	if err != nil {
		return 1
	}
	defer events.Close()
	h := &serviceHandler{parent: ctx, runner: runner, logger: slog.New(slog.NewJSONHandler(eventWriter{events}, nil))}
	if err := svc.Run(ServiceName, h); err != nil {
		h.logger.Error("Windows service dispatcher failed")
		return 1
	}
	return h.exitCode
}

type serviceHandler struct {
	parent   context.Context
	runner   Runner
	logger   *slog.Logger
	exitCode int
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(h.parent)
	defer cancel()
	ready := make(chan struct{})
	var once sync.Once
	done := make(chan int, 1)
	status := svc.Status{State: svc.StartPending, WaitHint: 15000, CheckPoint: 1}
	changes <- status
	go func() { done <- h.runner(ctx, func() { once.Do(func() { close(ready) }) }, h.logger) }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var stopping time.Time
	parentDone := h.parent.Done()
	stop := func() {
		if !stopping.IsZero() {
			return
		}
		stopping = time.Now()
		cancel()
		status = svc.Status{State: svc.StopPending, WaitHint: 15000, CheckPoint: 1}
		changes <- status
	}
	for {
		select {
		case <-ready:
			ready = nil
			if stopping.IsZero() {
				status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				changes <- status
			}
		case code := <-done:
			h.exitCode = code
			return code != 0, uint32(code)
		case request, ok := <-requests:
			if !ok {
				stop()
				requests = nil
				continue
			}
			switch request.Cmd {
			case svc.Interrogate:
				changes <- status
			case svc.Stop, svc.Shutdown:
				stop()
			}
		case <-parentDone:
			stop()
			parentDone = nil
		case <-tick.C:
			if !stopping.IsZero() && time.Since(stopping) > 30*time.Second {
				h.exitCode = 1
				h.logger.Error("service shutdown deadline exceeded")
				return true, 1
			}
			if status.State == svc.StartPending || status.State == svc.StopPending {
				status.CheckPoint++
				changes <- status
			}
		}
	}
}
