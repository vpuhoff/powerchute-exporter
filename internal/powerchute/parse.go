package powerchute

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"powerchute-exporter/internal/config"
)

type EnergySnapshot struct {
	KWh, Cost, CO2 float64
	From           string `json:"from"`
	To             string `json:"to"`
}
type DataSnapshot struct {
	InputVoltage, BatteryVoltage, BatteryCharge, Load float64
	Timestamp                                         time.Time
}

// A value field may contain a currency prefix or unit suffix, but exactly one number.
var numberToken = regexp.MustCompile(`[+-]?[0-9][0-9.,\x{00a0}\x{202f} ]*`)

func ParseNumber(s string, n config.NumberFormat) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty number")
	}
	// Reject scientific notation and HTML instead of silently accepting a prefix.
	if strings.ContainsAny(s, "<>") {
		return 0, errors.New("invalid number")
	}
	matches := numberToken.FindAllStringIndex(s, -1)
	if len(matches) != 1 {
		return 0, errors.New("expected one number")
	}
	m := matches[0]
	for _, r := range s[:m[0]] + s[m[1]:] {
		if unicode.IsDigit(r) || r == '+' || r == '-' || r == '.' || r == ',' {
			return 0, errors.New("invalid number decoration")
		}
	}
	if m[1] < len(s) && (s[m[1]] == 'e' || s[m[1]] == 'E') && s[m[1]-1] >= '0' && s[m[1]-1] <= '9' {
		return 0, errors.New("scientific notation is unsupported")
	}
	v := strings.TrimSpace(s[m[0]:m[1]])
	v = strings.ReplaceAll(strings.ReplaceAll(v, "\u00a0", " "), "\u202f", " ")
	sign := ""
	if v[0] == '-' || v[0] == '+' {
		sign = v[:1]
		v = v[1:]
	}
	parts := strings.Split(v, n.DecimalSeparator)
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
		return 0, errors.New("invalid decimal number")
	}
	integer := parts[0]
	// Whitespace grouping is accepted in addition to the configured grouping sign.
	group := n.ThousandsSeparator
	if strings.Contains(integer, " ") {
		if group != "" && group != " " && strings.Contains(integer, group) {
			return 0, errors.New("mixed grouping separators")
		}
		group = " "
	}
	if group != "" && strings.Contains(integer, group) {
		groups := strings.Split(integer, group)
		if len(groups[0]) < 1 || len(groups[0]) > 3 {
			return 0, errors.New("invalid grouping")
		}
		for _, g := range groups[1:] {
			if len(g) != 3 {
				return 0, errors.New("invalid grouping")
			}
		}
		integer = strings.Join(groups, "")
	}
	for _, p := range append([]string{integer}, parts[1:]...) {
		for _, r := range p {
			if r < '0' || r > '9' {
				return 0, errors.New("invalid digits")
			}
		}
	}
	normalized := sign + integer
	if len(parts) == 2 {
		normalized += "." + parts[1]
	}
	f, err := strconv.ParseFloat(normalized, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("non-finite number")
	}
	return f, nil
}

// Compile formats explicitly; neither the machine locale nor its timezone is consulted.
func DateLayout(format string) (string, error) {
	var out strings.Builder
	seen := map[byte]bool{}
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			if (format[i] >= 'a' && format[i] <= 'z') || (format[i] >= 'A' && format[i] <= 'Z') || (format[i] >= '0' && format[i] <= '9') {
				return "", errors.New("use strftime directives, not Go layouts")
			}
			out.WriteByte(format[i])
			continue
		}
		i++
		if i >= len(format) || seen[format[i]] {
			return "", errors.New("invalid or duplicate date directive")
		}
		seen[format[i]] = true
		switch format[i] {
		case 'Y':
			out.WriteString("2006")
		case 'y':
			out.WriteString("06")
		case 'm':
			out.WriteString("01")
		case 'd':
			out.WriteString("02")
		case 'H':
			out.WriteString("15")
		case 'M':
			out.WriteString("04")
		case 'S':
			out.WriteString("05")
		default:
			return "", errors.New("unsupported date directive")
		}
	}
	if out.Len() == 0 {
		return "", errors.New("empty date format")
	}
	return out.String(), nil
}

