package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeReadyAndShutdown(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := fmt.Sprintf("powerchute:\n  base_url: http://127.0.0.1:1\n  timezone: UTC\n  username: test-user\n  password: test-password\nexporter:\n  port: %d\nenergy:\n  currency: USD\ndatalog:\n  enabled: false\n", port)
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ready := false
	code := serve(ctx, path, func() {
		ready = true
		defer cancel()
		client := &http.Client{Timeout: time.Second}
		r, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		if err != nil {
			t.Error(err)
			return
		}
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if r.StatusCode != 200 || !strings.Contains(string(body), "powerchute_exporter_build_info") {
			t.Error("ready was called before metrics became available")
		}
	}, logger)
	if !ready || code != 0 {
		t.Fatal("runtime failed to initialize or shut down cleanly")
	}
}

func TestServeInvalidConfigNeverSignalsReady(t *testing.T) {
	ready := false
	code := serve(context.Background(), filepath.Join(t.TempDir(), "missing.yaml"), func() { ready = true }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if ready || code != 1 {
		t.Fatal("initialization failure was not propagated to the service manager")
	}
}
