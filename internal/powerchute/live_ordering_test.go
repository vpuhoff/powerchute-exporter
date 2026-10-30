package powerchute

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	"powerchute-exporter/internal/config"
)

// These tests only read /ajaxdatalog. Stop another exporter using the same
// PowerChute account first: the researched agent permits a single session.
func orderingClient(t *testing.T) (*Client, config.Config) {
	t.Helper()
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
	creds, err := cfg.PowerChute.Credentials()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg, creds)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Logout(ctx); err != nil {
			t.Error(err)
		}
		c.Close()
	})
	return c, cfg
}

func orderingPage(ctx context.Context, c *Client, cfg config.Config, offset, size int, extra url.Values) (logPage, error) {
	var page logPage
	err := c.withSession(ctx, func(s *session) error {
		values := logValues(offset, size, 1)
		for key, vs := range extra {
			values[key] = vs
		}
		r, err := s.read(ctx, "/ajaxdatalog", values)
		if err != nil {
			return err
		}
		page, err = parseLogPage(r.body, cfg.DataLog)
		return err
	})
	return page, err
}

func orderingTimes(t *testing.T, c *Client, cfg config.Config, rows []map[string]json.RawMessage) []string {
	t.Helper()
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		timestamp, err := rowTimestamp(row, cfg.DataLog, c.loc)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, timestamp.Format(time.RFC3339))
	}
	return out
}

type orderingReport struct {
	CheckedAt           string            `json:"checked_at"`
	Total               int               `json:"total"`
	RowsChecked         int               `json:"rows_checked"`
	Pages               int               `json:"pages"`
	Descending          bool              `json:"descending"`
	DuplicateTimestamps int               `json:"duplicate_timestamps"`
	Latest              string            `json:"latest"`
	Oldest              string            `json:"oldest"`
	FirstPageTimes      []string          `json:"first_page_times"`
	PaginationChecks    int               `json:"pagination_checks"`
	SortingParameters   map[string]string `json:"sorting_parameters"`
}

