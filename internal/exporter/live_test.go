package exporter

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"powerchute-exporter/internal/config"
	"powerchute-exporter/internal/powerchute"
)

func TestLiveBackgroundHTTP(t *testing.T) {
	if os.Getenv("POWERCHUTE_LIVE_TEST") != "1" {
		t.Skip("explicit live-test opt-in required")
	}
	path := os.Getenv("POWERCHUTE_CONFIG")
	if path == "" {
		t.Fatal("POWERCHUTE_CONFIG must be explicit")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Energy.PeriodMode != "existing" {
		t.Fatal("live test requires existing report mode")
	}
	creds, err := cfg.PowerChute.Credentials()
	if err != nil {
		t.Fatal(err)
	}
	client, err := powerchute.New(cfg, creds)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Logout(ctx); err != nil {
			t.Error(err)
		}
	}()
	e := New(cfg, client, "live-test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(e.Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("background workers did not stop")
		}
	}()
	httpClient := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(40 * time.Second)
	var healthy bool
	for time.Now().Before(deadline) {
		r, err := httpClient.Get(srv.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.StatusCode == 200 {
			t.Log("health: " + string(b))
			healthy = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("exporter never obtained two fresh real snapshots")
	}
	e.mu.RLock()
	authBefore := e.auth
	e.mu.RUnlock()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := httpClient.Get(srv.URL + "/metrics")
			if err != nil {
				t.Error(err)
				return
			}
			defer r.Body.Close()
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			p := expfmt.NewTextParser(model.UTF8Validation)
			metrics, err := p.TextToMetricFamilies(strings.NewReader(string(b)))
			if err != nil {
				t.Error(err)
				return
			}
			for _, name := range []string{"powerchute_energy_kwh", "powerchute_energy_cost", "powerchute_co2_kg", "powerchute_input_voltage_volts", "powerchute_battery_voltage_volts", "powerchute_battery_charge_ratio", "powerchute_load_ratio", "powerchute_datalog_fresh"} {
				if metrics[name] == nil {
					t.Errorf("missing live metric %s", name)
				}
			}
		}()
	}
	wg.Wait()
	e.mu.RLock()
	authAfter := e.auth
	e.mu.RUnlock()
	if authBefore != authAfter {
		t.Fatal("parallel scrapes triggered authentication")
	}
}
