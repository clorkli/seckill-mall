package main

import (
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	tests := []struct {
		name       string
		retryCount int
		want       time.Duration
	}{
		{
			name:       "zero retry count uses scan interval",
			retryCount: 0,
			want:       ScanInterval,
		},
		{
			name:       "first retry waits five seconds",
			retryCount: 1,
			want:       5 * time.Second,
		},
		{
			name:       "second retry doubles delay",
			retryCount: 2,
			want:       10 * time.Second,
		},
		{
			name:       "sixth retry caps at max delay",
			retryCount: 6,
			want:       MaxRetryDelay,
		},
		{
			name:       "large retry count caps at max delay",
			retryCount: 100,
			want:       MaxRetryDelay,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := retryDelay(tt.retryCount)
			if got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}
