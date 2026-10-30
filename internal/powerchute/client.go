package powerchute

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"powerchute-exporter/internal/config"
)

type Client struct {
	cfg        config.Config
	base       *url.URL
	http       *http.Client
	creds      config.Credentials
	loc        *time.Location
	gate       chan struct{}
	nextAuth   time.Time // protected by gate, shared across sources
	ownSession bool      // only release sessions established by this client
	AuthResult func(bool)
	Now        func() time.Time
}

func New(c config.Config, creds config.Credentials) (*Client, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateFormats(c); err != nil {
		return nil, err
	}
	u, _ := url.Parse(c.PowerChute.BaseURL)
	u.Path = ""
	loc, _ := time.LoadLocation(c.PowerChute.Timezone)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !c.PowerChute.TLSVerify} // explicit operator opt-out only
	if c.PowerChute.TLSCAFile != "" {
		pem, err := os.ReadFile(c.PowerChute.TLSCAFile)
		if err != nil {
			return nil, errors.New("cannot read powerchute.tls_ca_file")
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport := &http.Transport{
		DialContext:     (&net.Dialer{Timeout: time.Duration(c.Exporter.ConnectTimeoutSeconds) * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: tlsConfig, TLSHandshakeTimeout: time.Duration(c.Exporter.ConnectTimeoutSeconds) * time.Second,
		ResponseHeaderTimeout: time.Duration(c.Exporter.ReadTimeoutSeconds) * time.Second,
		MaxIdleConns:          2, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 1, IdleConnTimeout: 90 * time.Second,
	}
	jar, _ := cookiejar.New(nil)
	x := &Client{cfg: c, base: u, creds: creds, loc: loc, gate: make(chan struct{}, 1), Now: time.Now}
	x.http = &http.Client{Transport: transport, Jar: jar, Timeout: time.Duration(c.Exporter.ReadTimeoutSeconds) * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > c.Exporter.MaxRedirects || !sameOrigin(u, req.URL) || !allowedPath(req.URL.Path) {
			return fail(HTTP, "redirect rejected")
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead && req.URL.Path != via[0].URL.Path {
			return fail(HTTP, "POST redirect rejected")
		}
		return nil
	}}
	return x, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return b.User == nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

var sessionSuffix = regexp.MustCompile(`^;jsessionid=[A-Za-z0-9_.-]+$`)

func endpointPath(path string) (string, bool) {
	parts := strings.SplitN(path, ";", 2)
	if len(parts) == 2 && !sessionSuffix.MatchString(";"+parts[1]) {
		return "", false
	}
	return parts[0], true
}

func allowedPath(path string) bool {
	p, ok := endpointPath(path)
	if !ok {
		return false
	}
	switch p {
	case "/", "/status", "/logon", "/logoff", "/accessdenied", "/j_security_check", "/energyreporting", "/setenergyreporting", "/datalog", "/ajaxdatalog":
		return true
	}
	return false
}

func (c *Client) formAction(page *url.URL, action, want string) (*url.URL, error) {
	u, err := url.Parse(action)
	if err != nil {
		return nil, fail(Authentication, "invalid form action")
	}
	u = page.ResolveReference(u)
	p, ok := endpointPath(u.Path)
	if !ok || !sameOrigin(c.base, u) || p != want || u.RawQuery != "" || u.Fragment != "" {
		return nil, fail(Authentication, "form action rejected")
	}
	return u, nil
}

type response struct {
	body []byte
	url  *url.URL
}

func classifyNetwork(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var rh tls.RecordHeaderError
	if errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &rh) {
		return fail(TLS, "TLS verification or handshake failed")
	}
	return fail(Network, "request failed or timed out")
}

func (c *Client) request(ctx context.Context, method string, u *url.URL, values url.Values, ajax bool) (response, error) {
	if !sameOrigin(c.base, u) || !allowedPath(u.Path) {
		return response{}, fail(HTTP, "request destination rejected")
	}
	var body io.Reader
	if values != nil {
		body = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return response{}, fail(HTTP, "invalid request")
	}
	req.Header.Set("User-Agent", "powerchute-exporter")
	if values != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", c.base.String())
	}
	if ajax {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Referer", c.base.String()+"/datalog")
	} else if method == http.MethodPost {
		req.Header.Set("Referer", c.base.String()+"/logon")
	}
	r, err := c.http.Do(req)
	if err != nil {
		return response{}, classifyNetwork(err)
	}
	defer r.Body.Close()
	if r.StatusCode == 401 || r.StatusCode == 403 {
		return response{}, fail(Authentication, "protected resource denied")
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return response{}, fail(HTTP, "unexpected HTTP status")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, c.cfg.Exporter.MaxResponseBytes+1))
	if err != nil {
		return response{}, classifyNetwork(err)
	}
	if int64(len(b)) > c.cfg.Exporter.MaxResponseBytes {
		return response{}, fail(HTTP, "response exceeds size limit")
	}
	return response{b, r.Request.URL}, nil
}

func loginForm(b []byte) (*goquery.Selection, bool) {
	d, err := document(b)
	if err != nil {
		return nil, false
	}
	forms := d.Find("form").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return s.Find("input[name=j_password]").Length() > 0 || s.Is("#loginForm")
	})
	if forms.Length() == 0 {
		return nil, false
	}
	return forms, true
}

