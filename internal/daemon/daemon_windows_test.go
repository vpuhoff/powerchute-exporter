//go:build windows

package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestServiceLifecycleWaitsForReadyAndStops(t *testing.T) {
	requests := make(chan svc.ChangeRequest, 1)
	changes := make(chan svc.Status, 20)
	allowReady := make(chan struct{})
	stopped := make(chan struct{})
	h := &serviceHandler{parent: context.Background(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), runner: func(ctx context.Context, ready func(), _ *slog.Logger) int {
		<-allowReady
		ready()
		<-ctx.Done()
		close(stopped)
		return 0
	}}
	done := make(chan uint32, 1)
	go func() { _, code := h.Execute(nil, requests, changes); done <- code }()
	if status := <-changes; status.State != svc.StartPending {
		t.Fatal("missing pending status")
	}
	select {
	case status := <-changes:
		if status.State == svc.Running {
			t.Fatal("service reported running before listener initialization")
		}
	case <-time.After(20 * time.Millisecond):
	}
	close(allowReady)
	if status := <-changes; status.State != svc.Running || status.Accepts&svc.AcceptStop == 0 {
		t.Fatal("service did not accept stop after readiness")
	}
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	if status := <-changes; status.State != svc.StopPending {
		t.Fatal("missing stop pending status")
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal("clean stop failed")
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the exporter")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("service returned before worker shutdown")
	}
}

func TestServiceStartupFailureIsReported(t *testing.T) {
	h := &serviceHandler{parent: context.Background(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), runner: func(context.Context, func(), *slog.Logger) int { return 1 }}
	changes := make(chan svc.Status, 10)
	specific, code := h.Execute(nil, make(chan svc.ChangeRequest), changes)
	if !specific || code != 1 || h.exitCode != 1 {
		t.Fatal("SCM did not receive startup failure")
	}
	for len(changes) > 0 {
		if (<-changes).State == svc.Running {
			t.Fatal("failed service reported running")
		}
	}
}