func ValidateFormats(c config.Config) error {
	formats := []string{}
	if c.Energy.Enabled {
		formats = append(formats, c.Energy.DateFormat)
		if c.Energy.PeriodMode != "existing" {
			formats = append(formats, c.Energy.PostDateFormat)
		}
		for _, s := range []string{c.Energy.Selectors.Energy, c.Energy.Selectors.Cost, c.Energy.Selectors.CO2, c.Energy.Selectors.StartDate, c.Energy.Selectors.EndDate, c.Energy.Selectors.Form} {
			if s == "" {
				return errors.New("energy selectors must be nonempty")
			}
			if _, err := cascadia.Compile(s); err != nil {
				return errors.New("invalid energy CSS selector")
			}
		}
	}
	for _, f := range formats {
		if !strings.Contains(f, "%Y") || !strings.Contains(f, "%m") || !strings.Contains(f, "%d") || strings.Contains(f, "%y") {
			return errors.New("energy date formats require %Y, %m and %d")
		}
		if _, err := DateLayout(f); err != nil {
			return err
		}
	}
	if c.DataLog.Enabled {
		d := c.DataLog
		if !strings.Contains(d.DateFormat, "%m") || !strings.Contains(d.DateFormat, "%d") || (strings.Contains(d.DateFormat, "%Y") == strings.Contains(d.DateFormat, "%y")) {
			return errors.New("datalog.date_format requires a month, day and exactly one year directive")
		}
		if !strings.Contains(d.TimeFormat, "%H") || !strings.Contains(d.TimeFormat, "%M") || !strings.Contains(d.TimeFormat, "%S") {
			return errors.New("datalog.time_format requires %H, %M and %S")
		}
		if _, err := DateLayout(d.DateFormat + " " + d.TimeFormat); err != nil {
			return err
		}
	}
	return nil
}

func ParseTimestamp(date, clock string, c config.DataLog, loc *time.Location) (time.Time, error) {
	format := c.DateFormat + " " + c.TimeFormat
	layout, err := DateLayout(format)
	if err != nil {
		return time.Time{}, fail(Timestamp, "invalid date format")
	}
	input, err := normalizeDateTime(format, strings.TrimSpace(date)+" "+strings.TrimSpace(clock))
	if err != nil {
		return time.Time{}, fail(Timestamp, "invalid local timestamp")
	}
	t, err := time.ParseInLocation(layout, input, loc)
	if err != nil || t.Format(layout) != input {
		return time.Time{}, fail(Timestamp, "invalid local timestamp")
	}
	if strings.Contains(c.DateFormat, "%y") {
		y := t.Year() % 100
		start := c.TwoDigitYearStart
		expanded := start/100*100 + y
		if expanded < start {
			expanded += 100
		}
		t = time.Date(expanded, t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
		if t.Format(layout) != input {
			return time.Time{}, fail(Timestamp, "invalid local timestamp")
		}
	}
	// ParseInLocation chooses one side of a DST fold. Reject both sides as ambiguous.
	_, offset := t.Zone()
	for _, delta := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		_, other := t.Add(delta).Zone()
		if other != offset && t.Add(time.Duration(offset-other)*time.Second).Format(layout) == input {
			return time.Time{}, fail(Timestamp, "ambiguous local timestamp")
		}
	}
	return t, nil
}

var datePatterns sync.Map // format -> immutable compiled regexp

