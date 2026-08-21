package management

import (
	"testing"
	"time"
)

func TestCronEveryMinute(t *testing.T) {
	c, err := ParseCron("* * * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2024, 1, 2, 3, 4, 30, 0, time.UTC)
	next := c.Next(from)
	want := time.Date(2024, 1, 2, 3, 5, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestCronDailyAtNine(t *testing.T) {
	c, err := ParseCron("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 8:00 today -> 9:00 today
	next := c.Next(time.Date(2024, 1, 2, 8, 0, 0, 0, time.UTC))
	if want := time.Date(2024, 1, 2, 9, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
	// 10:00 today -> 9:00 tomorrow
	next = c.Next(time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC))
	if want := time.Date(2024, 1, 3, 9, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestCronNamesAndSteps(t *testing.T) {
	// 6:30 on every Sunday of January
	c, err := ParseCron("30 6 * jan sun")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 2024-01-07 is a Sunday; from the 1st the next should be the 7th at 06:30
	next := c.Next(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2024, 1, 7, 6, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}

	// every 10 minutes starting at 0 => :00, :10, :20 ...
	c10, err := ParseCron("*/10 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	next = c10.Next(time.Date(2024, 1, 1, 0, 5, 0, 0, time.UTC))
	if want := time.Date(2024, 1, 1, 0, 10, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestCronDowSevenIsSunday(t *testing.T) {
	c, err := ParseCron("0 0 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	// 2024-01-01 is a Monday; next Sunday is the 7th.
	next := c.Next(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))
	if want := time.Date(2024, 1, 7, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestCronInvalid(t *testing.T) {
	for _, bad := range []string{
		"",
		"* * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * * 13 *",
		"* * * * 8",
		"a * * * *",
		"1-2-3 * * * *",
		"*/0 * * * *",
	} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("ParseCron(%q) expected error", bad)
		}
	}
}

func TestCronNeverMatches(t *testing.T) {
	// February 30 never exists.
	c, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if next := c.Next(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)); !next.IsZero() {
		t.Fatalf("expected zero time for impossible expression, got %v", next)
	}
}

func TestCronDomOrDow(t *testing.T) {
	// runs when either day-of-month is 13 OR it is Friday (Vixie behaviour).
	c, err := ParseCron("0 12 13 * 5")
	if err != nil {
		t.Fatal(err)
	}
	// 2024-01-05 is a Friday (not the 13th) -> matches via dow, so it fires
	// before the 13th.
	next := c.Next(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2024, 1, 5, 12, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}

	// dom=13, dow=2 (Tuesday). From Wednesday Jan 10 the first match is
	// Saturday Jan 13 (the 13th) via dom.
	c2, err := ParseCron("0 12 13 * 2")
	if err != nil {
		t.Fatal(err)
	}
	next = c2.Next(time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2024, 1, 13, 12, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}
