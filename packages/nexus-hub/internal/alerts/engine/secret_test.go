package alerting

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func nowUTC() time.Time { return time.Now().UTC() }

// testKey is a fixed 32-byte AES-256 key for deterministic cipher tests.
var testKey = []byte("0123456789abcdef0123456789abcdef")

func newTestCipher(t *testing.T) *ChannelSecretCipher {
	t.Helper()
	c, err := NewChannelSecretCipher(testKey)
	if err != nil {
		t.Fatalf("NewChannelSecretCipher: %v", err)
	}
	return c
}

func TestChannelSecretCipher_SealOpenRoundTrip(t *testing.T) {
	c := newTestCipher(t)
	const plain = "hunter2-smtp-password"
	sealed, err := c.seal(plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if sealed == plain {
		t.Fatal("sealed value equals plaintext — not encrypted")
	}
	if !strings.HasPrefix(sealed, secretEnvelopePrefix) {
		t.Fatalf("sealed value missing envelope prefix: %q", sealed)
	}
	got, err := c.open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != plain {
		t.Fatalf("round-trip mismatch: got %q want %q", got, plain)
	}
}

func TestChannelSecretCipher_SealUniqueIV(t *testing.T) {
	c := newTestCipher(t)
	a, _ := c.seal("same")
	b, _ := c.seal("same")
	if a == b {
		t.Fatal("two seals of same plaintext produced identical envelopes — IV not random")
	}
}

func TestChannelSecretCipher_OpenPassthroughForCleartext(t *testing.T) {
	c := newTestCipher(t)
	got, err := c.open("not-encrypted")
	if err != nil {
		t.Fatalf("open passthrough: %v", err)
	}
	if got != "not-encrypted" {
		t.Fatalf("passthrough mismatch: got %q", got)
	}
}

func TestChannelSecretCipher_OpenErrors(t *testing.T) {
	c := newTestCipher(t)
	tampered, _ := c.seal("secret")
	// Flip the last hex nibble of the ciphertext to force a GCM auth failure.
	corrupt := tampered[:len(tampered)-1] + flipHex(tampered[len(tampered)-1:])

	cases := []struct {
		name string
		in   string
	}{
		{"malformed envelope (no second colon)", secretEnvelopePrefix + "deadbeef"},
		{"bad iv hex", secretEnvelopePrefix + "zz:00"},
		{"wrong iv length", secretEnvelopePrefix + "00ff:00ff"},
		{"bad ciphertext hex", secretEnvelopePrefix + "0123456789abcdef01234567:zz"},
		{"tampered ciphertext fails auth", corrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.open(tc.in); err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
		})
	}
}

func flipHex(s string) string {
	if s == "0" {
		return "1"
	}
	return "0"
}

