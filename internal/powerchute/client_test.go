package powerchute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"powerchute-exporter/internal/config"
)

type rig struct {
	t              *testing.T
	server         *httptest.Server
	mu             sync.Mutex
	cookie         string
	token          int
	logins         int
	requests       int
	badPassword    bool
	rotate         bool
	alwaysExpire   bool
	energyOverride string
	redirect       string
	loginAction    string
	energyStatus   int
	rows           []map[string]json.RawMessage
	changeTotal    bool
	periodPosts    int
	acceptPeriod   bool
	active         atomic.Int32
	maxActive      atomic.Int32
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t}
	p, err := parseLogPage(fixture(t, "datalog.json"), testConfig().DataLog)
	if err != nil {
		t.Fatal(err)
	}
	r.rows = p.Rows
	energy := string(fixture(t, "energy.html"))
	login := string(fixture(t, "login.html"))
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		a := r.active.Add(1)
		defer r.active.Add(-1)
		for {
			old := r.maxActive.Load()
			if a <= old || r.maxActive.CompareAndSwap(old, a) {
				break
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.requests++
		path, _ := endpointPath(q.URL.Path)
		loginBody := func() {
			r.token++
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: fmt.Sprintf("pre-%d", r.token), Path: "/"})
			body := strings.ReplaceAll(login, "fixture-session", fmt.Sprintf("pre-%d", r.token))
			body = strings.Replace(body, `action="j_security_check"`, fmt.Sprintf(`action="j_security_check;jsessionid=pre-%d"`, r.token), 1)
			body = strings.ReplaceAll(body, "fixture-token-id", "id-current")
			body = strings.ReplaceAll(body, "fixture-token", fmt.Sprintf("token-%d", r.token))
			if r.loginAction != "" {
				body = strings.Replace(body, fmt.Sprintf("j_security_check;jsessionid=pre-%d", r.token), r.loginAction, 1)
			}
			_, _ = w.Write([]byte(body))
		}
		if path == "/logon" {
			loginBody()
			return
		}
		if path == "/j_security_check" {
			r.logins++
			_ = q.ParseForm()
			cookie, err := q.Cookie("JSESSIONID")
			if err != nil || cookie.Value != fmt.Sprintf("pre-%d", r.token) || q.Form.Get("formtoken") != fmt.Sprintf("token-%d", r.token) || q.Form.Get("formtokenid") != "id-current" {
				t.Error("fresh token or cookie missing")
				w.WriteHeader(403)
				return
			}
			if q.Form.Get("j_username") != "test-user" || q.Form.Get("j_password") != "test-password" || q.Form.Get("login") != "Log On" {
				t.Error("wrong login form fields")
			}
			if r.badPassword {
				loginBody()
				return
			}
			r.cookie = fmt.Sprintf("authenticated-%d", r.logins)
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: r.cookie, Path: "/"})
			http.Redirect(w, q, "/", 303)
			return
		}
		if path == "/" {
			http.Redirect(w, q, "/status", 302)
			return
		}
		cookie, err := q.Cookie("JSESSIONID")
		if err != nil || r.cookie == "" || cookie.Value != r.cookie {
			http.Redirect(w, q, "/logon", 302)
			return
		}
		switch path {
		case "/status":
			_, _ = w.Write([]byte(`<a id="logOffLink" href="/logoff">Log Off</a>`))
		case "/energyreporting":
			if r.redirect != "" {
				http.Redirect(w, q, r.redirect, 302)
				return
			}
			if r.alwaysExpire {
				loginBody()
				return
			}
			if r.energyStatus != 0 {
				w.WriteHeader(r.energyStatus)
				return
			}
			if r.rotate {
				r.cookie += "-rotated"
				http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: r.cookie, Path: "/"})
				r.rotate = false
			}
			if r.energyOverride != "" {
				_, _ = w.Write([]byte(r.energyOverride))
			} else {
				_, _ = w.Write([]byte(energy))
			}
		case "/setenergyreporting":
			r.periodPosts++
			_ = q.ParseForm()
			if q.Form.Get("formtoken") != "fixture-token" || q.Form.Get("formtokenid") != "fixture-token-id" || q.Form.Get("apply") != "Apply" {
				t.Error("period form must carry fresh tokens and apply")
			}
			if r.acceptPeriod {
				r.energyOverride = strings.NewReplacer("07/02/2025", q.Form.Get("startdate"), "10/30/2026", q.Form.Get("enddate")).Replace(energy)
			}
			http.Redirect(w, q, "/energyreporting", 303)
		case "/ajaxdatalog":
			_ = q.ParseForm()
			if q.Method != "POST" || q.Header.Get("X-Requested-With") != "XMLHttpRequest" || q.Header.Get("Origin") != r.server.URL || q.Header.Get("Referer") != r.server.URL+"/datalog" || q.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Error("datalog request contract violated")
			}
			if q.Form.Get("iColumns") != "6" || q.Form.Get("mDataProp_5") != "upsload" {
				t.Error("datalog columns missing")
			}
			var offset, size int
			_, _ = fmt.Sscan(q.Form.Get("iDisplayStart"), &offset)
			_, _ = fmt.Sscan(q.Form.Get("iDisplayLength"), &size)
			end := offset + size
			if end > len(r.rows) {
				end = len(r.rows)
			}
			if offset > end {
				offset = end
			}
			total := len(r.rows)
			if r.changeTotal && offset > 0 {
				total++
			}
			w.Header().Set("Content-Type", "application/json")
			rows := r.rows[offset:end]
			if rows == nil {
				rows = []map[string]json.RawMessage{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"aaData": rows, "iTotalDisplayRecords": total, "iTotalRecords": total, "sEcho": q.Form.Get("sEcho")})
		default:
			http.NotFound(w, q)
		}
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *rig) client(t *testing.T, edit func(*config.Config)) *Client {
	t.Helper()
	c := testConfig()
	c.PowerChute.BaseURL = r.server.URL
	if edit != nil {
		edit(&c)
	}
	client, err := New(c, config.Credentials{Username: "test-user", Password: "test-password"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestFreshLoginCookieRotationAndExpiry(t *testing.T) {
	r := newRig(t)
	c := r.client(t, nil)
	r.rotate = true
	if _, err := c.CollectEnergy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CollectDataLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.logins != 1 {
		t.Fatal("cookie rotation caused another login")
	}
	r.cookie = "expired"
	if _, err := c.CollectEnergy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.logins != 2 {
		t.Fatal("session was not restored exactly once")
	}
}

func TestBadPasswordCooldownAndNoSecretErrors(t *testing.T) {
	r := newRig(t)
	r.badPassword = true
	c := r.client(t, nil)
	now := time.Now()
	c.Now = func() time.Time { return now }
	var failed int
	c.AuthResult = func(ok bool) {
		if ok {
			t.Error("bad password counted as success")
		}
		failed++
	}
	for i := 0; i < 2; i++ {
		_, err := c.CollectEnergy(context.Background())
		if err == nil || ErrorReason(err) != Authentication || strings.Contains(err.Error(), "test-password") {
			t.Fatal("authentication failure missing or leaked")
		}
	}
	if r.logins != 1 || failed != 1 {
		t.Fatal("bad password retried during cooldown")
	}
	now = now.Add(61 * time.Second)
	_, _ = c.CollectEnergy(context.Background())
	if r.logins != 2 {
		t.Fatal("cooldown never expired")
	}
}

func TestAtMostOneReauthenticationPerCycle(t *testing.T) {
	r := newRig(t)
	r.alwaysExpire = true
	c := r.client(t, nil)
	_, err := c.CollectEnergy(context.Background())
	if err == nil || ErrorReason(err) != Authentication || r.logins != 1 {
		t.Fatalf("unbounded login retry: %v, logins=%d", err, r.logins)
	}
}

func TestPaginationProvesLatest(t *testing.T) {
	r := newRig(t)
	r.rows[0], r.rows[2] = r.rows[2], r.rows[0]
	c := r.client(t, func(c *config.Config) { c.DataLog.PageSize = 1 })
	s, err := c.CollectDataLog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.InputVoltage != 239 || s.Timestamp.Hour() != 23 {
		t.Fatalf("first row was mistaken for latest: %+v", s)
	}
	c = r.client(t, func(c *config.Config) { c.DataLog.PageSize = 1; c.DataLog.MaxPages = 1 })
	_, err = c.CollectDataLog(context.Background())
	if err == nil || ErrorReason(err) != Timestamp {
		t.Fatal("partial scan claimed latest")
	}
}

func TestPaginationMutationEmptyAndOrdering(t *testing.T) {
	for _, mode := range []string{"mutation", "empty", "ordering"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t)
			switch mode {
			case "mutation":
				r.changeTotal = true
			case "empty":
				r.rows = nil
			case "ordering":
				r.rows[0], r.rows[2] = r.rows[2], r.rows[0]
			}
			c := r.client(t, func(c *config.Config) {
				if mode == "ordering" {
					c.DataLog.Ordering = "newest_first"
				} else {
					c.DataLog.PageSize = 1
				}
			})
			_, err := c.CollectDataLog(context.Background())
			want := Timestamp
			if mode == "empty" {
				want = Empty
			}
			if err == nil || ErrorReason(err) != want {
				t.Fatalf("want %s, got %v", want, err)
			}
		})
	}
}

