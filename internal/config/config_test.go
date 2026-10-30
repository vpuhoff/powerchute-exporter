package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, s string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadStrictDefaults(t *testing.T) {
	p := writeConfig(t, "powerchute:\n  timezone: Asia/Yerevan\n  tls_ca_file: ca.pem\nenergy:\n  currency: RUB\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.PowerChute.TLSVerify || c.Exporter.ListenAddress != "127.0.0.1" || c.DataLog.PageSize != 10 || c.Energy.PeriodMode != "existing" || c.PowerChute.TLSCAFile != filepath.Join(filepath.Dir(p), "ca.pem") {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	for _, bad := range []string{"powerchute:\n  unknown_option: never-accept-this\n", "powerchute:\n  timezone: UTC\nenergy:\n  currency: RUB\n---\n{}", "powerchute:\n  timezone: Invalid/Zone\nenergy:\n  currency: RUB\n"} {
		if _, err := Load(writeConfig(t, bad)); err == nil || strings.Contains(err.Error(), "never-accept-this") {
			t.Fatal("invalid config accepted or leaked")
		}
	}
}

func TestInlineCredentialsOverrideOtherSources(t *testing.T) {
	t.Setenv("POWERCHUTE_USERNAME", "environment-user")
	t.Setenv("POWERCHUTE_PASSWORD", "environment-password")
	path := writeConfig(t, "powerchute:\n  timezone: UTC\n  username: 'config-user'\n  password: 'config-password+'\n  username_file: missing-user.secret\n  password_file: missing-password.secret\nenergy:\n  currency: RUB\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := c.PowerChute.Credentials()
	if err != nil || creds.Username != "config-user" || creds.Password != "config-password+" {
		t.Fatal("inline credentials were not used verbatim")
	}
	c.PowerChute.Username, c.PowerChute.Password = "", ""
	c.PowerChute.UsernameFile, c.PowerChute.PasswordFile = "", ""
	creds, err = c.PowerChute.Credentials()
	if err != nil || creds.Username != "environment-user" || creds.Password != "environment-password" {
		t.Fatal("environment fallback stopped working")
	}
}

func TestValidateBoundaries(t *testing.T) {
	base := Defaults()
	base.PowerChute.Timezone = "UTC"
	base.Energy.Currency = "RUB"
	for _, change := range []func(*Config){
		func(c *Config) { c.PowerChute.BaseURL = "https://user:secret@localhost:6547" },
		func(c *Config) { c.PowerChute.BaseURL = "https://localhost:6547/path" },
		func(c *Config) { c.Energy.PeriodMode = "fixed_range" },
		func(c *Config) { c.PowerChute.AuthRetrySeconds = 0 },
		func(c *Config) { c.DataLog.PageSize = -1 },
		func(c *Config) { c.DataLog.RowsPath = "aaData" },
		func(c *Config) { c.DataLog.Ordering = "guess" },
		func(c *Config) { c.Energy.Enabled = false; c.DataLog.Enabled = false },
	} {
		c := base
		change(&c)
		if c.Validate() == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestSecretFileOverridesEnv(t *testing.T) {
	t.Setenv("TEST_POWERCHUTE_PASSWORD", "env-secret")
	path := filepath.Join(t.TempDir(), "password.secret")
	if err := os.WriteFile(path, []byte(" file-secret \r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := secret("TEST_POWERCHUTE_PASSWORD", path)
	if err != nil || v != " file-secret " {
		t.Fatal("secret was not preserved")
	}
}
