import type { MouseEventHandler, ReactNode } from 'react';
import { Button, IconButton, LinkButton } from '@nexus-gateway/ui-shared';

import { Tooltip } from '../Tooltip';
import styles from './RowActions.module.css';

export interface RowActionsProps {
  children: ReactNode;
  variant?: 'icon' | 'text';
}

export function RowActions({ children, variant = 'icon' }: RowActionsProps) {
  return (
    <div
      className={`${styles.rowActions} ${variant === 'text' ? styles.rowActionsText : ''}`}
      onClick={(event) => event.stopPropagation()}
    >
      {children}
    </div>
  );
}

export interface RowActionButtonProps {
  label: string;
  onAction: () => void;
  children: ReactNode;
  disabled?: boolean;
  tone?: 'default' | 'danger';
}

export function RowActionIconButton({
  label,
  onAction,
  children,
  disabled = false,
  tone = 'default',
}: RowActionButtonProps) {
  const handleClick: MouseEventHandler<HTMLButtonElement> = (event) => {
    event.stopPropagation();
    onAction();
  };

  const button = (
    <IconButton
      size="sm"
      variant="subtle"
      className={`${styles.iconButton} ${tone === 'danger' ? styles.iconButtonDanger : ''}`}
      aria-label={label}
      title={label}
      disabled={disabled}
      onClick={handleClick}
    >
      {children}
    </IconButton>
  );

  if (disabled) {
    return (
      <Tooltip content={label} side="top" delayDuration={200}>
        <span className={styles.tooltipTrigger}>{button}</span>
      </Tooltip>
    );
  }

  return (
    <Tooltip content={label} side="top" delayDuration={200}>
      {button}
    </Tooltip>
  );
}

export interface RowActionTextButtonProps {
  label: string;
  onAction: () => void;
  disabled?: boolean;
}

export function RowActionTextButton({ label, onAction, disabled = false }: RowActionTextButtonProps) {
  const handleClick: MouseEventHandler<HTMLButtonElement> = (event) => {
    event.stopPropagation();
    onAction();
  };

  return (
    <Button
      variant="secondary"
      size="sm"
      className={styles.textButton}
      disabled={disabled}
      onClick={handleClick}
    >
      {label}
    </Button>
  );
}

export interface RowActionTextLinkButtonProps {
  label: string;
  onAction: () => void;
  disabled?: boolean;
  tone?: 'default' | 'danger';
}

export function RowActionTextLinkButton({
  label,
  onAction,
  disabled = false,
  tone = 'default',
}: RowActionTextLinkButtonProps) {
  const handleClick: MouseEventHandler<HTMLButtonElement> = (event) => {
    event.stopPropagation();
    onAction();
  };

  return (
    <LinkButton
      className={`${styles.linkButton} ${tone === 'danger' ? styles.linkButtonDanger : ''}`}
      disabled={disabled}
      onClick={handleClick}
    >
      {label}
    </LinkButton>
  );
}

export interface RowActionTerminalProps {
  children: ReactNode;
}

export function RowActionTerminal({ children }: RowActionTerminalProps) {
  return <span className={styles.terminal}>{children}</span>;
}

export function OpenActionIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <line x1="5" y1="12" x2="19" y2="12" />
      <polyline points="12 5 19 12 12 19" />
    </svg>
  );
}

export function RevokeActionIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <circle cx="12" cy="12" r="10" />
      <line x1="4.93" y1="4.93" x2="19.07" y2="19.07" />
    </svg>
  );
}

export function EditActionIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M12 20h9" />
      <path d="M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4Z" />
    </svg>
  );
}

export function DeleteActionIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <polyline points="3 6 5 6 21 6" />
      <path d="M19 6l-2 14a2 2 0 0 1-2 2H9a2 2 0 0 1-2-2L5 6" />
      <path d="M10 11v6" />
      <path d="M14 11v6" />
      <path d="M9 6V4a2 2 0 0 1 2-2h2a2 2 0 0 1 2 2v2" />
    </svg>
  );
}

export function RowDeleteAction({
  label,
  onAction,
  disabled,
}: {
  label: string;
  onAction: () => void;
  disabled?: boolean;
}) {
  return (
    <RowActionIconButton label={label} tone="danger" disabled={disabled} onAction={onAction}>
      <DeleteActionIcon />
    </RowActionIconButton>
  );
}
