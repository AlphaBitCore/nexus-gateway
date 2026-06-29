import { useState, useMemo, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { complianceReportApi } from '../../api/services/compliance/compliance-report';
import reportStyles from './ComplianceReportPage.module.css';
import type { ComplianceReportResponse } from '../../api/services/compliance/compliance-report';
import {
  PageHeader, LoadingSpinner, ErrorBanner, Card, Stack, Button, Input, Select,
} from '@/components/ui';

/**
 * Compliance report viewer with browser print-to-PDF.
 *
 * No server-side PDF generation. The compliance officer picks a time
 * window, the BFF aggregates the report data, and this page renders it
 * as a print-friendly HTML layout. The "Print / Save as PDF" button
 * fires `window.print()`; the operator picks "Save as PDF" in their
 * print dialog.
 *
 * Why this approach instead of pdfkit / puppeteer?
 *
 *   - Zero new dependencies. The browser is the PDF engine.
 *   - The compliance officer is the human reviewer anyway; they have a
 *     browser open already.
 *   - Print-to-PDF produces a better-looking output than most pdfkit
 *     templates because the browser handles fonts, margins, page
 *     breaks, and embedded styles natively.
 *   - Headless server-side generation can be added in a future story
 *     by calling the same /api/admin/compliance/report endpoint from a
 *     cron and piping the JSON through a renderer of the operator's
 *     choosing — no client lock-in.
 */

type RangePreset = '24h' | '7d' | '30d' | 'custom';

function usePresetOptions() {
  const { t } = useTranslation();
  return [
    { value: '24h', label: t('pages:security.complianceReport.presetLast24h', 'Last 24 hours') },
    { value: '7d', label: t('pages:security.complianceReport.presetLast7d', 'Last 7 days') },
    { value: '30d', label: t('pages:security.complianceReport.presetLast30d', 'Last 30 days') },
    { value: 'custom', label: t('pages:security.complianceReport.presetCustom', 'Custom') },
  ];
}

function presetToRange(preset: Exclude<RangePreset, 'custom'>): { start: string; end: string } {
  const end = new Date();
  const start = new Date(end);
  switch (preset) {
    case '24h': start.setHours(start.getHours() - 24); break;
    case '7d': start.setDate(start.getDate() - 7); break;
    case '30d': start.setDate(start.getDate() - 30); break;
  }
  return { start: start.toISOString(), end: end.toISOString() };
}

function toLocalInput(iso: string): string {
  const d = new Date(iso);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function fromLocalInput(local: string): string {
  return new Date(local).toISOString();
}

function formatNumber(n: number): string {
  return n.toLocaleString();
}

const printStyles = `
  @media print {
    .no-print { display: none !important; }
    .printable-report {
      font-family: -apple-system, system-ui, sans-serif;
      color: black;
      background: white;
    }
    .printable-report h1, .printable-report h2 {
      page-break-after: avoid;
    }
    .printable-report .section {
      page-break-inside: avoid;
      margin-bottom: 1.5rem;
    }
    .printable-report table {
      page-break-inside: auto;
    }
    .printable-report tr {
      page-break-inside: avoid;
    }
    a[href]:after {
      content: none !important;
    }
  }
`;

export function ComplianceReportPage() {
  const { t } = useTranslation();
  const PRESET_OPTIONS = usePresetOptions();

  const initial = useMemo(() => presetToRange('30d'), []);
  const [preset, setPreset] = useState<RangePreset>('30d');
  const [startTime, setStartTime] = useState(initial.start);
  const [endTime, setEndTime] = useState(initial.end);
  const [report, setReport] = useState<ComplianceReportResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  const handleGenerate = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const r = await complianceReportApi.get(startTime, endTime);
      setReport(r);
    } catch (err) {
      setError(err instanceof Error ? err : new Error(String(err)));
    } finally {
      setLoading(false);
    }
  }, [startTime, endTime]);

  const handlePresetChange = useCallback((value: string) => {
    const next = value as RangePreset;
    setPreset(next);
    if (next !== 'custom') {
      const { start, end } = presetToRange(next);
      setStartTime(start);
      setEndTime(end);
    }
  }, []);

  const handlePrint = useCallback(() => {
    window.print();
  }, []);

  return (
    <>
      <style>{printStyles}</style>
      <div className="no-print">
        <PageHeader
          title={t('pages:security.complianceReport.title', 'Compliance Report')}
          subtitle={t(
            'pages:security.complianceReport.subtitle',
            'Generate a print-friendly compliance summary for a date range. Use your browser\'s Print dialog to save as PDF for the audit trail.',
          )}
        />
      </div>

      <div className="no-print">
        <Card>
          <Stack gap="sm">
            <div style={{ display: 'flex', gap: 'var(--g-space-3)', alignItems: 'flex-end', flexWrap: 'wrap' }}>
              <label style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-1)', fontSize: 'var(--g-font-size-xs)' }}>
                {t('pages:security.complianceReport.preset', 'Preset')}
                <Select value={preset} onValueChange={handlePresetChange} options={PRESET_OPTIONS} />
              </label>
              <label style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-1)', fontSize: 'var(--g-font-size-xs)' }}>
                {t('pages:security.complianceReport.startTime', 'Start')}
                <Input
                  type="datetime-local"
                  value={toLocalInput(startTime)}
                  onChange={(e) => { setStartTime(fromLocalInput(e.target.value)); setPreset('custom'); }}
                />
              </label>
              <label style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-1)', fontSize: 'var(--g-font-size-xs)' }}>
                {t('pages:security.complianceReport.endTime', 'End')}
                <Input
                  type="datetime-local"
                  value={toLocalInput(endTime)}
                  onChange={(e) => { setEndTime(fromLocalInput(e.target.value)); setPreset('custom'); }}
                />
              </label>
              <Button variant="primary" onClick={handleGenerate} disabled={loading}>
                {loading
                  ? t('pages:security.complianceReport.generating', 'Generating…')
                  : t('pages:security.complianceReport.generate', 'Generate')}
              </Button>
              {report && (
                <Button variant="ghost" onClick={handlePrint}>
                  {t('pages:security.complianceReport.print', 'Print / Save as PDF')}
                </Button>
              )}
            </div>
            <div className={reportStyles.hintText}>
              {t(
                'pages:security.complianceReport.hint',
                'Time window is capped at 366 days. The report is generated on demand and includes coverage, hook health, reject stats, DSAR queue, classification rules, and classification breakdown.',
              )}
            </div>
          </Stack>
        </Card>
      </div>

      {error && <div className="no-print"><ErrorBanner message={error.message} onRetry={handleGenerate} /></div>}
      {loading && !report && <div className="no-print"><LoadingSpinner /></div>}

      {report && (
        <div className="printable-report" style={{ marginTop: 'var(--g-space-6)' }}>
          <h1 style={{ fontSize: 'var(--g-font-size-2xl)', marginBottom: 'var(--g-space-1)' }}>
            {t('pages:security.complianceReport.reportTitle')}
          </h1>
          <div className={reportStyles.periodText}>
            {t('pages:security.complianceReport.periodLabel', { start: new Date(report.period.start).toLocaleString(), end: new Date(report.period.end).toLocaleString() })}
            <br />
            {t('pages:security.complianceReport.generatedLabel', { date: new Date(report.generatedAt).toLocaleString() })}
          </div>

          <div className="section">
            <h2 style={{ fontSize: 'var(--g-font-size-lg)', marginBottom: 'var(--g-space-2)' }}>{t('pages:security.complianceReport.section1Title')}</h2>
            <p style={{ fontSize: 'var(--g-font-size-base)' }}>
              <strong>{formatNumber(report.coverage.totalEvents)}</strong> {t('pages:security.complianceReport.totalEventsLabel', { count: '' }).trim()} |{' '}
              <strong>{report.coverage.coveragePercent.toFixed(2)}%</strong> {t('pages:security.complianceReport.coverageRateLabel', { rate: '' }).trim()}
            </p>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 'var(--g-font-size-base)', marginTop: 'var(--g-space-2)' }}>
              <thead>
                <tr><th className={reportStyles.reportTableTh}>{t('pages:security.complianceReport.colBumpStatus')}</th><th className={reportStyles.reportTableThRight}>{t('pages:security.complianceReport.colCount')}</th></tr>
              </thead>
              <tbody>
                {Object.entries(report.coverage.breakdown).map(([k, v]) => (
                  <tr key={k}>
                    <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }}><code>{k}</code></td>
                    <td style={{ padding: 'var(--g-space-1) var(--g-space-2)', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{formatNumber(v)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          <div className="section">
            <h2 style={{ fontSize: 'var(--g-font-size-lg)', marginBottom: 'var(--g-space-2)' }}>{t('pages:security.complianceReport.section2Title')}</h2>
            <p style={{ fontSize: 'var(--g-font-size-base)' }}>
              <strong>{formatNumber(report.hookHealth.total)}</strong> {t('pages:security.complianceReport.hookEvaluations', { count: '' }).trim()} |{' '}
              {t('pages:security.complianceReport.allow')} {formatNumber(report.hookHealth.byDecision.allow)} ·
              {t('pages:security.complianceReport.deny')} {formatNumber(report.hookHealth.byDecision.deny)} ·
              {t('pages:security.complianceReport.error')} {formatNumber(report.hookHealth.byDecision.error)} ·
              {t('pages:security.complianceReport.unknown')} {formatNumber(report.hookHealth.byDecision.unknown)}
            </p>
            {report.hookHealth.topDenyReasons.length > 0 && (
              <>
                <h3 style={{ fontSize: 'var(--g-font-size-md)', marginTop: 'var(--g-space-3)', marginBottom: 'var(--g-space-1)' }}>{t('pages:security.complianceReport.topDenyReasonCodes')}</h3>
                <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 'var(--g-font-size-base)' }}>
                  <thead><tr><th className={reportStyles.reportTableTh}>{t('pages:security.complianceReport.colReason')}</th><th className={reportStyles.reportTableThRight}>{t('pages:security.complianceReport.colCount')}</th></tr></thead>
                  <tbody>
                    {report.hookHealth.topDenyReasons.map((r) => (
                      <tr key={r.label}>
                        <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }}><code>{r.label}</code></td>
                        <td style={{ padding: 'var(--g-space-1) var(--g-space-2)', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{formatNumber(r.count)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </>
            )}
          </div>

          <div className="section">
            <h2 style={{ fontSize: 'var(--g-font-size-lg)', marginBottom: 'var(--g-space-2)' }}>{t('pages:security.complianceReport.section3Title')}</h2>
            <p style={{ fontSize: 'var(--g-font-size-base)' }}>
              <strong>{formatNumber(report.rejectStats.totalRejects)}</strong> {t('pages:security.complianceReport.totalRejects', { count: '' }).trim()}
            </p>
            {report.rejectStats.topTargets.length > 0 && (
              <>
                <h3 style={{ fontSize: 'var(--g-font-size-md)', marginTop: 'var(--g-space-3)', marginBottom: 'var(--g-space-1)' }}>{t('pages:security.complianceReport.topRejectTargets')}</h3>
                <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 'var(--g-font-size-base)' }}>
                  <thead><tr><th className={reportStyles.reportTableTh}>{t('pages:security.complianceReport.colTargetHost')}</th><th className={reportStyles.reportTableThRight}>{t('pages:security.complianceReport.colCount')}</th></tr></thead>
                  <tbody>
                    {report.rejectStats.topTargets.map((r) => (
                      <tr key={r.label}>
                        <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }}><code>{r.label}</code></td>
                        <td style={{ padding: 'var(--g-space-1) var(--g-space-2)', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{formatNumber(r.count)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </>
            )}
          </div>

          <div className="section">
            <h2 style={{ fontSize: 'var(--g-font-size-lg)', marginBottom: 'var(--g-space-2)' }}>{t('pages:security.complianceReport.section4Title')}</h2>
            <p style={{ fontSize: 'var(--g-font-size-base)' }}>
              {t('pages:security.complianceReport.dsarPending')} <strong>{report.dsar.pending}</strong> ·
              {t('pages:security.complianceReport.dsarInProgress')} <strong>{report.dsar.inProgress}</strong> ·
              {t('pages:security.complianceReport.dsarCompleted')} <strong>{report.dsar.completed}</strong> ·
              {t('pages:security.complianceReport.dsarRejected')} <strong>{report.dsar.rejected}</strong>
              <br />
              {t('pages:security.complianceReport.dsarCompletedInPeriod')} <strong>{report.dsar.completedInPeriod}</strong>
            </p>
          </div>

        </div>
      )}
    </>
  );
}
