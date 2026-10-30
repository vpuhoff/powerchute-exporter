package exporter

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"powerchute-exporter/internal/config"
	"powerchute-exporter/internal/powerchute"
)

type sourceState struct {
	Success     bool
	Duration    float64
	LastSuccess time.Time
	Reason      powerchute.Reason
	Errors      map[powerchute.Reason]uint64
	Energy      *powerchute.EnergySnapshot
	DataLog     *powerchute.DataSnapshot
}

type Exporter struct {
	cfg         config.Config
	client      *powerchute.Client
	version     string
	mu          sync.RWMutex
	collectGate chan struct{}
	states      map[string]*sourceState
	auth        [2]uint64 // failure, success
	desc        map[string]*prometheus.Desc
	Now         func() time.Time
	logger      *slog.Logger
}

func New(c config.Config, client *powerchute.Client, version string, logger *slog.Logger) *Exporter {
	if logger == nil {
		logger = slog.Default()
	}
	e := &Exporter{cfg: c, client: client, version: version, collectGate: make(chan struct{}, 1), states: map[string]*sourceState{}, desc: map[string]*prometheus.Desc{}, Now: time.Now, logger: logger}
	for name, enabled := range map[string]bool{"energy": c.Energy.Enabled, "datalog": c.DataLog.Enabled} {
		if enabled {
			e.states[name] = &sourceState{Errors: map[powerchute.Reason]uint64{}}
		}
	}
	defs := []struct {
		name, help string
		labels     []string
	}{
		{"powerchute_energy_kwh", "Energy for the actual report period in kWh.", nil},
		{"powerchute_energy_cost", "Cost for the actual report period.", []string{"currency"}},
		{"powerchute_co2_kg", "CO2 for the actual report period in kg.", nil},
		{"powerchute_input_voltage_volts", "Input line voltage of the latest datalog record.", nil},
		{"powerchute_battery_voltage_volts", "Battery voltage of the latest datalog record.", nil},
		{"powerchute_battery_charge_ratio", "Battery charge as a ratio.", nil},
		{"powerchute_load_ratio", "UPS load as a ratio; may exceed one during overload.", nil},
		{"powerchute_datalog_timestamp_seconds", "Unix timestamp of the latest datalog record.", nil},
		{"powerchute_datalog_age_seconds", "Current time minus the latest datalog timestamp.", nil},
		{"powerchute_datalog_fresh", "Whether datalog age is within the configured age and future-skew limits.", nil},
		{"powerchute_scrape_success", "Result of the latest background collection attempt.", []string{"source"}},
		{"powerchute_scrape_duration_seconds", "Duration of the latest background collection attempt.", []string{"source"}},
		{"powerchute_last_success_timestamp_seconds", "Time of the latest successful source collection.", []string{"source"}},
		{"powerchute_scrape_errors_total", "Failed background source collections by bounded reason.", []string{"source", "reason"}},
		{"powerchute_auth_attempts_total", "Login POST attempts by result.", []string{"result"}},
		{"powerchute_exporter_build_info", "Exporter build version.", []string{"version"}},
	}
	for _, d := range defs {
		e.desc[d.name] = prometheus.NewDesc(d.name, d.help, d.labels, nil)
	}
	client.AuthResult = func(ok bool) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if ok {
			e.auth[1]++
		} else {
			e.auth[0]++
		}
	}
	return e
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range e.desc {
		ch <- d
	}
}

func usable(s *sourceState, c config.Source, now time.Time) bool {
	return !s.LastSuccess.IsZero() && now.Sub(s.LastSuccess) < time.Duration(c.CacheTTLSeconds)*time.Second
}
func (e *Exporter) fresh(d *powerchute.DataSnapshot, now time.Time) bool {
	age := now.Sub(d.Timestamp).Seconds()
	return age >= -float64(e.cfg.DataLog.MaxFutureSkewSeconds) && age <= float64(e.cfg.DataLog.MaxAgeSeconds)
}
func boolValue(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

// Collect reads only memory. It must never authenticate or call PowerChute.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	now := e.Now()
	e.mu.RLock()
	defer e.mu.RUnlock()
	gauge := func(name string, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(e.desc[name], prometheus.GaugeValue, v, labels...)
	}
	counter := func(name string, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(e.desc[name], prometheus.CounterValue, float64(v), labels...)
	}
	gauge("powerchute_exporter_build_info", 1, e.version)
	counter("powerchute_auth_attempts_total", e.auth[0], "failure")
	counter("powerchute_auth_attempts_total", e.auth[1], "success")
	for name, s := range e.states {
		gauge("powerchute_scrape_success", boolValue(s.Success), name)
		gauge("powerchute_scrape_duration_seconds", s.Duration, name)
		if !s.LastSuccess.IsZero() {
			gauge("powerchute_last_success_timestamp_seconds", float64(s.LastSuccess.UnixNano())/1e9, name)
		}
		for _, r := range powerchute.Reasons {
			counter("powerchute_scrape_errors_total", s.Errors[r], name, string(r))
		}
		if name == "energy" && s.Energy != nil && usable(s, e.cfg.Energy.Source, now) {
			x := s.Energy
			gauge("powerchute_energy_kwh", x.KWh)
			gauge("powerchute_energy_cost", x.Cost, e.cfg.Energy.Currency)
			gauge("powerchute_co2_kg", x.CO2)
		}
		if name == "datalog" && s.DataLog != nil && usable(s, e.cfg.DataLog.Source, now) {
			x := s.DataLog
			gauge("powerchute_input_voltage_volts", x.InputVoltage)
			gauge("powerchute_battery_voltage_volts", x.BatteryVoltage)
			gauge("powerchute_battery_charge_ratio", x.BatteryCharge)
			gauge("powerchute_load_ratio", x.Load)
			gauge("powerchute_datalog_timestamp_seconds", float64(x.Timestamp.Unix()))
			gauge("powerchute_datalog_age_seconds", now.Sub(x.Timestamp).Seconds())
			gauge("powerchute_datalog_fresh", boolValue(e.fresh(x, now)))
		}
	}
}

