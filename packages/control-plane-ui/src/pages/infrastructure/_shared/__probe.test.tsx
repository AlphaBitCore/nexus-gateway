import { describe, it, expect } from 'vitest';
import styles from '@/pages/infrastructure/nodes/InfraNodesPage.module.css';

describe('CSS modules sanity', () => {
  it('exports dim class', () => {
    console.log('styles.dim =', JSON.stringify(styles.dim));
    console.log('styles keys =', Object.keys(styles).join(', '));
    expect(styles.dim).toBeDefined();
  });
});
