package core

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestJWTExpiry_Valid(t *testing.T) {
	want := time.Now().Add(time.Hour).Truncate(time.Second)
	tok := makeTestJWT(t, want)
	got, err := jwtExpiry(tok)
	if err != nil {
		t.Fatalf("jwtExpiry: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("exp = %v, want %v", got, want)
	}
}

func TestJWTExpiry_Errors(t *testing.T) {
	noExp := "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u"}`)) + ".s"
	notJSON := "h." + base64.RawURLEncoding.EncodeToString([]byte(`not-json`)) + ".s"
	cases := map[string]string{
		"two segments":    "header.payload",
		"bad base64":      "h.!!!not-base64!!!.s",
		"no exp claim":    noExp,
		"payload notjson": notJSON,
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := jwtExpiry(tok); err == nil {
				t.Fatalf("want error for %s", name)
			}
		})
	}
}