type SourceHealth struct {
	Usable             bool              `json:"usable"`
	LastAttemptSuccess bool              `json:"last_attempt_success"`
	LastSuccess        *time.Time        `json:"last_success,omitempty"`
	Reason             powerchute.Reason `json:"reason,omitempty"`
	Period             *ReportPeriod     `json:"period,omitempty"`
	DataLogTimestamp   *time.Time        `json:"datalog_timestamp,omitempty"`
	DataLogFresh       *bool             `json:"datalog_fresh,omitempty"`
}
type ReportPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (e *Exporter) health(w http.ResponseWriter, r *http.Request) {
	now := e.Now()
	all := true
	sources := map[string]SourceHealth{}
	e.mu.RLock()
	for name, s := range e.states {
		cfg := e.cfg.Energy.Source
		if name == "datalog" {
			cfg = e.cfg.DataLog.Source
		}
		h := SourceHealth{Usable: usable(s, cfg, now), LastAttemptSuccess: s.Success, Reason: s.Reason}
		if !s.LastSuccess.IsZero() {
			t := s.LastSuccess
			h.LastSuccess = &t
		}
		if s.Energy != nil {
			h.Period = &ReportPeriod{s.Energy.From, s.Energy.To}
		}
		if s.DataLog != nil {
			t := s.DataLog.Timestamp
			fresh := e.fresh(s.DataLog, now)
			h.DataLogTimestamp = &t
			h.DataLogFresh = &fresh
			h.Usable = h.Usable && fresh
		}
		all = all && h.Usable
		sources[name] = h
	}
	e.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !all {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(struct {
		Healthy bool                    `json:"healthy"`
		Sources map[string]SourceHealth `json:"sources"`
	}{all, sources})
}

func (e *Exporter) Handler() http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(e)
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true}))
	mux.HandleFunc("GET /health", e.health)
	return mux
}

// PollOnce atomically replaces only the source that completed successfully.
func (e *Exporter) PollOnce(ctx context.Context, name string) error {
	// A source gets its full cycle budget after admission. Starting its deadline
	// while another source owns the shared client would turn queue time into an
	// unrelated network failure, especially during a long Data Log scan.
	select {
	case e.collectGate <- struct{}{}:
		defer func() { <-e.collectGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	parent := ctx
	start := e.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.cfg.Exporter.CycleTimeoutSeconds)*time.Second)
	defer cancel()
	var energy powerchute.EnergySnapshot
	var data powerchute.DataSnapshot
	var err error
	if name == "energy" {
		energy, err = e.client.CollectEnergy(ctx)
	} else {
		data, err = e.client.CollectDataLog(ctx)
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	now := e.Now()
	e.mu.Lock()
	s := e.states[name]
	if s == nil {
		e.mu.Unlock()
		return err
	}
	s.Duration = now.Sub(start).Seconds()
	s.Success = err == nil
	if err != nil {
		s.Reason = powerchute.ErrorReason(err)
		s.Errors[s.Reason]++
	} else {
		s.Reason = ""
		s.LastSuccess = now
		if name == "energy" {
			s.Energy = &energy
		} else {
			s.DataLog = &data
		}
	}
	e.mu.Unlock()
	if err != nil {
		e.logger.Warn("source collection failed", "source", name, "reason", powerchute.ErrorReason(err), "detail", powerchute.ErrorDetail(err), "duration_seconds", now.Sub(start).Seconds())
	} else if name == "energy" {
		e.logger.Info("energy report collected", "from", energy.From, "to", energy.To)
	}
	return err
}

func nextDelay(base time.Duration, failures int) time.Duration {
	if failures > 3 {
		failures = 3
	}
	// Bounded exponential backoff, plus 0..20% jitter (never a rapid retry).
	d := base * time.Duration(1<<failures)
	return d + time.Duration(rand.Float64()*0.2*float64(d))
}

// Run starts one worker per enabled source. The client serializes all cycles.
func (e *Exporter) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for name, s := range map[string]config.Source{"energy": e.cfg.Energy.Source, "datalog": e.cfg.DataLog.Source} {
		if !s.Enabled {
			continue
		}
		wg.Add(1)
		go func(name string, s config.Source) {
			defer wg.Done()
			failures := 0
			for {
				if ctx.Err() != nil {
					return
				}
				err := e.PollOnce(ctx, name)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					failures++
				} else {
					failures = 0
				}
				t := time.NewTimer(nextDelay(time.Duration(s.PollIntervalSeconds)*time.Second, failures))
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
		}(name, s)
	}
	wg.Wait()
}