func TestChannelSecretCipher_SealIVError(t *testing.T) {
	c := newTestCipher(t)
	orig := secretRandReader
	secretRandReader = failingReader{}
	t.Cleanup(func() { secretRandReader = orig })
	if _, err := c.seal("x"); err == nil {
		t.Fatal("expected IV-generation error")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestNewChannelSecretCipher_BadKeyLength(t *testing.T) {
	if _, err := NewChannelSecretCipher([]byte("short")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}

// TestChannelSecretCipherFromKey pins the SEC-W2-03 Layer C contract: the cipher
// is built from the custody-RESOLVED plaintext the caller passes (not os.Getenv
// at point-of-use), so under provider "command" Hub keys the alert cipher with
// the unwrapped key — never the wrapped blob, which would fail the 64-hex check.
func TestChannelSecretCipherFromKey(t *testing.T) {
	t.Run("empty returns nil cipher", func(t *testing.T) {
		c, err := ChannelSecretCipherFromKey("")
		if err != nil || c != nil {
			t.Fatalf("want (nil,nil); got (%v,%v)", c, err)
		}
	})
	t.Run("wrong length errors", func(t *testing.T) {
		if _, err := ChannelSecretCipherFromKey("abcd"); err == nil {
			t.Fatal("expected length error")
		}
	})
	t.Run("non-hex errors", func(t *testing.T) {
		if _, err := ChannelSecretCipherFromKey(strings.Repeat("z", 64)); err == nil {
			t.Fatal("expected hex error")
		}
	})
	t.Run("valid key constructs cipher", func(t *testing.T) {
		// Non-degenerate 32-byte key (SEC-M2-02: ChannelSecretCipherFromKey now
		// rejects all-zero / single-repeat / low-distinct keys).
		c, err := ChannelSecretCipherFromKey("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
		if err != nil || c == nil {
			t.Fatalf("want cipher; got (%v,%v)", c, err)
		}
	})
	t.Run("weak key rejected", func(t *testing.T) {
		// SEC-M2-02 regression: an all-same-byte key (valid hex, right length)
		// must fail closed.
		if _, err := ChannelSecretCipherFromKey(strings.Repeat("ab", 32)); err == nil {
			t.Fatal("expected weak-key rejection for an all-0xab master key")
		}
	})
}

func TestEncryptDecryptConfig_RoundTrip(t *testing.T) {
	c := newTestCipher(t)
	cfg := map[string]any{
		"smtpHost":     "smtp.example.com",
		"smtpPort":     587,
		"smtpPassword": "s3cr3t-pw",
	}
	enc, err := c.encryptConfig(cfg)
	if err != nil {
		t.Fatalf("encryptConfig: %v", err)
	}
	// Secret field is now ciphertext; non-secret fields untouched.
	if enc["smtpPassword"] == "s3cr3t-pw" {
		t.Fatal("smtpPassword left as plaintext")
	}
	if !strings.HasPrefix(enc["smtpPassword"].(string), secretEnvelopePrefix) {
		t.Fatalf("smtpPassword not enveloped: %v", enc["smtpPassword"])
	}
	if enc["smtpHost"] != "smtp.example.com" {
		t.Fatalf("non-secret smtpHost mutated: %v", enc["smtpHost"])
	}
	// Original map is not mutated (deep copy).
	if cfg["smtpPassword"] != "s3cr3t-pw" {
		t.Fatal("encryptConfig mutated the input map")
	}
	dec, err := c.decryptConfig(enc)
	if err != nil {
		t.Fatalf("decryptConfig: %v", err)
	}
	if dec["smtpPassword"] != "s3cr3t-pw" {
		t.Fatalf("decrypt did not restore plaintext: %v", dec["smtpPassword"])
	}
}

// TestEncryptConfig_WebhookURLSealed verifies F-0247: a Slack incoming-webhook
// URL (secret token in the path) is encrypted at rest and restored on decrypt.
func TestEncryptConfig_WebhookURLSealed(t *testing.T) {
	c := newTestCipher(t)
	const url = "https://hooks.slack.com/services/T000/B000/Xsecret123"
	cfg := map[string]any{"webhookUrl": url, "name": "ops"}
	enc, err := c.encryptConfig(cfg)
	if err != nil {
		t.Fatalf("encryptConfig: %v", err)
	}
	if enc["webhookUrl"] == url {
		t.Fatal("webhookUrl left as plaintext at rest")
	}
	if !strings.HasPrefix(enc["webhookUrl"].(string), secretEnvelopePrefix) {
		t.Fatalf("webhookUrl not enveloped: %v", enc["webhookUrl"])
	}
	dec, err := c.decryptConfig(enc)
	if err != nil {
		t.Fatalf("decryptConfig: %v", err)
	}
	if dec["webhookUrl"] != url {
		t.Fatalf("decrypt did not restore webhookUrl: %v", dec["webhookUrl"])
	}
}

func TestEncryptConfig_SensitiveHeaders(t *testing.T) {
	c := newTestCipher(t)
	cfg := map[string]any{
		"url": "https://hook.example.com",
		"headers": map[string]any{
			"Authorization": "Bearer abc123",
			"X-Trace":       "plain-value",
		},
	}
	enc, err := c.encryptConfig(cfg)
	if err != nil {
		t.Fatalf("encryptConfig: %v", err)
	}
	hdrs := enc["headers"].(map[string]any)
	if !strings.HasPrefix(hdrs["Authorization"].(string), secretEnvelopePrefix) {
		t.Fatalf("Authorization header not encrypted: %v", hdrs["Authorization"])
	}
	if hdrs["X-Trace"] != "plain-value" {
		t.Fatalf("non-sensitive header mutated: %v", hdrs["X-Trace"])
	}
	dec, err := c.decryptConfig(enc)
	if err != nil {
		t.Fatalf("decryptConfig: %v", err)
	}
	if dec["headers"].(map[string]any)["Authorization"] != "Bearer abc123" {
		t.Fatal("header decrypt did not restore plaintext")
	}
}

func TestEncryptConfig_NilCipherPassthrough(t *testing.T) {
	var c *ChannelSecretCipher
	cfg := map[string]any{"smtpPassword": "plain"}
	enc, err := c.encryptConfig(cfg)
	if err != nil {
		t.Fatalf("nil cipher encrypt: %v", err)
	}
	if enc["smtpPassword"] != "plain" {
		t.Fatal("nil cipher must pass through unchanged")
	}
	dec, err := c.decryptConfig(cfg)
	if err != nil {
		t.Fatalf("nil cipher decrypt: %v", err)
	}
	if dec["smtpPassword"] != "plain" {
		t.Fatal("nil cipher decrypt must pass through")
	}
}

func TestEncryptConfig_IdempotentOnAlreadyEncrypted(t *testing.T) {
	c := newTestCipher(t)
	cfg := map[string]any{"routingKey": "pd-key"}
	once, _ := c.encryptConfig(cfg)
	twice, _ := c.encryptConfig(once)
	if once["routingKey"] != twice["routingKey"] {
		t.Fatal("re-encrypting an already-encrypted value changed it (double encryption)")
	}
}

func TestEncryptConfig_NonStringSecretPassthrough(t *testing.T) {
	c := newTestCipher(t)
	cfg := map[string]any{"routingKey": 12345} // unexpected non-string
	enc, err := c.encryptConfig(cfg)
	if err != nil {
		t.Fatalf("encryptConfig: %v", err)
	}
	if enc["routingKey"] != 12345 {
		t.Fatalf("non-string secret should pass through: %v", enc["routingKey"])
	}
}

func TestEncryptConfig_SealErrorPropagates(t *testing.T) {
	c := newTestCipher(t)
	orig := secretRandReader
	secretRandReader = failingReader{}
	t.Cleanup(func() { secretRandReader = orig })
	if _, err := c.encryptConfig(map[string]any{"smtpPassword": "x"}); err == nil {
		t.Fatal("expected seal error to propagate")
	}
}

// captureArg records the []byte config blob bound to an Insert/Update so the
// test can assert it is ciphertext at rest.
type captureArg struct{ got *[]byte }

func (c captureArg) Match(v any) bool {
	b, ok := v.([]byte)
	if !ok {
		return false
	}
	*c.got = b
	return true
}

func TestInsertChannel_EncryptsSecretAtRest(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	var stored []byte
	mock.ExpectQuery(`INSERT INTO "AlertChannel"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), captureArg{&stored}).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("ch-1"))

	s := NewStoreWithPgxPool(mock).WithChannelSecretCipher(newTestCipher(t))
	id, err := s.InsertChannel(context.Background(), Channel{
		Name: "test-email", Type: "email",
		Config: map[string]any{"smtpHost": "h", "smtpPassword": "PLAINTEXT-PW"},
	})
	if err != nil || id != "ch-1" {
		t.Fatalf("InsertChannel: id=%q err=%v", id, err)
	}
	got := string(stored)
	if strings.Contains(got, "PLAINTEXT-PW") {
		t.Fatalf("secret stored in cleartext at rest: %s", got)
	}
	if !strings.Contains(got, secretEnvelopePrefix) {
		t.Fatalf("stored config not encrypted (no envelope): %s", got)
	}
}

func TestUpdateChannel_EncryptsSecretAtRest(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	var stored []byte
	mock.ExpectExec(`UPDATE "AlertChannel"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), captureArg{&stored}).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	s := NewStoreWithPgxPool(mock).WithChannelSecretCipher(newTestCipher(t))
	err := s.UpdateChannel(context.Background(), Channel{
		ID: "ch-1", Name: "test-slack", Type: "slack",
		Config: map[string]any{"botToken": "PLAINTEXT-TOKEN"},
	})
	if err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	if strings.Contains(string(stored), "PLAINTEXT-TOKEN") {
		t.Fatalf("secret stored in cleartext at rest: %s", stored)
	}
	if !strings.Contains(string(stored), secretEnvelopePrefix) {
		t.Fatalf("stored config not encrypted: %s", stored)
	}
}

func TestGetChannel_DecryptsForSender(t *testing.T) {
	cipher := newTestCipher(t)
	sealed, err := cipher.seal("real-bot-token")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	encConfig := `{"botToken":"` + sealed + `","channel":"#alerts"}`

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "AlertChannel"`).
		WithArgs("ch-1").
		WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(
			"ch-1", "test-slack", "slack", true,
			[]string{"critical"}, []string{"quota"}, []byte(encConfig),
			nowUTC(), nowUTC(),
		))

	s := NewStoreWithPgxPool(mock).WithChannelSecretCipher(cipher)
	ch, err := s.GetChannel(context.Background(), "ch-1")
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	// The sender reads cleartext.
	if ch.Config["botToken"] != "real-bot-token" {
		t.Fatalf("GetChannel did not decrypt botToken for sender: %v", ch.Config["botToken"])
	}
	// Admin read path masks the (now-cleartext) secret — masking still works.
	masked := MaskChannelConfig(ch.Config)
	mv, _ := masked["botToken"].(string)
	if !strings.HasPrefix(mv, maskPrefix) {
		t.Fatalf("admin mask not applied to decrypted secret: %v", masked["botToken"])
	}
}

func TestGetChannel_DecryptErrorPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// botToken carries an envelope prefix but garbage body → open() errors.
	bad := `{"botToken":"` + secretEnvelopePrefix + `zz:zz"}`
	mock.ExpectQuery(`FROM "AlertChannel"`).
		WithArgs("ch-1").
		WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(
			"ch-1", "test-slack", "slack", true,
			[]string{"critical"}, []string{"quota"}, []byte(bad),
			nowUTC(), nowUTC(),
		))
	s := NewStoreWithPgxPool(mock).WithChannelSecretCipher(newTestCipher(t))
	if _, err := s.GetChannel(context.Background(), "ch-1"); err == nil ||
		!strings.Contains(err.Error(), "decrypt channel config") {
		t.Fatalf("want decrypt error; got %v", err)
	}
}

func TestListChannels_DecryptsEach(t *testing.T) {
	cipher := newTestCipher(t)
	sealed, _ := cipher.seal("pd-routing-key")
	enc := `{"routingKey":"` + sealed + `"}`

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "AlertChannel"`).
		WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(
			"ch-1", "test-pd", "pagerduty", true,
			[]string{"critical"}, []string{"quota"}, []byte(enc),
			nowUTC(), nowUTC(),
		))
	s := NewStoreWithPgxPool(mock).WithChannelSecretCipher(cipher)
	chs, err := s.ListChannels(context.Background())
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(chs) != 1 || chs[0].Config["routingKey"] != "pd-routing-key" {
		t.Fatalf("ListChannels did not decrypt: %+v", chs)
	}
}

func TestListEnabledChannels_DecryptsEach(t *testing.T) {
	cipher := newTestCipher(t)
	sealed, _ := cipher.seal("pd-routing-key")
	enc := `{"routingKey":"` + sealed + `"}`

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`WHERE enabled = true`).
		WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(
			"ch-1", "test-pd", "pagerduty", true,
			[]string{"critical"}, []string{"quota"}, []byte(enc),
			nowUTC(), nowUTC(),
		))
	s := NewStoreWithPgxPool(mock).WithChannelSecretCipher(cipher)
	chs, err := s.ListEnabledChannels(context.Background())
	if err != nil {
		t.Fatalf("ListEnabledChannels: %v", err)
	}
	if len(chs) != 1 || chs[0].Config["routingKey"] != "pd-routing-key" {
		t.Fatalf("ListEnabledChannels did not decrypt: %+v", chs)
	}
}

