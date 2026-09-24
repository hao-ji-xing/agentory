package cli

import (
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hao-ji-xing/agentory/internal/model"
)

// flagSet wraps flag.FlagSet with short/long aliases and interspersed
// positional arguments ("agentory 发票 -p erp" and "agentory -p erp 发票"
// behave the same).
type flagSet struct {
	*flag.FlagSet
	usage []string
}

func newFlagSet(name string) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return &flagSet{FlagSet: fs}
}

func names(short, long string) string {
	if short == "" {
		return "    --" + long
	}
	return "-" + short + ", --" + long
}

func (f *flagSet) str(p *string, short, long, def, arg, help string) {
	*p = def
	if short != "" {
		f.StringVar(p, short, def, help)
	}
	f.StringVar(p, long, def, help)
	f.usage = append(f.usage, fmt.Sprintf("  %-28s %s", names(short, long)+" "+arg, help))
}

func (f *flagSet) int(p *int, short, long string, def int, arg, help string) {
	*p = def
	if short != "" {
		f.IntVar(p, short, def, help)
	}
	f.IntVar(p, long, def, help)
	f.usage = append(f.usage, fmt.Sprintf("  %-28s %s", names(short, long)+" "+arg, help))
}

func (f *flagSet) bool(p *bool, short, long, help string) {
	if short != "" {
		f.BoolVar(p, short, false, help)
	}
	f.BoolVar(p, long, false, help)
	f.usage = append(f.usage, fmt.Sprintf("  %-28s %s", names(short, long), help))
}

func (f *flagSet) help() string { return strings.Join(f.usage, "\n") }

// parse parses flags anywhere in args and returns the positional arguments.
// Everything after "--" is positional.
func (f *flagSet) parse(args []string) ([]string, error) {
	var pos []string
	for i, a := range args {
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			args = args[:i]
			break
		}
	}
	var rest []string
	for {
		if err := f.Parse(args); err != nil {
			return nil, err
		}
		args = f.Args()
		if len(args) == 0 {
			break
		}
		rest = append(rest, args[0])
		args = args[1:]
	}
	return append(rest, pos...), nil
}

var relTime = regexp.MustCompile(`^(\d+)(min|m|h|d|w|mo|y)$`)

// parseTime understands relative durations (30m, 12h, 7d, 2w, 3mo, 1y),
// "today", "yesterday", and absolute dates/times in local time. For an
// upper bound a bare date means the end of that day.
func parseTime(s string, now time.Time, upper bool) (time.Time, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return time.Time{}, nil
	}
	day := func(t time.Time) time.Time {
		if upper {
			return t.AddDate(0, 0, 1)
		}
		return t
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch s {
	case "today":
		return day(midnight), nil
	case "yesterday":
		return day(midnight.AddDate(0, 0, -1)), nil
	}
	if m := relTime.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "min", "m":
			return now.Add(-time.Duration(n) * time.Minute), nil
		case "h":
			return now.Add(-time.Duration(n) * time.Hour), nil
		case "d":
			return now.AddDate(0, 0, -n), nil
		case "w":
			return now.AddDate(0, 0, -7*n), nil
		case "mo":
			return now.AddDate(0, -n, 0), nil
		case "y":
			return now.AddDate(-n, 0, 0), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, now.Location()); err == nil {
		return day(t), nil
	}
	for _, layout := range []string{"2006-01-02t15:04:05", "2006-01-02t15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return t, nil
		}
	}
	if t, err := time.Parse(time.RFC3339, strings.ToUpper(s)); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (use 7d, 12h, 2w, today, 2026-09-01 …)", s)
}

func parseKinds(s string) ([]model.Kind, error) {
	var out []model.Kind
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k := model.Kind(part)
		if !model.ValidKind(k) {
			return nil, fmt.Errorf("unknown kind %q (valid: %v)", part, model.AllKinds)
		}
		out = append(out, k)
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
