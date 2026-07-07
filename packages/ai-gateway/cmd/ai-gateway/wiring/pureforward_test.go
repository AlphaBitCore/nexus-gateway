package wiring

import (
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/proxy"
)

func gaugeValue(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "nexus_ai_gateway_pure_forward_mode" {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatal("gauge nexus_ai_gateway_pure_forward_mode not registered")
	return 0
}

func TestRegisterPureForwardMode_GaugeReflectsFlag(t *testing.T) {
	regOff := prometheus.NewRegistry()
	RegisterPureForwardMode(regOff, slog.Default())
	want := 0.0
	if proxy.PerfPureForward() {
		want = 1.0
	}
	if v := gaugeValue(t, regOff); v != want {
		t.Errorf("gauge=%v want %v (flag=%v)", v, want, proxy.PerfPureForward())
	}
}