// Real Data Log rows use unpadded days (10/9/26). Numeric directives accept
// one or two digits, as strptime does; year width and field order stay explicit.
func normalizeDateTime(format, input string) (string, error) {
	value, ok := datePatterns.Load(format)
	if !ok {
		var pattern strings.Builder
		pattern.WriteByte('^')
		for i := 0; i < len(format); i++ {
			if format[i] != '%' {
				pattern.WriteString(regexp.QuoteMeta(format[i : i+1]))
				continue
			}
			i++
			if i >= len(format) {
				return "", errors.New("invalid format")
			}
			switch format[i] {
			case 'Y':
				pattern.WriteString(`([0-9]{4})`)
			case 'y':
				pattern.WriteString(`([0-9]{2})`)
			default:
				pattern.WriteString(`([0-9]{1,2})`)
			}
		}
		pattern.WriteByte('$')
		value, _ = datePatterns.LoadOrStore(format, regexp.MustCompile(pattern.String()))
	}
	groups := value.(*regexp.Regexp).FindStringSubmatch(input)
	if groups == nil {
		return "", errors.New("timestamp format mismatch")
	}
	var normalized strings.Builder
	group := 1
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			normalized.WriteByte(format[i])
			continue
		}
		i++
		raw := groups[group]
		group++
		if format[i] == 'Y' || format[i] == 'y' {
			normalized.WriteString(raw)
		} else {
			n, _ := strconv.Atoi(raw)
			fmt.Fprintf(&normalized, "%02d", n)
		}
	}
	return normalized.String(), nil
}

