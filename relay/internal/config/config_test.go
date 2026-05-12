package config

import (
	"testing"
	"time"
)

func TestTimeoutDuration(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"300s", 300 * time.Second},
		{"5m", 5 * time.Minute},
		{"0s", 0},
		{"0", 0},
		{"", 300 * time.Second},
		{"invalid", 300 * time.Second},
		{"-1s", 0},
	}

	for _, tc := range cases {
		got := (InferenceConfig{Timeout: tc.input}).TimeoutDuration()
		if got != tc.want {
			t.Errorf("TimeoutDuration(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}
