const DEFAULT_AI_GATEWAY_BASE_URL = 'http://localhost:3050';
const AIGUARD_COMPLIANCE_WEBHOOK_PATH = '/v1/ai-guard/compliance-webhook';

function normalizeBaseUrl(raw: string): string {
  return raw.trim().replace(/\/+$/, '');
}

export function defaultAIGatewayBaseUrl(): string {
  if (typeof window === 'undefined') return DEFAULT_AI_GATEWAY_BASE_URL;
  const host = window.location.hostname || 'localhost';
  return `http://${host}:3050`;
}

export function aiguardComplianceWebhookUrl(baseUrl?: string): string {
  const base = normalizeBaseUrl(baseUrl || defaultAIGatewayBaseUrl());
  return `${base}${AIGUARD_COMPLIANCE_WEBHOOK_PATH}`;
}

