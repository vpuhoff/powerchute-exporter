package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	PowerChute PowerChute `yaml:"powerchute"`
	Exporter   Exporter   `yaml:"exporter"`
	Energy     Energy     `yaml:"energy"`
	DataLog    DataLog    `yaml:"datalog"`
}

type PowerChute struct {
	BaseURL          string `yaml:"base_url"`
	Username         string `yaml:"username" json:"-"`
	Password         string `yaml:"password" json:"-"`
	UsernameEnv      string `yaml:"username_env"`
	PasswordEnv      string `yaml:"password_env"`
	UsernameFile     string `yaml:"username_file"`
	PasswordFile     string `yaml:"password_file"`
	Timezone         string `yaml:"timezone"`
	TLSCAFile        string `yaml:"tls_ca_file"`
	TLSVerify        bool   `yaml:"tls_verify"`
	AuthRetrySeconds int    `yaml:"auth_retry_seconds"`
}

type Exporter struct {
	ListenAddress         string `yaml:"listen_address"`
	Port                  int    `yaml:"port"`
	ConnectTimeoutSeconds int    `yaml:"connect_timeout_seconds"`
	ReadTimeoutSeconds    int    `yaml:"read_timeout_seconds"`
	CycleTimeoutSeconds   int    `yaml:"cycle_timeout_seconds"`
	MaxResponseBytes      int64  `yaml:"max_response_bytes"`
	MaxRedirects          int    `yaml:"max_redirects"`
}

type Source struct {
	Enabled             bool `yaml:"enabled"`
	PollIntervalSeconds int  `yaml:"poll_interval_seconds"`
	CacheTTLSeconds     int  `yaml:"cache_ttl_seconds"`
}

type NumberFormat struct {
	DecimalSeparator   string `yaml:"decimal_separator"`
	ThousandsSeparator string `yaml:"thousands_separator"`
}

type Energy struct {
	Source            `yaml:",inline"`
	NumberFormat      `yaml:",inline"`
	PeriodMode        string          `yaml:"period_mode"`
	StartDate         string          `yaml:"start_date"`
	EndDate           string          `yaml:"end_date"`
	Currency          string          `yaml:"currency"`
	AllowPeriodUpdate bool            `yaml:"allow_period_update"`
	DateFormat        string          `yaml:"date_format"`
	PostDateFormat    string          `yaml:"post_date_format"`
	ApplyValue        string          `yaml:"apply_value"`
	Selectors         EnergySelectors `yaml:"selectors"`
}

type EnergySelectors struct {
	Energy    string `yaml:"energy"`
	Cost      string `yaml:"cost"`
	CO2       string `yaml:"co2"`
	StartDate string `yaml:"start_date"`
	EndDate   string `yaml:"end_date"`
	Form      string `yaml:"form"`
}

type DataLog struct {
	Source               `yaml:",inline"`
	NumberFormat         `yaml:",inline"`
	PageSize             int    `yaml:"page_size"`
	MaxPages             int    `yaml:"max_pages"`
	RowsPath             string `yaml:"rows_path"`
	TotalPath            string `yaml:"total_path"`
	Ordering             string `yaml:"ordering"`
	InitializePage       bool   `yaml:"initialize_page"`
	DateFormat           string `yaml:"date_format"`
	TimeFormat           string `yaml:"time_format"`
	TwoDigitYearStart    int    `yaml:"two_digit_year_start"`
	MaxAgeSeconds        int    `yaml:"max_age_seconds"`
	MaxFutureSkewSeconds int    `yaml:"max_future_skew_seconds"`
}

func Defaults() Config {
	return Config{
		PowerChute: PowerChute{BaseURL: "https://localhost:6547", UsernameEnv: "POWERCHUTE_USERNAME", PasswordEnv: "POWERCHUTE_PASSWORD", TLSVerify: true, AuthRetrySeconds: 60},
		Exporter:   Exporter{ListenAddress: "127.0.0.1", Port: 9787, ConnectTimeoutSeconds: 5, ReadTimeoutSeconds: 10, CycleTimeoutSeconds: 30, MaxResponseBytes: 2 << 20, MaxRedirects: 5},
		Energy: Energy{Source: Source{true, 300, 900}, NumberFormat: NumberFormat{".", ","}, PeriodMode: "existing", DateFormat: "%m/%d/%Y", ApplyValue: "Apply", Selectors: EnergySelectors{
			Energy: "#pcbeenergykwh .pcbeenergytextvalue", Cost: "#pcbeenergycost .pcbeenergytextvalue", CO2: "#pcbeenergyco2 .pcbeenergytextvalue", StartDate: "input[name=startdate]", EndDate: "input[name=enddate]", Form: "form[name=energyreportingform]",
		}},
		DataLog: DataLog{Source: Source{true, 60, 180}, NumberFormat: NumberFormat{".", ","}, PageSize: 100, MaxPages: 1000, RowsPath: "/aaData", TotalPath: "/iTotalDisplayRecords", Ordering: "scan", DateFormat: "%m/%d/%y", TimeFormat: "%H:%M:%S", TwoDigitYearStart: 2000, MaxAgeSeconds: 2700, MaxFutureSkewSeconds: 60},
	}
}

