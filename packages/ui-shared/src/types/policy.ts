/** Action a PolicyRule may take when matched. */
export type PolicyAction = 'inspect' | 'passthrough' | 'deny';

/**
 * PolicyRule is the per-domain action a Nexus agent applies before
 * forwarding traffic. The same shape is returned by the agent's
 * statusapi (the rules currently loaded into the local policy
 * engine) and the admin API (the rules being pushed via shadow).
 */
export interface PolicyRule {
  /** Domain pattern, e.g. `*.example.com` or an exact host. */
  domain: string;
  action: PolicyAction;
  /** Lower priority values match first. Default 100. */
  priority?: number;
}
