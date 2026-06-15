import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import type { ConfirmRequest } from './streamChat';

interface ConfirmCardProps {
  pendingConfirm: ConfirmRequest;
  confirmToken: string | null;
  decideConfirm: (decision: boolean) => void;
}

// ConfirmCard renders the confirm-tier write card. A prod Allow is two-step
// : the first Allow surfaces the second-step prompt (confirmToken set by the
// parent) and the next Allow executes. Stateless — all decision logic lives in the
// parent's decideConfirm.
export function ConfirmCard({ pendingConfirm, confirmToken, decideConfirm }: ConfirmCardProps) {
  const { t } = useTranslation();
  return (
    <div className="border-t border-border bg-muted/40 p-3 text-sm" role="alertdialog" aria-label={t('common:assistant.confirmTitle')}>
      <p className="mb-1 font-semibold">
        {pendingConfirm.prod ? '⚠ ' : ''}
        {t('common:assistant.confirmTitle')}
      </p>
      {pendingConfirm.reason && <p className="mb-1 text-muted-foreground">{pendingConfirm.reason}</p>}
      <code className="mb-2 block overflow-x-auto whitespace-pre-wrap rounded bg-background px-2 py-1 text-xs">
        {`${pendingConfirm.tool}(${JSON.stringify(pendingConfirm.input)})`}
      </code>
      {pendingConfirm.preview && (
        <div className="mb-2 rounded border border-border bg-background p-2 text-xs">
          <p className="mb-1 font-semibold">{t('common:assistant.impactTitle')}</p>
          {pendingConfirm.preview.unavailable ? (
            <p className="text-destructive">{pendingConfirm.preview.note}</p>
          ) : (
            <>
              {pendingConfirm.preview.summary && <p className="mb-1">{pendingConfirm.preview.summary}</p>}
              {pendingConfirm.preview.irreversible && (
                <p className="mb-1 font-semibold text-destructive">{t('common:assistant.impactIrreversible')}</p>
              )}
              {pendingConfirm.preview.current && (
                <code className="block overflow-x-auto whitespace-pre-wrap text-muted-foreground">
                  {JSON.stringify(pendingConfirm.preview.current)}
                </code>
              )}
            </>
          )}
        </div>
      )}
      {pendingConfirm.prod && !confirmToken && <p className="mb-2 text-destructive">{t('common:assistant.confirmProd')}</p>}
      {confirmToken && <p className="mb-2 font-semibold text-destructive">{t('common:assistant.confirmSecondStep')}</p>}
      <div className="flex gap-2">
        <button
          type="button"
          onClick={() => decideConfirm(true)}
          className={cn(
            'rounded-md px-3 py-1 text-sm',
            pendingConfirm.prod ? 'bg-destructive text-destructive-foreground' : 'bg-primary text-primary-foreground',
          )}
        >
          {confirmToken ? t('common:assistant.confirmExecute') : t('common:assistant.allow')}
        </button>
        <button
          type="button"
          onClick={() => decideConfirm(false)}
          className="rounded-md border border-border px-3 py-1 text-sm hover:bg-muted"
        >
          {t('common:assistant.deny')}
        </button>
      </div>
    </div>
  );
}