func Load(path string) (Config, error) {
	c := Defaults()
	f, err := os.Open(path)
	if err != nil {
		return c, errors.New("cannot open configuration file")
	}
	defer f.Close()
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, errors.New("invalid YAML configuration (check field names and types)")
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, errors.New("configuration must contain one YAML document")
	}
	// File paths are relative to the configuration file, not the process directory.
	base, _ := filepath.Abs(filepath.Dir(path))
	for _, p := range []*string{&c.PowerChute.TLSCAFile, &c.PowerChute.UsernameFile, &c.PowerChute.PasswordFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	u, err := url.Parse(c.PowerChute.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return errors.New("powerchute.base_url must be an HTTP(S) origin without credentials, path or query")
	}
	if c.PowerChute.Timezone == "" {
		return errors.New("powerchute.timezone is required")
	}
	if _, err := time.LoadLocation(c.PowerChute.Timezone); err != nil {
		return errors.New("invalid powerchute.timezone")
	}
	if c.PowerChute.AuthRetrySeconds < 60 || c.PowerChute.AuthRetrySeconds > 86400 {
		return errors.New("powerchute.auth_retry_seconds must be in 60..86400")
	}
	if c.Exporter.Port < 1 || c.Exporter.Port > 65535 || net.ParseIP(c.Exporter.ListenAddress) == nil {
		return errors.New("exporter requires an IP listen_address and port in 1..65535")
	}
	for name, v := range map[string]int{"connect_timeout_seconds": c.Exporter.ConnectTimeoutSeconds, "read_timeout_seconds": c.Exporter.ReadTimeoutSeconds, "cycle_timeout_seconds": c.Exporter.CycleTimeoutSeconds} {
		if v < 1 || v > 86400 {
			return fmt.Errorf("exporter.%s must be in 1..86400", name)
		}
	}
	if c.Exporter.MaxResponseBytes < 1024 || c.Exporter.MaxResponseBytes > 64<<20 || c.Exporter.MaxRedirects < 1 || c.Exporter.MaxRedirects > 20 {
		return errors.New("invalid response size or redirect limit")
	}
	if !c.Energy.Enabled && !c.DataLog.Enabled {
		return errors.New("at least one source must be enabled")
	}
	for name, s := range map[string]Source{"energy": c.Energy.Source, "datalog": c.DataLog.Source} {
		if s.Enabled && (s.PollIntervalSeconds < 1 || s.PollIntervalSeconds > 604800 || s.CacheTTLSeconds < 1 || s.CacheTTLSeconds > 31536000) {
			return fmt.Errorf("invalid %s polling interval or TTL", name)
		}
	}
	for name, n := range map[string]NumberFormat{"energy": c.Energy.NumberFormat, "datalog": c.DataLog.NumberFormat} {
		if (n.DecimalSeparator != "." && n.DecimalSeparator != ",") || (n.ThousandsSeparator != "" && n.ThousandsSeparator != "." && n.ThousandsSeparator != "," && n.ThousandsSeparator != " ") || n.ThousandsSeparator == n.DecimalSeparator {
			return fmt.Errorf("invalid %s number separators", name)
		}
	}
	if c.Energy.Enabled {
		if !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(c.Energy.Currency) {
			return errors.New("energy.currency must be an explicit three-letter ISO currency code")
		}
		switch c.Energy.PeriodMode {
		case "existing":
		case "fixed_start_to_today", "fixed_range":
			if !c.Energy.AllowPeriodUpdate || c.Energy.PostDateFormat == "" {
				return errors.New("fixed periods require energy.allow_period_update and an explicitly verified post_date_format")
			}
			start, err := time.Parse("2006-01-02", c.Energy.StartDate)
			if err != nil {
				return errors.New("energy.start_date must be YYYY-MM-DD")
			}
			if c.Energy.PeriodMode == "fixed_range" {
				end, err := time.Parse("2006-01-02", c.Energy.EndDate)
				if err != nil || end.Before(start) {
					return errors.New("energy.end_date must be YYYY-MM-DD and not before start_date")
				}
			}
		default:
			return errors.New("invalid energy.period_mode")
		}
	}
	if c.DataLog.Enabled {
		d := c.DataLog
		if d.PageSize < 1 || d.PageSize > 1000 || d.MaxPages < 1 || d.MaxPages > 10000 || d.MaxAgeSeconds < 1 || d.MaxAgeSeconds > 31536000 || d.MaxFutureSkewSeconds < 0 || d.MaxFutureSkewSeconds > 86400 || d.TwoDigitYearStart < 1900 || d.TwoDigitYearStart > 9900 {
			return errors.New("invalid datalog pagination, freshness or year window")
		}
		if d.Ordering != "scan" && d.Ordering != "newest_first" {
			return errors.New("datalog.ordering must be scan or newest_first")
		}
		for _, p := range []string{d.RowsPath, d.TotalPath} {
			if p != "" && !strings.HasPrefix(p, "/") {
				return errors.New("datalog paths must be JSON pointers")
			}
		}
	}
	return nil
}

// Resolved credentials are passed only to the HTTP client. Never log Config or Credentials.
type Credentials struct{ Username, Password string }

func (c PowerChute) Credentials() (Credentials, error) {
	u, err := configuredSecret(c.Username, c.UsernameEnv, c.UsernameFile)
	if err != nil || u == "" {
		return Credentials{}, errors.New("username is missing or unreadable (username/username_env/username_file)")
	}
	p, err := configuredSecret(c.Password, c.PasswordEnv, c.PasswordFile)
	if err != nil || p == "" {
		return Credentials{}, errors.New("password is missing or unreadable (password/password_env/password_file)")
	}
	return Credentials{u, p}, nil
}

func configuredSecret(value, env, path string) (string, error) {
	if value != "" {
		return value, nil
	}
	return secret(env, path)
}

func secret(env, path string) (string, error) {
	if path == "" {
		return os.Getenv(env), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return "", errors.New("unreadable secret")
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}
