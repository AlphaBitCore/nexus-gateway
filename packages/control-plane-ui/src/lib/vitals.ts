/**
 * Web Vitals reporting — tracks Core Web Vitals (LCP, FID, CLS, FCP, TTFB).
 *
 * In development: logs to console.
 * In production: can be wired to an analytics endpoint.
 *
 * Called once from main.tsx after app renders.
 */
import { onCLS, onFCP, onLCP, onTTFB, type Metric } from 'web-vitals';

function reportMetric(metric: Metric) {
  if (process.env.NODE_ENV !== 'production') {
    console.debug(`[WebVital] ${metric.name}: ${metric.value.toFixed(1)}ms (${metric.rating})`);
  }

  // Production: send to analytics endpoint
  // Example: navigator.sendBeacon('/api/admin/vitals', JSON.stringify(metric));
}

export function reportWebVitals() {
  onCLS(reportMetric);
  onFCP(reportMetric);
  onLCP(reportMetric);
  onTTFB(reportMetric);
}
