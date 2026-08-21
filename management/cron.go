package management

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file implements a minimal, dependency-free five-field cron expression
// parser and the "next fire time" computation used by the Scheduler. Only the
// standard library is used (Go 1.17 compatible).
//
// A valid expression has five whitespace-separated fields:
//
//	minute  hour  day-of-month  month  day-of-week
//	0-59    0-23  1-31          1-12   0-7 (0 and 7 are Sunday)
//
// Each field accepts *, lists (a,b), ranges (a-b), steps (*/n, a-b/n) and, for
// the month and day-of-week fields, three-letter names (jan-dec, sun-sat).
// Advanced features (L, W, #, @preset aliases, seconds field) are not
// supported and produce a parse error. If both day-of-month and day-of-week
// are restricted the job runs when either matches (Vixie-cron behaviour).

var (
	monthNames = map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}
	dowNames = map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}
)

// cronField holds the allowed values of one cron field. values is a bit set
// indexed by value; star reports whether the field is unconstrained ("*").
type cronField struct {
	min    int
	max    int
	values map[int]bool
	star   bool
}

// Cron is a parsed five-field cron expression.
type Cron struct {
	minute cronField
	hour   cronField
	dom    cronField
	month  cronField
	dow    cronField
}

// ParseCron parses a five-field cron expression. It returns an error wrapping
// ErrInvalid when the expression is malformed.
func ParseCron(expr string) (*Cron, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: %w: expected 5 fields, got %d", expr, ErrInvalid, len(fields))
	}

	minute, err := parseField(fields[0], 0, 59, nil)
	if err != nil {
		return nil, fmt.Errorf("cron %q: minute: %w", expr, err)
	}
	hour, err := parseField(fields[1], 0, 23, nil)
	if err != nil {
		return nil, fmt.Errorf("cron %q: hour: %w", expr, err)
	}
	dom, err := parseField(fields[2], 1, 31, nil)
	if err != nil {
		return nil, fmt.Errorf("cron %q: day-of-month: %w", expr, err)
	}
	month, err := parseField(fields[3], 1, 12, monthNames)
	if err != nil {
		return nil, fmt.Errorf("cron %q: month: %w", expr, err)
	}
	dow, err := parseField(fields[4], 0, 7, dowNames)
	if err != nil {
		return nil, fmt.Errorf("cron %q: day-of-week: %w", expr, err)
	}

	// Normalize day-of-week 7 (Sunday, Vixie convention) onto 0.
	if dow.values[7] {
		dow.values[0] = true
		delete(dow.values, 7)
	}

	return &Cron{minute: minute, hour: hour, dom: dom, month: month, dow: dow}, nil
}

// Next returns the earliest time strictly after from that matches the
// expression, in the location of from. It returns the zero time when no match
// occurs within a five-year horizon (which also covers malformed-but-parsable
// expressions that can never fire).
func (c *Cron) Next(from time.Time) time.Time {
	if from.IsZero() {
		from = time.Now()
	}
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if c.minute.values[t.Minute()] &&
			c.hour.values[t.Hour()] &&
			c.matchDay(t.Day(), int(t.Weekday())) &&
			c.month.values[int(t.Month())] {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

// matchDay implements the day-of-month / day-of-week rule: when both fields
// are restricted either match counts; otherwise only the restricted field
// applies.
func (c *Cron) matchDay(day, dow int) bool {
	if !c.dom.star && !c.dow.star {
		return c.dom.values[day] || c.dow.values[dow]
	}
	if !c.dom.star {
		return c.dom.values[day]
	}
	if !c.dow.star {
		return c.dow.values[dow]
	}
	return true
}

func parseField(tok string, min, max int, names map[string]int) (cronField, error) {
	f := cronField{min: min, max: max, values: make(map[int]bool)}
	tok = strings.TrimSpace(tok)
	parts := strings.Split(tok, ",")
	if len(parts) == 1 && strings.TrimSpace(parts[0]) == "*" {
		f.star = true
		for v := min; v <= max; v++ {
			f.values[v] = true
		}
		return f, nil
	}
	for _, part := range parts {
		if err := parseFieldPart(strings.TrimSpace(part), &f, names); err != nil {
			return f, err
		}
	}
	return f, nil
}

func parseFieldPart(part string, f *cronField, names map[string]int) error {
	step := 1
	rangePart := part
	if i := strings.IndexByte(part, '/'); i >= 0 {
		rangePart = part[:i]
		s, err := strconv.Atoi(strings.TrimSpace(part[i+1:]))
		if err != nil || s <= 0 {
			return fmt.Errorf("invalid step in %q", part)
		}
		step = s
	}

	var lo, hi int
	switch {
	case strings.TrimSpace(rangePart) == "*":
		lo, hi = f.min, f.max
	case strings.IndexByte(rangePart, '-') >= 0:
		bits := strings.SplitN(rangePart, "-", 2)
		var err error
		if lo, err = parseValue(bits[0], f.min, f.max, names); err != nil {
			return err
		}
		if hi, err = parseValue(bits[1], f.min, f.max, names); err != nil {
			return err
		}
		if lo > hi {
			return fmt.Errorf("range %q is inverted", rangePart)
		}
	default:
		v, err := parseValue(rangePart, f.min, f.max, names)
		if err != nil {
			return err
		}
		lo, hi = v, v
	}

	for v := lo; v <= hi; v += step {
		if v > f.max {
			break
		}
		f.values[v] = true
	}
	return nil
}

func parseValue(s string, min, max int, names map[string]int) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if names != nil {
		if n, ok := names[s]; ok {
			return n, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("value %d out of range [%d,%d]", v, min, max)
	}
	return v, nil
}
