/**
 * Skeleton loading primitives — shimmer placeholders that match page layouts.
 *
 * Usage:
 *   <Skeleton.Line />                     — text line
 *   <Skeleton.Heading />                  — heading (wider, taller)
 *   <Skeleton.Box width={200} height={40} /> — arbitrary box
 *   <Skeleton.Circle size={36} />         — avatar / icon
 *   <Skeleton.Card lines={4} />           — card with N shimmer lines
 *   <Skeleton.MetricsRow count={4} />     — row of metric cards
 *   <Skeleton.Table rows={5} cols={4} />  — table with header + rows
 */
import styles from './Skeleton.module.css';
import { Stack } from '../Stack';

function Line({ width }: { width?: string | number }) {
  return <div className={styles.line} style={width ? { width } : undefined} />;
}

function Heading({ width }: { width?: string | number }) {
  return <div className={styles.heading} style={width ? { width } : undefined} />;
}

function Box({ width, height }: { width: number | string; height: number | string }) {
  return <div className={styles.base} style={{ width, height }} />;
}

function Circle({ size = 36 }: { size?: number }) {
  return <div className={styles.circle} style={{ width: size, height: size }} />;
}

function Card({ lines = 3 }: { lines?: number }) {
  return (
    <div className={styles.cardSkeleton}>
      <Heading />
      {Array.from({ length: lines }).map((_, i) => (
        <Line key={i} width={`${70 + ((i * 17) % 30)}%`} />
      ))}
    </div>
  );
}

function MetricsRow({ count = 4 }: { count?: number }) {
  return (
    <div className={styles.metricsRow}>
      {Array.from({ length: count }).map((_, i) => (
        <div key={i} className={styles.cardSkeleton}>
          <Line width="60%" />
          <Box width="50%" height={28} />
          <Line width="80%" />
        </div>
      ))}
    </div>
  );
}

function Table({ rows = 5, cols = 4 }: { rows?: number; cols?: number }) {
  return (
    <div className={styles.cardSkeleton} style={{ padding: 'var(--g-space-0)' }}>
      {/* Header */}
      <div className={styles.tableRow} style={{ padding: 'var(--g-space-3) var(--g-space-4)' }}>
        {Array.from({ length: cols }).map((_, i) => (
          <div key={i} className={styles.tableCell} style={{ height: 12, opacity: 0.6 }} />
        ))}
      </div>
      {/* Rows */}
      {Array.from({ length: rows }).map((_, rowIdx) => (
        <div key={rowIdx} className={styles.tableRow} style={{ padding: 'var(--g-space-3) var(--g-space-4)' }}>
          {Array.from({ length: cols }).map((_, colIdx) => (
            <div
              key={colIdx}
              className={styles.tableCell}
              style={{ width: `${60 + ((rowIdx * 17 + colIdx * 23) % 30)}%` }}
            />
          ))}
        </div>
      ))}
    </div>
  );
}

/** Page-level skeleton: header + metrics + table (matches DashboardPage layout) */
function DashboardSkeleton() {
  return (
    <Stack gap="lg">
      <div className={styles.pageHeader}>
        <Heading width="25%" />
      </div>
      <MetricsRow count={4} />
      <MetricsRow count={4} />
      <Card lines={2} />
      <Table rows={5} cols={5} />
    </Stack>
  );
}

/** Page-level skeleton: header + toolbar + table (matches list pages) */
function ListPageSkeleton() {
  return (
    <Stack gap="lg">
      <div className={styles.pageHeader}>
        <Heading width="30%" />
        <Line width="50%" />
      </div>
      <Box width="100%" height={44} />
      <Table rows={8} cols={5} />
    </Stack>
  );
}

/** Page-level skeleton: header + card with fields (matches detail/edit pages) */
function DetailPageSkeleton() {
  return (
    <Stack gap="lg">
      <Line width={120} />
      <div className={styles.pageHeader}>
        <Heading width="35%" />
        <Line width="45%" />
      </div>
      <Card lines={6} />
      <Card lines={4} />
    </Stack>
  );
}

export const Skeleton = {
  Line,
  Heading,
  Box,
  Circle,
  Card,
  MetricsRow,
  Table,
  DashboardSkeleton,
  ListPageSkeleton,
  DetailPageSkeleton,
};
