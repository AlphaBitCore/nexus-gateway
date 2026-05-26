package aiguard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

func silentSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type stubLoader struct {
	calls int
	out   *configstore.AIGuardConfig
	err   error
}

func (s *stubLoader) Load(ctx context.Context) (*configstore.AIGuardConfig, error) {
	s.calls++
	return s.out, s.err
}

func TestConfigCache_LoadsOnce(t *testing.T) {
	s := &stubLoader{out: &configstore.AIGuardConfig{ID: "singleton", BackendMode: "configured_provider"}}
	c := NewConfigCache(s, 1*time.Minute, silentSlog())
	ctx := context.Background()
	_, _ = c.Get(ctx)
	_, _ = c.Get(ctx)
	if s.calls != 1 {
		t.Errorf("loader calls: got %d, want 1 (should cache)", s.calls)
	}
}

func TestConfigCache_Invalidate(t *testing.T) {
	s := &stubLoader{out: &configstore.AIGuardConfig{ID: "singleton"}}
	c := NewConfigCache(s, 1*time.Hour, silentSlog())
	_, _ = c.Get(context.Background())
	c.Invalidate()
	_, _ = c.Get(context.Background())
	if s.calls != 2 {
		t.Errorf("after Invalidate, reload should happen; calls: %d", s.calls)
	}
}

func TestConfigCache_TTLRefresh(t *testing.T) {
	s := &stubLoader{out: &configstore.AIGuardConfig{ID: "singleton"}}
	c := NewConfigCache(s, 10*time.Millisecond, silentSlog())
	_, _ = c.Get(context.Background())
	time.Sleep(15 * time.Millisecond)
	_, _ = c.Get(context.Background())
	if s.calls != 2 {
		t.Errorf("after TTL, reload should happen; calls: %d", s.calls)
	}
}

func TestConfigCache_ErrorWithPriorSnapshotReturnsStale(t *testing.T) {
	s := &stubLoader{out: &configstore.AIGuardConfig{ID: "singleton", BackendMode: "configured_provider"}}
	c := NewConfigCache(s, 10*time.Millisecond, silentSlog())
	_, _ = c.Get(context.Background())
	time.Sleep(15 * time.Millisecond)
	s.err = errors.New("boom")
	s.out = nil
	cfg, err := c.Get(context.Background())
	if err != nil {
		t.Errorf("want fail-open on error with prior snapshot, got: %v", err)
	}
	if cfg == nil || cfg.BackendMode != "configured_provider" {
		t.Errorf("want stale snapshot, got: %+v", cfg)
	}
}

func TestConfigCache_ErrorNoPriorSnapshotReturnsError(t *testing.T) {
	s := &stubLoader{err: errors.New("boom")}
	c := NewConfigCache(s, 1*time.Minute, silentSlog())
	_, err := c.Get(context.Background())
	if err == nil {
		t.Fatal("want error when no prior snapshot exists")
	}
}
