package testkit

import (
	"testing"
	"time"
)

func TestDeadlineFromEnv(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", DefaultDeadline},
		{"junk", DefaultDeadline},
		{"-1s", DefaultDeadline},
		{"0", DefaultDeadline},
		{"120s", 120 * time.Second},
		{"1m30s", 90 * time.Second},
	} {
		if got := deadlineFromEnv(tc.in, DefaultDeadline); got != tc.want {
			t.Errorf("deadlineFromEnv(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	t.Setenv(EnvDeadline, "7s")
	if got := Deadline(); got != 7*time.Second {
		t.Errorf("Deadline() with %s=7s = %v", EnvDeadline, got)
	}
}
