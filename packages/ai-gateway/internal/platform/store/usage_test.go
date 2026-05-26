package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestGetUsageForVK(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM traffic_event\s+WHERE source = 'ai-gateway' AND identity->'vk'->>'id' = \$1`).
			WithArgs("vk-1").
			WillReturnRows(pgxmock.NewRows([]string{"requests", "pt", "ct", "tt", "cost"}).
				AddRow(int64(10), int64(100), int64(200), int64(300), float64(1.5)))
		got, err := db.GetUsageForVK(context.Background(), "vk-1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.TotalRequests != 10 || got.TotalTokens != 300 || got.EstimatedCostUsd != 1.5 {
			t.Errorf("unexpected: %+v", got)
		}
		if got.VirtualKeyID != "vk-1" {
			t.Errorf("vk id mismatch: %s", got.VirtualKeyID)
		}
	})

	t.Run("err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM traffic_event`).
			WithArgs("vk-1").
			WillReturnError(want)
		_, err := db.GetUsageForVK(context.Background(), "vk-1")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "get usage for vk") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}

func TestCostSumSince(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM traffic_event\s+WHERE source = 'ai-gateway'`).
			WithArgs(pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"cost"}).AddRow(float64(42.5)))
		got, err := db.CostSumSince(context.Background(), time.Hour)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != 42.5 {
			t.Errorf("got %v", got)
		}
	})

	t.Run("err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM traffic_event`).
			WithArgs(pgxmock.AnyArg()).
			WillReturnError(want)
		_, err := db.CostSumSince(context.Background(), time.Hour)
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "cost sum since") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}

func TestGetDailyUsageForVK(t *testing.T) {
	start := time.Now().Add(-7 * 24 * time.Hour)
	end := time.Now()

	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM traffic_event\s+WHERE source = 'ai-gateway'`).
			WithArgs("vk-1", start, end).
			WillReturnRows(pgxmock.NewRows([]string{"day", "model", "provider", "requests", "pt", "ct", "tt", "cost"}).
				AddRow(start, "gpt-4o", "openai", int64(5), int64(100), int64(200), int64(300), float64(1.0)).
				AddRow(start, "claude-3", "anthropic", int64(3), int64(50), int64(150), int64(200), float64(0.5)))
		got, err := db.GetDailyUsageForVK(context.Background(), "vk-1", start, end)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("len = %d", len(got))
		}
		if got[0].ModelName != "gpt-4o" || got[0].Requests != 5 {
			t.Errorf("row 0: %+v", got[0])
		}
	})

	t.Run("query err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM traffic_event`).
			WithArgs("vk-1", start, end).
			WillReturnError(want)
		_, err := db.GetDailyUsageForVK(context.Background(), "vk-1", start, end)
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "get daily usage for vk") {
			t.Errorf("missing prefix: %v", err)
		}
	})

	t.Run("scan err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM traffic_event`).
			WithArgs("vk-1", start, end).
			WillReturnRows(pgxmock.NewRows([]string{"day"}).AddRow(start))
		_, err := db.GetDailyUsageForVK(context.Background(), "vk-1", start, end)
		if err == nil || !strings.Contains(err.Error(), "scan daily usage row") {
			t.Errorf("expected scan err; got: %v", err)
		}
	})

	t.Run("rows close err propagated", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("close-err")
		mock.ExpectQuery(`FROM traffic_event`).
			WithArgs("vk-1", start, end).
			WillReturnRows(pgxmock.NewRows([]string{"day", "model", "provider", "requests", "pt", "ct", "tt", "cost"}).
				AddRow(start, "gpt-4o", "openai", int64(5), int64(100), int64(200), int64(300), float64(1.0)).
				CloseError(want))
		_, err := db.GetDailyUsageForVK(context.Background(), "vk-1", start, end)
		if !errors.Is(err, want) {
			t.Errorf("must propagate close err; got: %v", err)
		}
		if !strings.Contains(err.Error(), "iterate daily usage") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}
