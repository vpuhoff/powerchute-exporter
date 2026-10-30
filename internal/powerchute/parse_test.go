package powerchute

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"powerchute-exporter/internal/config"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testConfig() config.Config {
	c := config.Defaults()
	c.PowerChute.Timezone = "Asia/Yerevan"
	c.Energy.Currency = "RUB"
	return c
}

func TestEnergyRealFixture(t *testing.T) {
	c := testConfig()
	loc, _ := time.LoadLocation(c.PowerChute.Timezone)
	s, err := ParseEnergy(fixture(t, "energy.html"), c.Energy, loc)
	if err != nil {
		t.Fatal(err)
	}
	if s.KWh != 3482.20 || s.Cost != 34961.26 || s.CO2 != 887.96 || s.From != "2025-07-02" || s.To != "2026-10-30" {
		t.Fatalf("unexpected energy snapshot: %+v", s)
	}
	localized := strings.NewReplacer("3,482.20", "3\u202f482,20", "34,961.26", "34.961,26", "887.96", "887,96", "pyб", "€").Replace(string(fixture(t, "energy.html")))
	c.Energy.DecimalSeparator = ","
	c.Energy.ThousandsSeparator = "."
	s, err = ParseEnergy([]byte(localized), c.Energy, loc)
	if err != nil || s.Cost != 34961.26 {
		t.Fatalf("localized fixture: %v %+v", err, s)
	}
	bad := strings.Replace(localized, "887,96", "N/A", 1)
	if _, err := ParseEnergy([]byte(bad), c.Energy, loc); ErrorReason(err) != Parse || err == nil {
		t.Fatal("missing value must fail")
	}
	if _, err := ParseEnergy([]byte("<html>3482 34960 887</html>"), c.Energy, loc); err == nil {
		t.Fatal("arbitrary numbers must not be parsed")
	}
}

func TestNumbers(t *testing.T) {
	for _, tt := range []struct {
		input, decimal, group string
		want                  float64
		bad                   bool
	}{
		{"3,482.17 kWh", ".", ",", 3482.17, false}, {"pyб 34,960.99", ".", ",", 34960.99, false},
		{"1\u00a0234,56 ₽", ",", ".", 1234.56, false}, {"1\u202f234,56 €", ",", " ", 1234.56, false},
		{"0.0 V", ".", ",", 0, false}, {"-2.5", ".", ",", -2.5, false},
		{"12,34.5", ".", ",", 0, true}, {"1.234,5.6", ",", ".", 0, true}, {"1  234", ".", ",", 0, true},
		{"N/A", ".", ",", 0, true}, {"NaN", ".", ",", 0, true}, {"1e3", ".", ",", 0, true}, {"1E+3", ".", ",", 0, true},
		{"12 and 45", ".", ",", 0, true}, {"", ".", ",", 0, true}, {"<b>123</b>", ".", ",", 0, true},
	} {
		t.Run(tt.input, func(t *testing.T) {
			v, err := ParseNumber(tt.input, config.NumberFormat{DecimalSeparator: tt.decimal, ThousandsSeparator: tt.group})
			if (err != nil) != tt.bad || (!tt.bad && math.Abs(v-tt.want) > 1e-8) {
				t.Fatalf("value=%v error=%v", v, err)
			}
		})
	}
}

func TestTimestampPolicy(t *testing.T) {
	c := testConfig().DataLog
	loc, _ := time.LoadLocation("Asia/Yerevan")
	ts, err := ParseTimestamp("10/30/26", "23:37:46", c, loc)
	if err != nil {
		t.Fatal(err)
	}
	if ts.UTC().Format(time.RFC3339) != "2026-10-30T19:37:46Z" {
		t.Fatal(ts)
	}
	unpadded, err := ParseTimestamp("10/9/26", "23:57:14", c, loc)
	if err != nil || unpadded.Format("2006-01-02 15:04:05") != "2026-10-09 23:57:14" {
		t.Fatal("real unpadded day rejected", err)
	}
	for _, year := range []struct {
		raw  string
		want int
	}{{"00", 2000}, {"68", 2068}, {"69", 2069}, {"99", 2099}} {
		v, err := ParseTimestamp("01/01/"+year.raw, "00:00:00", c, loc)
		if err != nil || v.Year() != year.want {
			t.Fatalf("year %s: %v %v", year.raw, v, err)
		}
	}
	c.TwoDigitYearStart = 1970
	v, err := ParseTimestamp("01/01/69", "00:00:00", c, loc)
	if err != nil || v.Year() != 2069 {
		t.Fatal(v, err)
	}
	v, err = ParseTimestamp("01/01/70", "00:00:00", c, loc)
	if err != nil || v.Year() != 1970 {
		t.Fatal(v, err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	for _, tt := range []struct{ d, clock string }{{"11/01/26", "01:30:00"}, {"03/08/26", "02:30:00"}, {"02/30/26", "10:00:00"}, {"13/01/26", "10:00:00"}, {"10/30/26", "25:00:00"}} {
		if _, err := ParseTimestamp(tt.d, tt.clock, c, ny); err == nil || ErrorReason(err) != Timestamp {
			t.Fatalf("ambiguous/invalid timestamp accepted: %+v", tt)
		}
	}
}

func TestDataLogRealFixture(t *testing.T) {
	c := testConfig().DataLog
	loc, _ := time.LoadLocation("Asia/Yerevan")
	p, err := parseLogPage(fixture(t, "datalog.json"), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rows) != 3 || p.Total != 3 {
		t.Fatalf("unexpected page: %+v", p)
	}
	s, err := parseLogRow(p.Rows[0], c, loc)
	if err != nil {
		t.Fatal(err)
	}
	if s.InputVoltage != 239 || s.BatteryVoltage != 27.1 || s.BatteryCharge != 1 || s.Load != 0.3 {
		t.Fatal(s)
	}
	p.Rows[0]["upsload"] = json.RawMessage(`"135.5"`)
	p.Rows[0]["linevolt"] = json.RawMessage(`0`)
	s, err = parseLogRow(p.Rows[0], c, loc)
	if err != nil || s.Load != 1.355 || s.InputVoltage != 0 {
		t.Fatalf("overload or outage hidden: %+v %v", s, err)
	}
	for _, bad := range []string{`null`, `""`, `"N/A"`, `"NaN"`, `"101"`, `"-1"`, `true`} {
		p.Rows[0]["battcap"] = json.RawMessage(bad)
		if _, err := parseLogRow(p.Rows[0], c, loc); err == nil {
			t.Fatalf("invalid charge accepted: %s", bad)
		}
	}
	for _, bad := range []string{`{"data":[]}`, `{"aaData":[[1,2]],"iTotalDisplayRecords":1}`, `{"aaData":null,"iTotalDisplayRecords":0}`, `{"aaData":[],"iTotalDisplayRecords":0} {}`} {
		if _, err := parseLogPage([]byte(bad), c); err == nil {
			t.Fatal("invalid JSON schema accepted")
		}
	}
}