func TestLiveDataLogOrdering(t *testing.T) {
	c, cfg := orderingClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var all []map[string]json.RawMessage
	total := -1
	pages := 0
	for offset := 0; pages < cfg.DataLog.MaxPages; {
		p, err := orderingPage(ctx, c, cfg, offset, 100, nil)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if total < 0 {
			total = p.Total
		} else if total != p.Total {
			t.Fatal("log changed during baseline; repeat when stable")
		}
		if len(p.Rows) > 100 {
			t.Fatal("server ignored requested page size")
		}
		all = append(all, p.Rows...)
		offset += len(p.Rows)
		if len(p.Rows) < 100 || offset == total {
			break
		}
	}
	if total < 1 || len(all) != total {
		t.Fatal("could not inspect the complete log")
	}
	times := orderingTimes(t, c, cfg, all)
	report := orderingReport{CheckedAt: time.Now().UTC().Format(time.RFC3339), Total: total, RowsChecked: len(all), Pages: pages, Descending: true, Latest: times[0], Oldest: times[len(times)-1], FirstPageTimes: times[:min(10, len(times))], SortingParameters: map[string]string{}}
	var previous time.Time
	maximum := time.Time{}
	for _, raw := range times {
		stamp, _ := time.Parse(time.RFC3339, raw)
		if !previous.IsZero() {
			if stamp.After(previous) {
				report.Descending = false
			}
			if stamp.Equal(previous) {
				report.DuplicateTimestamps++
			}
		}
		if stamp.After(maximum) {
			maximum = stamp
		}
		previous = stamp
	}
	if !report.Descending || report.Latest != maximum.Format(time.RFC3339) {
		t.Fatal("baseline is not globally newest first")
	}
	for _, check := range []struct{ offset, size int }{{0, 1}, {0, 10}, {1, 10}, {9, 10}, {10, 10}, {0, 25}, {25, 25}, {0, 50}, {50, 50}, {99, 10}, {100, 100}, {total - 5, 10}} {
		if check.offset < 0 || check.offset >= total {
			continue
		}
		p, err := orderingPage(ctx, c, cfg, check.offset, check.size, nil)
		if err != nil {
			t.Fatal(err)
		}
		end := min(check.offset+check.size, total)
		if p.Total != total || !reflect.DeepEqual(orderingTimes(t, c, cfg, p.Rows), times[check.offset:end]) {
			t.Fatalf("pagination differs from baseline at offset=%d length=%d", check.offset, check.size)
		}
		report.PaginationChecks++
	}
	for _, direction := range []string{"asc", "desc"} {
		extra := url.Values{"iSortingCols": {"2"}, "iSortCol_0": {"0"}, "sSortDir_0": {direction}, "iSortCol_1": {"1"}, "sSortDir_1": {direction}, "bSortable_0": {"true"}, "bSortable_1": {"true"}}
		p, err := orderingPage(ctx, c, cfg, 0, 10, extra)
		if err != nil {
			report.SortingParameters[direction] = "rejected: " + string(ErrorReason(err))
			continue
		}
		switch head := orderingTimes(t, c, cfg, p.Rows); {
		case reflect.DeepEqual(head, report.FirstPageTimes):
			report.SortingParameters[direction] = "unchanged: newest first"
		default:
			report.SortingParameters[direction] = "different order"
		}
	}
	last, err := orderingPage(ctx, c, cfg, 0, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if last.Total != total || !reflect.DeepEqual(orderingTimes(t, c, cfg, last.Rows), report.FirstPageTimes) {
		t.Fatal("baseline changed during probe; repeat when stable")
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("POWERCHUTE_ORDERING_REPORT"); path != "" {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("globally descending: rows=%d pages=%d latest=%s oldest=%s duplicates=%d pagination_checks=%d sorting=%v", report.RowsChecked, report.Pages, report.Latest, report.Oldest, report.DuplicateTimestamps, report.PaginationChecks, report.SortingParameters)
}

func TestLiveDataLogInsertionAtHead(t *testing.T) {
	c, cfg := orderingClient(t)
	path := os.Getenv("POWERCHUTE_ORDERING_REPORT")
	if path == "" {
		t.Fatal("POWERCHUTE_ORDERING_REPORT baseline is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var baseline orderingReport
	if json.Unmarshal(raw, &baseline) != nil || !baseline.Descending || len(baseline.FirstPageTimes) < 2 {
		t.Fatal("invalid baseline")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := orderingPage(ctx, c, cfg, 0, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	head := orderingTimes(t, c, cfg, p.Rows)
	if len(head) < 2 {
		t.Fatal("first page too short")
	}
	current, _ := time.Parse(time.RFC3339, head[0])
	old, _ := time.Parse(time.RFC3339, baseline.Latest)
	if !current.After(old) {
		t.Fatal("a new record has not appeared since the baseline")
	}
	shift := -1
	for i, stamp := range head {
		if stamp == baseline.Latest {
			shift = i
			break
		}
	}
	if shift < 1 {
		t.Fatal("previous newest record not found behind the new head")
	}
	if !reflect.DeepEqual(head[shift:], baseline.FirstPageTimes[:len(head)-shift]) {
		t.Fatal("insertion did not preserve the previous page order")
	}
	for i := 1; i < len(head); i++ {
		a, _ := time.Parse(time.RFC3339, head[i-1])
		b, _ := time.Parse(time.RFC3339, head[i])
		if b.After(a) {
			t.Fatal("first page is no longer descending")
		}
	}
	second, err := orderingPage(ctx, c, cfg, shift, 10-shift, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orderingTimes(t, c, cfg, second.Rows), head[shift:]) {
		t.Fatal("offset after insertion does not match the first page")
	}
	repeat, err := orderingPage(ctx, c, cfg, 0, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orderingTimes(t, c, cfg, repeat.Rows), head) {
		t.Fatal("first page changed during insertion check")
	}
	// Validate the existing production fast adapter against the directly
	// inspected new head, without changing the installed configuration.
	c.cfg.DataLog.Ordering = "newest_first"
	c.cfg.DataLog.PageSize = 10
	start := time.Now()
	snapshot, err := c.CollectDataLog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Timestamp.Format(time.RFC3339) != head[0] {
		t.Fatal("fast adapter did not select the new head")
	}
	proof := map[string]any{"checked_at": time.Now().UTC().Format(time.RFC3339), "previous_latest": baseline.Latest, "current_latest": head[0], "inserted_at_head": shift, "previous_order_preserved": true, "offset_check": true, "fast_adapter_matches": true, "fast_collection_seconds": time.Since(start).Seconds(), "total_before": baseline.Total, "total_now": p.Total}
	data, _ := json.MarshalIndent(proof, "", "  ")
	if output := os.Getenv("POWERCHUTE_INSERTION_REPORT"); output != "" {
		if err := os.WriteFile(output, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("new records inserted before previous head: count=%d previous=%s current=%s total_before=%d total_now=%d", shift, baseline.Latest, head[0], baseline.Total, p.Total)
	t.Logf("production newest_first adapter matched in %.3f seconds", time.Since(start).Seconds())
}