func TestClientSerializesCycles(t *testing.T) {
	r := newRig(t)
	c := r.client(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = c.CollectEnergy(context.Background())
			} else {
				_, err = c.CollectDataLog(context.Background())
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if r.logins != 1 || r.maxActive.Load() != 1 {
		t.Fatalf("parallel session or request: logins=%d max=%d", r.logins, r.maxActive.Load())
	}
}

func TestCrossOriginAndFormActions(t *testing.T) {
	var received atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer other.Close()
	r := newRig(t)
	r.redirect = other.URL + "/status"
	c := r.client(t, nil)
	_, err := c.CollectEnergy(context.Background())
	if err == nil || received.Load() != 0 {
		t.Fatal("cross-origin redirect followed")
	}
	for _, action := range []string{other.URL + "/j_security_check", "/shutdown", "/j_security_check?secret=value", "/j_security_check;evil=1"} {
		r := newRig(t)
		r.loginAction = action
		c := r.client(t, nil)
		_, err := c.CollectEnergy(context.Background())
		if err == nil || r.logins != 0 || received.Load() != 0 {
			t.Fatal("unsafe form action used")
		}
	}
	base, _ := url.Parse("https://localhost:6547")
	for _, raw := range []string{"https://localhost:6548/status", "http://localhost:6547/status", "https://user@localhost:6547/status"} {
		u, _ := url.Parse(raw)
		if sameOrigin(base, u) {
			t.Fatal("unsafe origin accepted")
		}
	}
}

func TestResponseLimitHTTPAndCancellation(t *testing.T) {
	r := newRig(t)
	c := r.client(t, nil)
	if _, err := c.CollectEnergy(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.energyOverride = strings.Repeat("x", int(c.cfg.Exporter.MaxResponseBytes)+1)
	if _, err := c.CollectEnergy(context.Background()); err == nil || ErrorReason(err) != HTTP {
		t.Fatal("oversized response accepted")
	}
	r.energyOverride = ""
	r.energyStatus = 500
	if _, err := c.CollectEnergy(context.Background()); err == nil || ErrorReason(err) != HTTP {
		t.Fatal("HTTP failure accepted")
	}
	c.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.CollectEnergy(ctx); err == nil || ErrorReason(err) != Network {
		t.Fatal("waiting ignored cycle deadline")
	}
	<-c.gate
}

func TestFixedPeriodVerificationAndNoRepeatedPost(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(fmt.Sprint(accept), func(t *testing.T) {
			r := newRig(t)
			r.acceptPeriod = accept
			c := r.client(t, func(c *config.Config) {
				c.Energy.PeriodMode = "fixed_range"
				c.Energy.AllowPeriodUpdate = true
				c.Energy.PostDateFormat = "%m/%d/%Y"
				c.Energy.StartDate = "2026-01-01"
				c.Energy.EndDate = "2026-10-30"
			})
			_, err := c.CollectEnergy(context.Background())
			if !accept {
				if err == nil || ErrorReason(err) != Period {
					t.Fatal("rejected report period accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.CollectEnergy(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r.periodPosts != 1 {
				t.Fatal("unchanged period was posted again")
			}
		})
	}
}