func tokens(form *goquery.Selection) (url.Values, error) {
	v := url.Values{}
	for _, key := range []string{"formtoken", "formtokenid"} {
		s := form.Find("input[name=" + key + "]")
		value, ok := s.Attr("value")
		if s.Length() != 1 || !ok || value == "" {
			return nil, fail(Parse, "missing current form token")
		}
		v.Set(key, value)
	}
	return v, nil
}

type session struct {
	c               *Client
	reauthenticated bool
}

func (c *Client) withSession(ctx context.Context, fn func(*session) error) error {
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return fail(Network, "cycle deadline exceeded while waiting")
	}
	if ctx.Err() != nil {
		return fail(Network, "cycle deadline exceeded")
	}
	return fn(&session{c: c})
}

func (s *session) read(ctx context.Context, path string, values url.Values) (response, error) {
	u := s.c.base.ResolveReference(&url.URL{Path: path})
	method := http.MethodGet
	if values != nil {
		method = http.MethodPost
	}
	for attempt := 0; attempt < 2; attempt++ {
		r, err := s.c.request(ctx, method, u, values, path == "/ajaxdatalog")
		if err == nil {
			if _, isLogin := loginForm(r.body); !isLogin {
				return r, nil
			}
			err = fail(Authentication, "login form returned for protected resource")
		}
		if ErrorReason(err) != Authentication || s.reauthenticated {
			return response{}, err
		}
		s.reauthenticated = true
		if err = s.c.authenticate(ctx); err != nil {
			return response{}, err
		}
	}
	return response{}, fail(Authentication, "session could not be restored")
}

func (c *Client) authenticate(ctx context.Context) (err error) {
	if c.Now().Before(c.nextAuth) {
		return fail(Authentication, "authentication cooldown is active")
	}
	attempted := false
	defer func() {
		if attempted && c.AuthResult != nil {
			c.AuthResult(err == nil)
		}
		if err != nil {
			c.nextAuth = c.Now().Add(time.Duration(c.cfg.PowerChute.AuthRetrySeconds) * time.Second)
		} else {
			c.nextAuth = time.Time{}
		}
	}()
	u := c.base.ResolveReference(&url.URL{Path: "/status"})
	r, err := c.request(ctx, http.MethodGet, u, nil, false)
	if err != nil {
		return err
	}
	form, found := loginForm(r.body)
	if found {
		if form.Length() != 1 {
			return fail(Authentication, "ambiguous login form")
		}
		action, _ := form.Attr("action")
		endpoint, err := c.formAction(r.url, action, "/j_security_check")
		if err != nil {
			return err
		}
		v, err := tokens(form)
		if err != nil {
			return err
		}
		v.Set("j_username", c.creds.Username)
		v.Set("j_password", c.creds.Password)
		v.Set("login", "Log On")
		attempted = true
		r, err = c.request(ctx, http.MethodPost, endpoint, v, false)
		if err != nil {
			return err
		}
		if _, found := loginForm(r.body); found {
			return fail(Authentication, "login rejected (credentials or another active session)")
		}
	}
	// Verify an actual protected page; a redirect or HTTP 200 is not proof of login.
	r, err = c.request(ctx, http.MethodGet, c.base.ResolveReference(&url.URL{Path: "/status"}), nil, false)
	if err != nil {
		return err
	}
	if _, found := loginForm(r.body); found {
		return fail(Authentication, "session verification failed")
	}
	d, err := document(r.body)
	if err != nil {
		return err
	}
	if d.Find("#logOffLink").Length() != 1 {
		return fail(Authentication, "expected authenticated marker missing")
	}
	if attempted {
		c.ownSession = true
	}
	return nil
}

// Logout releases this client's session, important on agents that allow only one login.
// It neither logs in nor retries if the session has already expired.
func (c *Client) Logout(ctx context.Context) error {
	return c.withSession(ctx, func(_ *session) error {
		if !c.ownSession {
			return nil
		}
		_, err := c.request(ctx, http.MethodGet, c.base.ResolveReference(&url.URL{Path: "/logoff"}), nil, false)
		if err != nil && ErrorReason(err) != Authentication {
			return err
		}
		c.ownSession = false
		jar, _ := cookiejar.New(nil)
		c.http.Jar = jar
		return nil
	})
}

