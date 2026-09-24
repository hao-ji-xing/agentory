package cli

import (
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 9, 23, 15, 30, 0, 0, loc)
	cases := []struct {
		in    string
		upper bool
		want  time.Time
	}{
		{"7d", false, now.AddDate(0, 0, -7)},
		{"12h", false, now.Add(-12 * time.Hour)},
		{"30m", false, now.Add(-30 * time.Minute)},
		{"2w", false, now.AddDate(0, 0, -14)},
		{"3mo", false, now.AddDate(0, -3, 0)},
		{"today", false, time.Date(2026, 9, 23, 0, 0, 0, 0, loc)},
		{"yesterday", false, time.Date(2026, 9, 22, 0, 0, 0, 0, loc)},
		{"2026-09-01", false, time.Date(2026, 9, 1, 0, 0, 0, 0, loc)},
		{"2026-09-01", true, time.Date(2026, 9, 2, 0, 0, 0, 0, loc)}, // until includes the day
		{"2026-09-01 08:15", false, time.Date(2026, 9, 1, 8, 15, 0, 0, loc)},
		{"2026-09-01T08:15:00Z", false, time.Date(2026, 9, 1, 8, 15, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := parseTime(c.in, now, c.upper)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("parseTime(%q, upper=%v) = %v, %v; want %v", c.in, c.upper, got, err, c.want)
		}
	}
	if _, err := parseTime("last tuesday", now, false); err == nil {
		t.Error("garbage must be rejected")
	}
}

func TestFlagsInterspersed(t *testing.T) {
	for _, args := range [][]string{
		{"foo", "-p", "erp", "bar"},
		{"-p", "erp", "foo", "bar"},
		{"foo", "bar", "--project", "erp"},
	} {
		var s searchFlags
		fs := newFlagSet("t")
		s.register(fs, true, true)
		pos, err := fs.parse(args)
		if err != nil || s.project != "erp" || len(pos) != 2 || pos[0] != "foo" || pos[1] != "bar" {
			t.Errorf("%v: pos=%v project=%q err=%v", args, pos, s.project, err)
		}
	}
	var s searchFlags
	fs := newFlagSet("t")
	s.register(fs, true, true)
	pos, _ := fs.parse([]string{"-n", "5", "--", "-literal", "-p"})
	if s.limit != 5 || len(pos) != 2 || pos[0] != "-literal" {
		t.Errorf("-- terminator: pos=%v limit=%d", pos, s.limit)
	}
}
