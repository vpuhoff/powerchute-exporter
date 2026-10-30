package exporter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"powerchute-exporter/internal/config"
	"powerchute-exporter/internal/powerchute"
)

func setup(t *testing.T) (*Exporter, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	t.Cleanup(srv.Close)
	c := config.Defaults()
	c.PowerChute.Timezone = "Asia/Yerevan"
	c.PowerChute.BaseURL = srv.URL
	c.Energy.Currency = "RUB"
	client, err := powerchute.New(c, config.Credentials{Username: "secret-user", Password: "secret-password"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	e := New(c, client, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	return e, &requests
}

func response(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func seed(e *Exporter, now time.Time) {
	e.states["energy"].LastSuccess = now
	e.states["energy"].Success = true
	e.states["energy"].Energy = &powerchute.EnergySnapshot{KWh: 100, Cost: 200, CO2: 30, From: "2026-01-01", To: "2026-10-30"}
	e.states["datalog"].LastSuccess = now
	e.states["datalog"].Success = true
	e.states["datalog"].DataLog = &powerchute.DataSnapshot{InputVoltage: 230, BatteryVoltage: 27.1, BatteryCharge: 1, Load: 1.3, Timestamp: now.Add(-20 * time.Minute)}
}

func TestInitialCacheAndValidPrometheusFormat(t *testing.T) {
	e, requests := setup(t)
	h := e.Handler()
	if w := response(t, h, "/health"); w.Code != 503 {
		t.Fatal("empty cache reported healthy")
	}
	w := response(t, h, "/metrics")
	if w.Code != 200 || strings.Contains(w.Body.String(), "powerchute_energy_kwh ") || strings.Contains(w.Body.String(), "powerchute_last_success_timestamp_seconds{") {
		t.Fatal("uninitialized source exported invented values")
	}
	p := expfmt.NewTextParser(model.UTF8Validation)
	if _, err := p.TextToMetricFamilies(strings.NewReader(w.Body.String())); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("scrape accessed PowerChute")
	}
}

func TestTTLIndependentErrorsFreshnessAndPeriod(t *testing.T) {
	e, _ := setup(t)
	now := time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)
	seed(e, now)
	e.Now = func() time.Time { return now }
	h := e.Handler()
	w := response(t, h, "/health")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var health struct {
		Sources map[string]SourceHealth `json:"sources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health.Sources["energy"].Period.From != "2026-01-01" {
		t.Fatal("actual period missing")
	}
	// An HTTP failure marks only this source and retains its last complete snapshot.
	if err := e.PollOnce(context.Background(), "energy"); err == nil {
		t.Fatal("expected HTTP failure")
	}
	w = response(t, h, "/metrics")
	text := w.Body.String()
	for _, line := range []string{`powerchute_scrape_success{source="energy"} 0`, `powerchute_scrape_success{source="datalog"} 1`, `powerchute_energy_kwh 100`, `powerchute_scrape_errors_total{reason="http",source="energy"} 1`, `powerchute_load_ratio 1.3`} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing metric %s", line)
		}
	}
	if response(t, h, "/health").Code != 200 {
		t.Fatal("usable cache should remain healthy after HTTP failure")
	}
	now = now.Add(180 * time.Second)
	text = response(t, h, "/metrics").Body.String()
	if strings.Contains(text, "powerchute_input_voltage_volts ") || !strings.Contains(text, "powerchute_energy_kwh 100") {
		t.Fatal("TTL did not expire independently")
	}
	if response(t, h, "/health").Code != 503 {
		t.Fatal("expired datalog reported healthy")
	}
	now = now.Add(720 * time.Second)
	if strings.Contains(response(t, h, "/metrics").Body.String(), "powerchute_energy_kwh ") {
		t.Fatal("energy remained after TTL")
	}
}

func TestDataLogAgeAndFutureSkew(t *testing.T) {
	e, _ := setup(t)
	now := time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	h := e.Handler()
	for _, tt := range []struct {
		age   time.Duration
		fresh bool
	}{{20 * time.Minute, true}, {45 * time.Minute, true}, {45*time.Minute + time.Second, false}, {-60 * time.Second, true}, {-61 * time.Second, false}} {
		seed(e, now)
		e.states["datalog"].DataLog.Timestamp = now.Add(-tt.age)
		w := response(t, h, "/health")
		if (w.Code == 200) != tt.fresh {
			t.Fatalf("age %v: %s", tt.age, w.Body.String())
		}
		want := "powerchute_datalog_fresh 0"
		if tt.fresh {
			want = "powerchute_datalog_fresh 1"
		}
		if !strings.Contains(response(t, h, "/metrics").Body.String(), want) {
			t.Fatalf("age %v: freshness mismatch", tt.age)
		}
	}
}

func TestConcurrentScrapesDoNotCauseRequests(t *testing.T) {
	e, requests := setup(t)
	now := time.Now()
	seed(e, now)
	h := e.Handler()
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := response(t, h, "/metrics")
			if w.Code != 200 {
				t.Error("scrape failed")
			}
		}()
	}
	wg.Wait()
	if requests.Load() != 0 {
		t.Fatal("metrics scraping triggered collection")
	}
}

func TestPublicationKeepsWholeSnapshot(t *testing.T) {
	// A valid fixture is published together; a subsequent invalid page cannot replace half.
	body, err := os.ReadFile("../powerchute/testdata/energy.html")
	if err != nil {
		t.Fatal(err)
	}
	var bad atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bad.Load() {
			_, _ = io.WriteString(w, "<html>bad</html>")
		} else {
			_, _ = w.Write(body)
		}
	}))
	defer srv.Close()
	c := config.Defaults()
	c.PowerChute.Timezone = "UTC"
	c.PowerChute.BaseURL = srv.URL
	c.Energy.Currency = "RUB"
	c.DataLog.Enabled = false
	client, err := powerchute.New(c, config.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	e := New(c, client, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := e.PollOnce(context.Background(), "energy"); err != nil {
		t.Fatal(err)
	}
	prior := *e.states["energy"].Energy
	bad.Store(true)
	if err := e.PollOnce(context.Background(), "energy"); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if *e.states["energy"].Energy != prior {
		t.Fatal("failure modified cached snapshot")
	}
}

func TestBackoffIsBounded(t *testing.T) {
	for failure := 0; failure < 20; failure++ {
		d := nextDelay(time.Second, failure)
		n := failure
		if n > 3 {
			n = 3
		}
		base := time.Second * time.Duration(1<<n)
		if d < base || d > base+base/5 {
			t.Fatal("backoff out of range")
		}
	}
}
