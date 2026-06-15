import { useCallback, useState } from 'react';
import { confirmDecision } from './streamChat';
import type { ConfirmRequest, ConfirmResult } from './streamChat';

// useConfirmFlow owns the confirm-gated-write state for the chat widget: the
// pending confirm card and the production second-confirm challenge token
// . The component arms it from the SSE confirm event (armConfirm) and
// clears it on close (clearConfirm); decideConfirm resolves the card.

// confirmErrKey maps a failed confirm result to the right i18n message: another
// instance owns the session (421), the pod restarted so re-issue, or a plain
// expiry/timeout.
function confirmErrKey(r: ConfirmResult): string {
  if (r.misrouted) return 'common:assistant.confirmMisrouted';
  if (r.reissue) return 'common:assistant.confirmReissue';
  return 'common:assistant.confirmExpired';
}

export interface ConfirmFlow {
  pendingConfirm: ConfirmRequest | null;
  /** Non-null once the backend issued a prod second-confirm challenge token. */
  confirmToken: string | null;
  /** Arm the card for a new confirm event (resets any prior second-step token). */
  armConfirm: (req: ConfirmRequest) => void;
  /** Drop any parked confirm so a reopened popup never resurrects a stale card. */
  clearConfirm: () => void;
  decideConfirm: (decision: boolean) => Promise<void>;
}

export function useConfirmFlow(onError: (i18nKey: string) => void): ConfirmFlow {
  const [pendingConfirm, setPendingConfirm] = useState<ConfirmRequest | null>(null);
  const [confirmToken, setConfirmToken] = useState<string | null>(null);

  const armConfirm = useCallback((req: ConfirmRequest) => {
    setConfirmToken(null); // new confirm → reset any prior second-step token
    setPendingConfirm(req);
  }, []);

  const clearConfirm = useCallback(() => {
    setPendingConfirm(null);
    setConfirmToken(null);
  }, []);

  // Resolve a confirm-tier write. Deny (or a non-prod Allow) ends the card. A prod
  // Allow is two-step: the first Allow returns a challenge token and the card
  // stays open in second-step mode; the next Allow echoes the token to execute. The
  // turn, blocked server-side, resumes only once the write is actually resolved.
  const decideConfirm = useCallback(
    async (decision: boolean) => {
      if (!pendingConfirm) return;
      const { sessionId: sid, callId } = pendingConfirm;
      if (!decision) {
        setPendingConfirm(null);
        setConfirmToken(null);
        void confirmDecision(sid, callId, false);
        return;
      }
      if (confirmToken) {
        // Second step: echo the server-issued token to execute the prod write. Await
        // it so we can tell the user the truth if the confirmation already expired
        // (turn timed out) — otherwise the card would just close and they'd wrongly
        // believe the production write ran.
        setPendingConfirm(null);
        setConfirmToken(null);
        const second = await confirmDecision(sid, callId, true, confirmToken);
        if (!second.ok) {
          onError(confirmErrKey(second));
        }
        return;
      }
      // First (or only) Allow. In prod the backend withholds execution and returns a
      // one-time challenge token; keep the card open for the second confirm.
      const result = await confirmDecision(sid, callId, true);
      if (result.secondConfirmRequired && result.challengeToken) {
        setConfirmToken(result.challengeToken);
      } else {
        setPendingConfirm(null);
        // A non-prod single Allow that the backend rejected (expired/timed out, or a
        // 421 affinity miss) did not run — surface the right reason rather than
        // silently closing the card.
        if (!result.ok) {
          onError(confirmErrKey(result));
        }
      }
    },
    [pendingConfirm, confirmToken, onError],
  );

  return { pendingConfirm, confirmToken, armConfirm, clearConfirm, decideConfirm };
}