func parseDate(raw, format string, loc *time.Location) (time.Time, error) {
	layout, err := DateLayout(format)
	if err != nil {
		return time.Time{}, err
	}
	input, err := normalizeDateTime(format, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.ParseInLocation(layout, input, loc)
	if err != nil || t.Format(layout) != input {
		return time.Time{}, errors.New("invalid report date")
	}
	return t, nil
}

func document(b []byte) (*goquery.Document, error) {
	d, err := goquery.NewDocumentFromReader(bytes.NewReader(b))
	if err != nil {
		return nil, fail(Parse, "invalid HTML")
	}
	return d, nil
}

func field(d *goquery.Document, selector string) (string, error) {
	s := d.Find(selector)
	if s.Length() != 1 {
		return "", fail(Parse, "expected exactly one HTML value field")
	}
	if s.Is("input") {
		v, ok := s.Attr("value")
		if !ok {
			return "", fail(Parse, "HTML input has no value")
		}
		return v, nil
	}
	return s.Text(), nil
}

func ParseEnergy(b []byte, c config.Energy, loc *time.Location) (EnergySnapshot, error) {
	d, err := document(b)
	if err != nil {
		return EnergySnapshot{}, err
	}
	s := EnergySnapshot{}
	for _, v := range []struct {
		selector string
		target   *float64
	}{{c.Selectors.Energy, &s.KWh}, {c.Selectors.Cost, &s.Cost}, {c.Selectors.CO2, &s.CO2}} {
		raw, err := field(d, v.selector)
		if err != nil {
			return s, err
		}
		n, err := ParseNumber(raw, c.NumberFormat)
		if err != nil {
			return s, fail(Parse, "invalid energy numeric field")
		}
		*v.target = n
	}
	if s.KWh < 0 || s.CO2 < 0 {
		return s, fail(Parse, "negative energy or CO2")
	}
	dates := []*string{&s.From, &s.To}
	for i, selector := range []string{c.Selectors.StartDate, c.Selectors.EndDate} {
		raw, err := field(d, selector)
		if err != nil {
			return s, fail(Period, "missing report dates")
		}
		t, err := parseDate(raw, c.DateFormat, loc)
		if err != nil {
			return s, fail(Period, "invalid report dates")
		}
		*dates[i] = t.Format("2006-01-02")
	}
	if s.From > s.To {
		return s, fail(Period, "report end precedes start")
	}
	return s, nil
}

type logPage struct {
	Rows  []map[string]json.RawMessage
	Total int
}

func jsonPointer(root any, path string) (any, error) {
	if path == "" {
		return root, nil
	}
	v := root
	for _, part := range strings.Split(path[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch obj := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = obj[part]
			if !ok {
				return nil, errors.New("missing JSON field")
			}
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(obj) {
				return nil, errors.New("invalid JSON index")
			}
			v = obj[i]
		default:
			return nil, errors.New("invalid JSON path")
		}
	}
	return v, nil
}

func parseLogPage(b []byte, c config.DataLog) (logPage, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var root any
	if err := d.Decode(&root); err != nil {
		return logPage{}, fail(Parse, "invalid datalog JSON")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return logPage{}, fail(Parse, "trailing datalog JSON")
	}
	rows, err := jsonPointer(root, c.RowsPath)
	if err != nil {
		return logPage{}, fail(Parse, "datalog rows path is missing")
	}
	array, ok := rows.([]any)
	if !ok {
		return logPage{}, fail(Parse, "datalog rows must be an array")
	}
	p := logPage{Total: -1}
	if c.TotalPath != "" {
		v, err := jsonPointer(root, c.TotalPath)
		if err != nil {
			return p, fail(Parse, "datalog total path is missing")
		}
		n, err := strconv.Atoi(fmt.Sprint(v))
		if err != nil || n < 0 {
			return p, fail(Parse, "invalid datalog total")
		}
		p.Total = n
	}
	for _, r := range array {
		obj, ok := r.(map[string]any)
		if !ok {
			return p, fail(Parse, "datalog row must be an object")
		}
		row := map[string]json.RawMessage{}
		for k, v := range obj {
			raw, _ := json.Marshal(v)
			row[k] = raw
		}
		p.Rows = append(p.Rows, row)
	}
	return p, nil
}

func rowValue(row map[string]json.RawMessage, key string) (string, error) {
	raw, ok := row[key]
	if !ok || bytes.Equal(raw, []byte("null")) {
		return "", fail(Parse, "missing datalog value")
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return "", fail(Parse, "empty datalog value")
		}
		return s, nil
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil && len(n.String()) > 0 {
		return n.String(), nil
	}
	return "", fail(Parse, "unsupported datalog value type")
}

func rowTimestamp(row map[string]json.RawMessage, c config.DataLog, loc *time.Location) (time.Time, error) {
	d, err := rowValue(row, "date")
	if err != nil {
		return time.Time{}, fail(Timestamp, "missing datalog date")
	}
	t, err := rowValue(row, "time")
	if err != nil {
		return time.Time{}, fail(Timestamp, "missing datalog time")
	}
	return ParseTimestamp(d, t, c, loc)
}

func parseLogRow(row map[string]json.RawMessage, c config.DataLog, loc *time.Location) (DataSnapshot, error) {
	s := DataSnapshot{}
	var err error
	s.Timestamp, err = rowTimestamp(row, c, loc)
	if err != nil {
		return s, err
	}
	for _, f := range []struct {
		key    string
		target *float64
	}{{"linevolt", &s.InputVoltage}, {"battvolt", &s.BatteryVoltage}, {"battcap", &s.BatteryCharge}, {"upsload", &s.Load}} {
		raw, err := rowValue(row, f.key)
		if err != nil {
			return s, err
		}
		// JSON numeric scalars always use a dot, independent of the display locale.
		nf := c.NumberFormat
		if row[f.key][0] != '"' {
			nf = config.NumberFormat{DecimalSeparator: "."}
		}
		*f.target, err = ParseNumber(raw, nf)
		if err != nil || *f.target < 0 {
			return s, fail(Parse, "invalid datalog numeric field")
		}
	}
	if s.BatteryCharge > 100 {
		return s, fail(Parse, "battery charge exceeds 100 percent")
	}
	s.BatteryCharge /= 100
	s.Load /= 100
	return s, nil
}
