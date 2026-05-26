import { useTranslation } from 'react-i18next';
import { Stack } from '../Stack';
import { Switch } from '../Switch';
import styles from './ListEnabledSwitchCell.module.css';

export interface ListEnabledSwitchCellProps {
  enabled: boolean;
  /** When false, only the status badge is shown. */
  canToggle: boolean;
  /** Disable the switch while a mutation is in flight. */
  toggleDisabled?: boolean;
  onToggle: (nextEnabled: boolean) => void;
  /** Accessible name for the switch (e.g. entity name). */
  ariaLabel: string;
}

/**
 * List row control for enabled/disabled: switch + badge when editable,
 * badge-only when read-only. Matches the interception domains list pattern.
 */
export function ListEnabledSwitchCell({
  enabled,
  canToggle,
  toggleDisabled = false,
  onToggle,
  ariaLabel,
}: ListEnabledSwitchCellProps) {
  const { t } = useTranslation();

  const statusText = (
    <span className={enabled ? styles.statusEnabled : styles.statusDisabled}>
      {enabled ? t('common:enabled') : t('common:disabled')}
    </span>
  );

  if (!canToggle) {
    return statusText;
  }

  return (
    <Stack
      direction="horizontal"
      gap="sm"
      align="center"
      onClick={(e) => e.stopPropagation()}
    >
      <Switch
        checked={enabled}
        disabled={toggleDisabled}
        aria-label={ariaLabel}
        onCheckedChange={(next) => {
          if (next === enabled) return;
          onToggle(next);
        }}
      />
      {statusText}
    </Stack>
  );
}
