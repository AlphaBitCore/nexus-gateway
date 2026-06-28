import { useTranslation } from 'react-i18next';
import {
  Button,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui';
import styles from './ConfigurationTab.module.css';

export interface ConfigurationToolbarProps {
  desiredVer: number;
  reportedVer: number;
  overrideCount: number;
  staleCount: number;
  resyncingAll: boolean;
  resyncingKey: string | null;
  handleResyncAll: () => void;
  addableKeys: string[];
  openEditor: (mode: 'add' | 'edit', configKey: string) => void;
}

export function ConfigurationToolbar({
  desiredVer,
  reportedVer,
  overrideCount,
  staleCount,
  resyncingAll,
  resyncingKey,
  handleResyncAll,
  addableKeys,
  openEditor,
}: ConfigurationToolbarProps) {
  const { t } = useTranslation();

  return (
    <div className={styles.toolbar}>
      <div className={styles.counters}>
        <span>
          {t('pages:infrastructure.configuration.targetApplied', {
            target: desiredVer,
            applied: reportedVer,
          })}
        </span>
        <span>
          <span className={styles.counterDot}>{'·'}</span>
          {t('pages:infrastructure.configuration.nKeysOverridden', {
            count: overrideCount,
          })}
        </span>
        {staleCount > 0 && (
          <span>
            <span className={styles.counterDot}>{'·'}</span>
            {t('pages:infrastructure.configuration.nStale', {
              count: staleCount,
            })}
          </span>
        )}
      </div>
      <div className={styles.toolbarActions}>
        <Button
          variant="primary"
          size="sm"
          className={styles.toolbarButton}
          loading={resyncingAll}
          disabled={resyncingKey !== null}
          onClick={handleResyncAll}
        >
          {t('pages:infrastructure.configuration.forceResyncAll')}
        </Button>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              variant="secondary"
              size="sm"
              className={styles.toolbarButton}
              disabled={addableKeys.length === 0}
            >
              {t('pages:infrastructure.configuration.addOverride')}
              {' ▾'}
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start">
            {addableKeys.map((k) => (
              <DropdownMenuItem
                key={k}
                onSelect={() => openEditor('add', k)}
              >
                <code>{k}</code>
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </div>
  );
}
