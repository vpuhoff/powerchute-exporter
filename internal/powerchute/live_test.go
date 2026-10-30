package powerchute

import (
	"context"
	"os"
	"testing"
	"time"

	"powerchute-exporter/internal/config"
)

// Explicit opt-in only: normal tests never contact a real UPS agent.
func TestLiveSessionReconnection(t *testing.T) {
	if os.Getenv("POWERCHUTE_LIVE_TEST") != "1" {
		t.Skip("set POWERCHUTE_LIVE_TEST=1 and POWERCHUTE_CONFIG to opt in")
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
		t.Fatal("live test requires read-only existing report mode")
	}
	creds, err := cfg.PowerChute.Credentials()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg, creds)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Logout(ctx); err != nil {
			t.Error(err)
		}
	})
	attempts := 0
	c.AuthResult = func(ok bool) {
		if !ok {
			t.Error("live login failed")
		}
		attempts++
	}
	cycle := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Exporter.CycleTimeoutSeconds)*time.Second)
		defer cancel()
		e, err := c.CollectEnergy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		dataCtx, dataCancel := context.WithTimeout(context.Background(), time.Duration(cfg.Exporter.CycleTimeoutSeconds)*time.Second)
		defer dataCancel()
		d, err := c.CollectDataLog(dataCtx)
		if err != nil {
			t.Fatal(err)
		}
		if e.KWh < 0 || d.Timestamp.IsZero() {
			t.Fatal("invalid live snapshot")
		}
		t.Logf("energy=%.2f kWh, cost=%.2f %s, CO2=%.2f kg, period=%s..%s; datalog=%s input=%.1f V battery=%.1f V charge=%.3f load=%.3f", e.KWh, e.Cost, cfg.Energy.Currency, e.CO2, e.From, e.To, d.Timestamp.Format(time.RFC3339), d.InputVoltage, d.BatteryVoltage, d.BatteryCharge, d.Load)
	}
	cycle()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	cycle()
	if attempts != 2 {
		t.Fatalf("expected two independently verified sessions, got %d", attempts)
	}
}
