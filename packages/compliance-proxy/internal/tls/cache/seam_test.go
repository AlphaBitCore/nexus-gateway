package cache

import (
	"crypto/ecdsa"
	"crypto/tls"
	"strings"
	"testing"
	"time"
)

// TestPutToRedis_EncryptFailureBranch drives the EncryptPrivateKey
// failure arm inside putToRedis by handing it a cert whose private key
// is malformed (nil-curve ECDSA). Asserts the error is wrapped with
// `encrypt key for redis`.
func TestPutToRedis_EncryptFailureBranch(t *testing.T) {
	_, rdb := newTestRedis(t)
	iss := newIssuerForCacheTests(t)
	c := NewCertCache(iss, NewLRUCache(10), rdb, time.Hour, discardLogger())

	bogus := &tls.Certificate{
		Certificate: [][]byte{[]byte("placeholder-der")},
		PrivateKey:  &ecdsa.PrivateKey{}, // nil curve => marshal fails
	}
	err := c.putToRedis("encrypt-fail.example.com", bogus)
	if err == nil {
		t.Fatal("expected encrypt error from putToRedis")
	}
	if !strings.Contains(err.Error(), "encrypt key for redis") {
		t.Errorf("err should wrap 'encrypt key for redis'; got %q", err)
	}
}
