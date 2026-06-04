package kit

import "testing"

func TestSplitCmdArg(t *testing.T) {
	cases := []struct {
		in, cmd, arg string
	}{
		{"event ev-9a3f", "event", "ev-9a3f"},
		{"cost", "cost", ""},
		{"/event  ev-1 ", "event", "ev-1"},
		{"   ", "", ""},
		{"/clear", "clear", ""},
	}
	for _, tc := range cases {
		cmd, arg := SplitCmdArg(tc.in)
		if cmd != tc.cmd || arg != tc.arg {
			t.Errorf("SplitCmdArg(%q) = (%q,%q), want (%q,%q)", tc.in, cmd, arg, tc.cmd, tc.arg)
		}
	}
}
