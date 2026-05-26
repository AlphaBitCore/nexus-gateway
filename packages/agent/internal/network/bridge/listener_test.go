package bridge

import "testing"

func TestParseHeader(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantHost string
		wantPort int
		wantFlow string
		wantErr  bool
	}{
		{
			name:     "ipv4 dns",
			input:    "BRIDGE api2.cursor.sh:443 abc123\n",
			wantHost: "api2.cursor.sh", wantPort: 443, wantFlow: "abc123",
		},
		{
			name:     "ipv4 literal",
			input:    "BRIDGE 1.2.3.4:8443 fid-9\n",
			wantHost: "1.2.3.4", wantPort: 8443, wantFlow: "fid-9",
		},
		{
			name:     "ipv6 bracketed",
			input:    "BRIDGE [::1]:443 v6flow\n",
			wantHost: "::1", wantPort: 443, wantFlow: "v6flow",
		},
		{
			name:     "trailing CRLF tolerated",
			input:    "BRIDGE x.y.z:443 fid\r\n",
			wantHost: "x.y.z", wantPort: 443, wantFlow: "fid",
		},
		{name: "missing prefix", input: "GET / HTTP/1.1\n", wantErr: true},
		{name: "empty flow", input: "BRIDGE host:443 \n", wantErr: true},
		{name: "missing port", input: "BRIDGE host abc\n", wantErr: true},
		{name: "bad port number", input: "BRIDGE host:0 abc\n", wantErr: true},
		{name: "ipv6 missing brackets", input: "BRIDGE ::1:443 abc\n", wantErr: true},
		{name: "host too long", input: "BRIDGE " + repeat("a", 254) + ":443 abc\n", wantErr: true},
		{name: "flow too long", input: "BRIDGE host:443 " + repeat("f", 129) + "\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, p, f, err := parseHeader(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got host=%q port=%d flow=%q", h, p, f)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h != tc.wantHost || p != tc.wantPort || f != tc.wantFlow {
				t.Errorf("got (%q,%d,%q), want (%q,%d,%q)", h, p, f, tc.wantHost, tc.wantPort, tc.wantFlow)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
