import { describe, it, expect } from 'vitest';
import {
  sumMetricByBucket,
  rowsForMetricChart,
  pivotProviderMetricByBucket,
  sumPointValues,
  isProviderRollupRow,
} from '../src/pages/metrics/metrics-aggregates-helpers';
import type { MetricAggregatePoint } from '../src/api/types';

describe('isProviderRollupRow', () => {
  it('detects provider dimension keys', () => {
    expect(
      isProviderRollupRow({
        bucketStart: '',
        metricName: '',
        dimensionKey: 'routed_provider:openai',
        dimensions: {},
        value: '0',
      }),
    ).toBe(true);
    expect(
      isProviderRollupRow({
        bucketStart: '',
        metricName: '',
        dimensionKey: 'department:Eng',
        dimensions: {},
        value: '0',
      }),
    ).toBe(false);
  });
});

describe('rowsForMetricChart', () => {
  it('prefers provider rows when both provider and department exist', () => {
    const rows: MetricAggregatePoint[] = [
      {
        bucketStart: '2026-01-01T10:00:00.000Z',
        metricName: 'request_count',
        dimensionKey: 'routed_provider:openai',
        dimensions: { provider: 'openai' },
        value: '10',
      },
      {
        bucketStart: '2026-01-01T10:00:00.000Z',
        metricName: 'request_count',
        dimensionKey: 'department:Eng',
        dimensions: { department: 'Eng' },
        value: '99',
      },
    ];
    expect(rowsForMetricChart(rows, 'request_count')).toHaveLength(1);
    expect(rowsForMetricChart(rows, 'request_count')[0].value).toBe('10');
  });
});

describe('sumMetricByBucket', () => {
  it('sums values per bucket for the requested metric and formats bucket labels', () => {
    const rows: MetricAggregatePoint[] = [
      {
        bucketStart: '2026-01-01T10:00:00.000Z',
        metricName: 'request_count',
        dimensionKey: 'provider:a',
        dimensions: { provider: 'a' },
        value: '2',
      },
      {
        bucketStart: '2026-01-01T10:00:00.000Z',
        metricName: 'request_count',
        dimensionKey: 'provider:b',
        dimensions: { provider: 'b' },
        value: '3',
      },
      {
        bucketStart: '2026-01-01T11:00:00.000Z',
        metricName: 'request_count',
        dimensionKey: 'provider:a',
        dimensions: { provider: 'a' },
        value: '1',
      },
      {
        bucketStart: '2026-01-01T10:00:00.000Z',
        metricName: 'token_usage',
        dimensionKey: 'provider:a',
        dimensions: { provider: 'a' },
        value: '99',
      },
    ];

    const out = sumMetricByBucket(rows, 'request_count', { rangeHours: 24 });
    expect(out).toEqual([
      { bucketStart: '10:00', bucketAt: '2026-01-01T10:00:00.000Z', total: 5 },
      { bucketStart: '11:00', bucketAt: '2026-01-01T11:00:00.000Z', total: 1 },
    ]);
  });

  it('ignores non-finite values', () => {
    const rows: MetricAggregatePoint[] = [
      {
        bucketStart: '2026-01-01T12:00:00.000Z',
        metricName: 'request_count',
        dimensionKey: 'provider:x',
        dimensions: {},
        value: 'nan',
      },
      {
        bucketStart: '2026-01-01T12:00:00.000Z',
        metricName: 'request_count',
        dimensionKey: 'provider:x',
        dimensions: {},
        value: '4',
      },
    ];
    expect(sumMetricByBucket(rows, 'request_count')).toEqual([
      { bucketStart: '12:00', bucketAt: '2026-01-01T12:00:00.000Z', total: 4 },
    ]);
  });
});

describe('sumPointValues', () => {
  it('sums numeric values', () => {
    const rows: MetricAggregatePoint[] = [
      { bucketStart: '', metricName: '', dimensionKey: '', dimensions: {}, value: '1.5' },
      { bucketStart: '', metricName: '', dimensionKey: '', dimensions: {}, value: '2' },
    ];
    expect(sumPointValues(rows)).toBe(3.5);
  });
});

describe('pivotProviderMetricByBucket', () => {
  it('builds per-provider columns per bucket', () => {
    const rows: MetricAggregatePoint[] = [
      {
        bucketStart: '2026-01-01T10:00:00.000Z',
        metricName: 'request_count',
        dimensionKey: 'provider:openai',
        dimensions: { provider: 'openai' },
        value: '3',
      },
      {
        bucketStart: '2026-01-01T10:00:00.000Z',
        metricName: 'request_count',
        dimensionKey: 'provider:google',
        dimensions: { provider: 'google' },
        value: '7',
      },
      {
        bucketStart: '2026-01-01T11:00:00.000Z',
        metricName: 'request_count',
        dimensionKey: 'provider:openai',
        dimensions: { provider: 'openai' },
        value: '1',
      },
    ];
    const { data, providers } = pivotProviderMetricByBucket(rows, 'request_count', { rangeHours: 24 });
    expect(providers).toEqual(['google', 'openai']);
    expect(data).toHaveLength(2);
    expect(data[0].google).toBe(7);
    expect(data[0].openai).toBe(3);
    expect(data[1].openai).toBe(1);
    expect(data[1].google).toBe(0);
  });
});
