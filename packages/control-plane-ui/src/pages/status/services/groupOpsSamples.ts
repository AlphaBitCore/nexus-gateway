/**
 * groupOpsSamples — frontend aggregation shim that turns flat ops-metrics
 * samples (from `/api/admin/ops-metrics/current`) into the per-service shape
 * the legacy Status / ServiceDetail cards already consume.
 *
 * Why this lives in the UI: spec §10.5 deletes the old
 * `/api/admin/service-metrics` endpoint. The new `/ops-metrics/current` is
 * the single source for live process telemetry, but it returns one row per
 * (nodeId, metricName, dimensionKey) sample — the cards still want
 * per-`serviceName` cells. Aggregating in the UI keeps the rewire
 * narrowly scoped to T35; subsequent UI tasks (T36-T41) introduce
 * richer per-node views that consume samples directly.
 *
 * Aggregation policy:
 *   - Counters (`*_total`, `requests_total`, ...) — SUM `value` across
 *     instances and dimensions.
 *   - Gauges  (`runtime.*`, `redis.available`, `tunnels.active`, ...) —
 *     LAST sample by `sampledAt` per (nodeType, metricName, dim).
 *   - Histograms — opaque from the wire (no scalar value), so p50/p99
 *     fields default to 0. The full distribution lands on
 *     timeseries / fleet endpoints.
 *
 * Service-name mapping: a node's wire `nodeType` matches the serviceName
 * the legacy cards expect (`control-plane`, `ai-gateway`,
 * `compliance-proxy`, `nexus-hub`). Other types are grouped under their
 * raw nodeType so future cards can claim them without code changes.
 */

import type { OpsMetricSample } from '@/api/services/infrastructure/ops/opsmetrics';
import type { RuntimeMetrics, ServiceMetricSet, ServiceMetricsResponse } from '@/api/services/infrastructure/misc/system';

/** Service-name canonicalization. Returns the same string for known services. */
export function nodeTypeToServiceName(nodeType: string): string {
  switch (nodeType) {
    case 'control-plane':
    case 'ai-gateway':
    case 'compliance-proxy':
    case 'nexus-hub':
      return nodeType;
    default:
      return nodeType;
  }
}

interface aggregateState {
  // Counter sums, keyed by metric name (no dim breakdown — top-level total only).
  counters: Map<string, number>;
  // Last-seen gauges by metric name.
  gauges: Map<string, { value: number; sampledAt: string }>;
  // Number of unique node IDs (rough instance count).
  nodeIds: Set<string>;
}

function emptyState(): aggregateState {
  return { counters: new Map(), gauges: new Map(), nodeIds: new Set() };
}

function recordSample(state: aggregateState, s: OpsMetricSample): void {
  state.nodeIds.add(s.nodeId);
  const v = s.value;
  if (v == null) return;
  const kind = s.metricKind;
  if (kind === 'counter') {
    state.counters.set(s.metricName, (state.counters.get(s.metricName) ?? 0) + v);
  } else if (kind === 'gauge') {
    const prev = state.gauges.get(s.metricName);
    if (!prev || prev.sampledAt < s.sampledAt) {
      state.gauges.set(s.metricName, { value: v, sampledAt: s.sampledAt });
    }
  }
  // histograms: nothing scalar to record here — leave for richer per-node UIs.
}

function num(v: number | undefined): number {
  return v ?? 0;
}

/**
 * Build a `RuntimeMetrics` cell from the aggregated state's `runtime.*`
 * gauges. Bytes are converted to MB to match the legacy unit display.
 */
function buildRuntime(state: aggregateState): RuntimeMetrics {
  const heapAllocBytes = state.gauges.get('runtime.heap_alloc_bytes')?.value;
  const heapSysBytes = state.gauges.get('runtime.heap_sys_bytes')?.value;
  return {
    goroutines: num(state.gauges.get('runtime.goroutines')?.value),
    heapAllocMB: heapAllocBytes != null ? heapAllocBytes / (1024 * 1024) : 0,
    heapSysMB: heapSysBytes != null ? heapSysBytes / (1024 * 1024) : 0,
    gcPauseP50Ms: num(state.gauges.get('runtime.gc_pause_p50_ms')?.value),
    gcCount: Math.round(num(state.counters.get('runtime.gc_count_total'))),
    threads: num(state.gauges.get('runtime.threads')?.value),
  };
}

/**
 * Build a metric record using the legacy card schema. Unknown / unset values
 * fall back to 0 so the existing renderers don't crash on missing fields.
 */
function buildMetricRecord(state: aggregateState, serviceName: string): Record<string, number | boolean> {
  const m: Record<string, number | boolean> = {};

  // Counters shared across services
  m.requestsTotal = num(state.counters.get('requests_total')) || num(state.counters.get('http.requests_total'));
  m.errorsTotal = num(state.counters.get('errors_total')) || num(state.counters.get('http.errors_total'));

  // Histogram p50/p99: not available from /current — keep at 0 so cards render
  // a stable shape; rich timeseries views land in T36-T41.
  m.requestDurationP50Ms = 0;
  m.requestDurationP99Ms = 0;
  m.tlsHandshakeP50Ms = 0;

  if (serviceName === 'control-plane') {
    m.authFailuresTotal = num(state.counters.get('auth.attempts_total'));
    m.iamDenialsTotal = num(state.counters.get('iam.eval_total'));
  } else if (serviceName === 'ai-gateway') {
    m.tokensPromptTotal = num(state.counters.get('tokens_total'));
    m.tokensCompletionTotal = 0; // direction dim not split in this aggregation
  } else if (serviceName === 'compliance-proxy') {
    m.connectionsActive = num(state.gauges.get('tunnels.active')?.value);
    m.connectionsTotal = num(state.counters.get('tunnels.total'));
    m.connectionsRejected = 0;
    m.certCacheHitRate = 0;
    m.auditQueueDepth = num(state.gauges.get('audit.queue_depth')?.value);
    const redis = state.gauges.get('redis.available')?.value;
    m.redisAvailable = redis != null ? redis > 0 : false;
  }
  return m;
}

/**
 * Group raw ops-metrics samples into the legacy `ServiceMetricsResponse`
 * shape consumed by `ServiceCard` / `ServiceDetailPage`.
 *
 * The function never throws — empty / missing samples produce an empty
 * `services` map so callers can render the same "no data" state regardless
 * of whether the endpoint succeeded with zero rows or merely returned a
 * subset.
 */
export function groupOpsSamples(samples: OpsMetricSample[] | null | undefined): ServiceMetricsResponse {
  const states = new Map<string, aggregateState>();
  for (const s of samples ?? []) {
    const svc = nodeTypeToServiceName(s.nodeType);
    if (!svc) continue;
    let state = states.get(svc);
    if (!state) {
      state = emptyState();
      states.set(svc, state);
    }
    recordSample(state, s);
  }

  const services: Record<string, ServiceMetricSet> = {};
  for (const [svc, state] of states) {
    services[svc] = {
      instances: state.nodeIds.size,
      metrics: buildMetricRecord(state, svc),
      runtime: buildRuntime(state),
    };
  }

  return { cachedAt: new Date().toISOString(), services };
}
