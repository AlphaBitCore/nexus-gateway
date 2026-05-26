/**
 * GenericRenderer — safe fallback used for any rule ID that doesn't have a
 * bespoke renderer registered.
 *
 * Preserves the pre-registry behaviour of `AlertDetailDrawer` by rendering
 * `alert.details` as a pretty-printed JSON block. No field-by-field guessing —
 * operators can inspect the raw payload to decide whether a rule deserves a
 * dedicated renderer.
 */
import styles from './renderer.module.css';
import type { DetailRendererProps } from './types';

export function GenericRenderer({ alert }: DetailRendererProps) {
  return (
    <pre className={styles.jsonBlock}>
      {JSON.stringify(alert.details ?? {}, null, 2)}
    </pre>
  );
}
