package core

import (
	"testing"
	"time"
)

func TestMedian(t *testing.T) {
	if got := median([]int64{5, 1, 3}); got != 3 {
		t.Errorf("odd median = %d", got)
	}
	if got := median([]time.Duration{4, 1, 3, 2}); got != 2 {
		t.Errorf("even median = %d", got)
	}
	xs := []int64{3, 1, 2}
	median(xs)
	if xs[0] != 3 {
		t.Error("median sorted its input")
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0:         "0 Б",
		1023:      "1023 Б",
		1536:      "1.5 КБ",
		50 << 20:  "50.0 МБ",
		300 << 30: "300 ГБ",
		3 << 40:   "3.0 ТБ",
	} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int64]string{1: "бэкап", 2: "бэкапа", 5: "бэкапов", 11: "бэкапов", 21: "бэкап", 104: "бэкапа", 112: "бэкапов"} {
		if got := plural(n, "бэкап", "бэкапа", "бэкапов"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:            "30 с",
		47 * time.Minute:            "47 мин",
		2*time.Hour + 5*time.Minute: "2 ч 5 мин",
		3 * time.Hour:               "3 ч",
		72 * time.Hour:              "3 дн",
	} {
		if got := HumanDuration(d); got != want {
			t.Errorf("HumanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
