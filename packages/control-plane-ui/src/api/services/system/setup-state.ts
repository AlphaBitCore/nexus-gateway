/**
 * Setup wizard state service.
 *
 * Reads / writes the operator's setup-wizard completion state from the
 * SystemMetadata-backed BFF endpoint. Replaces the legacy localStorage
 * primary store so wizard progress survives across browsers and admin
 * users.
 */

import { api } from '../../client';

export interface SetupState {
  completed: boolean;
  steps: Record<string, boolean>;
  updatedAt: string | null;
  updatedBy: string | null;
}

export const setupStateApi = {
  get(): Promise<SetupState> {
    return api.get('/api/admin/setup-state');
  },
  put(state: { completed: boolean; steps: Record<string, boolean> }): Promise<SetupState> {
    return api.put('/api/admin/setup-state', state);
  },
};
