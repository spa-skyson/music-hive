package db

import (
	"testing"
	"time"
)

func TestMondayZeroWeekday(t *testing.T) {
	tests := []struct {
		day  time.Weekday
		want int
	}{
		{time.Monday, 0},
		{time.Tuesday, 1},
		{time.Saturday, 5},
		{time.Sunday, 6},
	}
	for _, tt := range tests {
		if got := mondayZeroWeekday(tt.day); got != tt.want {
			t.Fatalf("mondayZeroWeekday(%s) = %d, want %d", tt.day, got, tt.want)
		}
	}
}
