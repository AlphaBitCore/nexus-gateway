/**
 * Compliance report service.
 *
 * Returns a structured JSON snapshot the dashboard renders as a
 * print-friendly HTML page. The compliance officer uses the browser's
 * "Print → Save as PDF" to produce the auditor-ready PDF.
 */

import { api } from '../../client';

export interface ComplianceReportResponse {
  period: { start: string; end: string };
  generatedAt: string;
  generatedBy: string;
  coverage: {
    totalEvents: number;
    bumped: number;
    coveragePercent: number;
    breakdown: Record<string, number>;
  };
  hookHealth: {
    total: number;
    byDecision: { allow: number; deny: number; error: number; unknown: number };
    topDenyReasons: Array<{ label: string; count: number }>;
  };
  rejectStats: {
    totalRejects: number;
    topTargets: Array<{ label: string; count: number }>;
    topReasonCodes: Array<{ label: string; count: number }>;
  };
  dsar: {
    pending: number;
    inProgress: number;
    completed: number;
    rejected: number;
    completedInPeriod: number;
  };
}

export const complianceReportApi = {
  get(startTime: string, endTime: string): Promise<ComplianceReportResponse> {
    const qs = new URLSearchParams({ startTime, endTime }).toString();
    return api.get(`/api/admin/compliance/report?${qs}`);
  },
};