func TestUpdateChannel_EncryptErrorPropagates(t *testing.T) {
	c := newTestCipher(t)
	orig := secretRandReader
	secretRandReader = failingReader{}
	t.Cleanup(func() { secretRandReader = orig })
	s := NewStoreWithPgxPool(nil).WithChannelSecretCipher(c)
	err := s.UpdateChannel(context.Background(), Channel{
		ID: "ch-1", Config: map[string]any{"botToken": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "encrypt config") {
		t.Fatalf("want encrypt config error; got %v", err)
	}
}

func TestListChannels_DecryptErrorPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	bad := `{"routingKey":"` + secretEnvelopePrefix + `zz:zz"}`
	mock.ExpectQuery(`FROM "AlertChannel"`).
		WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(
			"ch-1", "test-pd", "pagerduty", true,
			[]string{"critical"}, []string{"quota"}, []byte(bad),
			nowUTC(), nowUTC(),
		))
	s := NewStoreWithPgxPool(mock).WithChannelSecretCipher(newTestCipher(t))
	if _, err := s.ListChannels(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "decrypt channel config") {
		t.Fatalf("want decrypt error; got %v", err)
	}
}

func TestListEnabledChannels_DecryptErrorPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	bad := `{"routingKey":"` + secretEnvelopePrefix + `zz:zz"}`
	mock.ExpectQuery(`WHERE enabled = true`).
		WillReturnRows(pgxmock.NewRows(channelRowCols()).AddRow(
			"ch-1", "test-pd", "pagerduty", true,
			[]string{"critical"}, []string{"quota"}, []byte(bad),
			nowUTC(), nowUTC(),
		))
	s := NewStoreWithPgxPool(mock).WithChannelSecretCipher(newTestCipher(t))
	if _, err := s.ListEnabledChannels(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "decrypt channel config") {
		t.Fatalf("want decrypt error; got %v", err)
	}
}

func TestTransformHeaders_FnErrorPropagates(t *testing.T) {
	c := newTestCipher(t)
	orig := secretRandReader
	secretRandReader = failingReader{}
	t.Cleanup(func() { secretRandReader = orig })
	cfg := map[string]any{
		"headers": map[string]any{"Authorization": "Bearer x"},
	}
	if _, err := c.encryptConfig(cfg); err == nil {
		t.Fatal("expected seal error to propagate through transformHeaders")
	}
}

func TestInsertChannel_EncryptErrorPropagates(t *testing.T) {
	c := newTestCipher(t)
	orig := secretRandReader
	secretRandReader = failingReader{}
	t.Cleanup(func() { secretRandReader = orig })
	s := NewStoreWithPgxPool(nil).WithChannelSecretCipher(c)
	_, err := s.InsertChannel(context.Background(), Channel{
		Config: map[string]any{"smtpPassword": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "encrypt config") {
		t.Fatalf("want encrypt config error; got %v", err)
	}
}
