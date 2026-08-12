package httpjson

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "seconds", value: "7", want: 7 * time.Second},
		{name: "HTTP date", value: now.Add(9 * time.Second).Format(http.TimeFormat), want: 9 * time.Second},
		{name: "past HTTP date", value: now.Add(-time.Second).Format(http.TimeFormat)},
		{name: "invalid", value: "later"},
		{name: "empty"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := parseRetryAfter(test.value, now); got != test.want {
				t.Fatalf("parseRetryAfter(%q) = %s, want %s", test.value, got, test.want)
			}
		})
	}
}
