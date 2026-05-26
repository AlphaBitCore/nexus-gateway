package http

import (
	"context"
	"testing"
)

func TestAttempt(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want int
	}{
		{"absent defaults to 1", context.Background(), 1},
		{"set to 1", WithAttempt(context.Background(), 1), 1},
		{"set to 5", WithAttempt(context.Background(), 5), 5},
		{"set to 0 coerced to 1", WithAttempt(context.Background(), 0), 1},
		{"negative coerced to 1", WithAttempt(context.Background(), -3), 1},
		{"overwrite last write wins", WithAttempt(WithAttempt(context.Background(), 2), 7), 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AttemptFromContext(tc.ctx); got != tc.want {
				t.Errorf("AttemptFromContext: got %d, want %d", got, tc.want)
			}
		})
	}
}