func (c *Client) CollectEnergy(ctx context.Context) (EnergySnapshot, error) {
	var snapshot EnergySnapshot
	err := c.withSession(ctx, func(s *session) error {
		r, err := s.read(ctx, "/energyreporting", nil)
		if err != nil {
			return err
		}
		cfg := c.cfg.Energy
		current, err := ParseEnergy(r.body, cfg, c.loc)
		if err != nil {
			return err
		}
		if cfg.PeriodMode == "existing" {
			snapshot = current
			return nil
		}
		from, to := cfg.StartDate, cfg.EndDate
		if cfg.PeriodMode == "fixed_start_to_today" {
			to = c.Now().In(c.loc).Format("2006-01-02")
		}
		if to < from {
			return fail(Period, "requested report end precedes start")
		}
		if current.From != from || current.To != to {
			d, _ := document(r.body)
			form := d.Find(cfg.Selectors.Form)
			if form.Length() != 1 {
				return fail(Period, "missing energy period form")
			}
			action, _ := form.Attr("action")
			endpoint, err := c.formAction(r.url, action, "/setenergyreporting")
			if err != nil {
				return fail(Period, "energy form action rejected")
			}
			v, err := tokens(form)
			if err != nil {
				return err
			}
			layout, _ := DateLayout(cfg.PostDateFormat)
			start, _ := time.Parse("2006-01-02", from)
			end, _ := time.Parse("2006-01-02", to)
			v.Set("startdate", start.Format(layout))
			v.Set("enddate", end.Format(layout))
			if cfg.ApplyValue != "" {
				v.Set("apply", cfg.ApplyValue)
			}
			// This changes server state: never automatically replay the POST on auth failure.
			post, err := c.request(ctx, http.MethodPost, endpoint, v, false)
			if err != nil {
				return err
			}
			if _, found := loginForm(post.body); found {
				return fail(Authentication, "session expired during period update")
			}
			r, err = s.read(ctx, "/energyreporting", nil)
			if err != nil {
				return err
			}
			current, err = ParseEnergy(r.body, cfg, c.loc)
			if err != nil {
				return err
			}
			if current.From != from || current.To != to {
				return fail(Period, "server did not accept requested report period")
			}
		}
		snapshot = current
		return nil
	})
	return snapshot, err
}

func logValues(offset, size, echo int) url.Values {
	v := url.Values{"sEcho": {strconv.Itoa(echo)}, "iColumns": {"6"}, "sColumns": {""}, "iDisplayStart": {strconv.Itoa(offset)}, "iDisplayLength": {strconv.Itoa(size)}}
	for i, name := range []string{"date", "time", "linevolt", "battvolt", "battcap", "upsload"} {
		v.Set("mDataProp_"+strconv.Itoa(i), name)
	}
	return v
}

func pageHash(p logPage) [32]byte { b, _ := json.Marshal(p); return sha256.Sum256(b) }

func (c *Client) CollectDataLog(ctx context.Context) (DataSnapshot, error) {
	var snapshot DataSnapshot
	err := c.withSession(ctx, func(s *session) error {
		cfg := c.cfg.DataLog
		if cfg.InitializePage {
			if _, err := s.read(ctx, "/datalog", nil); err != nil {
				return err
			}
		}
		var newest map[string]json.RawMessage
		var newestTime, previousTime time.Time
		var firstHash [32]byte
		seen := map[[32]byte]bool{}
		total, offset, pages := -1, 0, 0
		for ; pages < cfg.MaxPages; pages++ {
			r, err := s.read(ctx, "/ajaxdatalog", logValues(offset, cfg.PageSize, pages+1))
			if err != nil {
				return err
			}
			p, err := parseLogPage(r.body, cfg)
			if err != nil {
				return err
			}
			if len(p.Rows) > cfg.PageSize {
				return fail(Parse, "datalog page exceeds requested size")
			}
			if pages == 0 {
				total = p.Total
				firstHash = pageHash(p)
			} else if p.Total != total {
				return fail(Timestamp, "datalog changed during pagination")
			}
			if len(p.Rows) == 0 && pages == 0 {
				return fail(Empty, "datalog is empty")
			}
			if total >= 0 && offset+len(p.Rows) > total {
				return fail(Parse, "datalog row count exceeds total")
			}
			hash := pageHash(p)
			if len(p.Rows) > 0 && seen[hash] {
				return fail(Parse, "datalog pagination repeated a page")
			}
			seen[hash] = true
			for _, row := range p.Rows {
				t, err := rowTimestamp(row, cfg, c.loc)
				if err != nil {
					return err
				}
				if cfg.Ordering == "newest_first" && !previousTime.IsZero() && t.After(previousTime) {
					return fail(Timestamp, "datalog is not ordered newest first")
				}
				previousTime = t
				if newest == nil || t.After(newestTime) {
					newest = row
					newestTime = t
				}
			}
			offset += len(p.Rows)
			if cfg.Ordering == "newest_first" {
				break
			}
			if len(p.Rows) < cfg.PageSize || (total >= 0 && offset == total) {
				if total >= 0 && offset != total {
					return fail(Parse, "incomplete datalog pagination")
				}
				break
			}
		}
		if pages == cfg.MaxPages {
			return fail(Timestamp, "latest datalog record unproven within page limit")
		}
		if cfg.Ordering == "scan" && pages > 0 {
			r, err := s.read(ctx, "/ajaxdatalog", logValues(0, cfg.PageSize, pages+2))
			if err != nil {
				return err
			}
			p, err := parseLogPage(r.body, cfg)
			if err != nil {
				return err
			}
			if pageHash(p) != firstHash {
				return fail(Timestamp, "datalog changed during pagination")
			}
		}
		var err error
		snapshot, err = parseLogRow(newest, cfg, c.loc)
		return err
	})
	return snapshot, err
}
