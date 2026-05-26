package mq

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsDeferAck(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"other", errors.New("x"), false},
		{"exact", ErrDeferAck, true},
		{"wrapped", fmt.Errorf("batch: %w", ErrDeferAck), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeferAck(c.err); got != c.want {
				t.Errorf("IsDeferAck(%v) = %v; want %v", c.err, got, c.want)
			}
		})
	}
}
