/**
 * Sparkline — tiny inline SVG trend chart for metric cards.
 *
 * Usage:
 *   <Sparkline data={[12, 15, 8, 22, 18, 25, 20]} />
 *   <Sparkline data={values} color="var(--color-success)" height={32} />
 */
import styles from './Sparkline.module.css';

export interface SparklineProps {
  /** Array of numeric values (min 2 points). */
  data: number[];
  /** SVG width. @default 80 */
  width?: number;
  /** SVG height. @default 28 */
  height?: number;
  /** Stroke color. @default 'var(--color-primary)' */
  color?: string;
  /** Show a filled area under the line. @default true */
  filled?: boolean;
  className?: string;
}

export function Sparkline({
  data,
  width = 80,
  height = 28,
  color = 'var(--color-primary)',
  filled = true,
  className,
}: SparklineProps) {
  if (!data || data.length < 2) return null;

  const padding = 2;
  const w = width - padding * 2;
  const h = height - padding * 2;

  const min = Math.min(...data);
  const max = Math.max(...data);
  const range = max - min || 1;

  const points = data.map((v, i) => ({
    x: padding + (i / (data.length - 1)) * w,
    y: padding + h - ((v - min) / range) * h,
  }));

  const linePath = points.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x.toFixed(1)},${p.y.toFixed(1)}`).join(' ');

  const areaPath = filled
    ? `${linePath} L${points[points.length - 1].x.toFixed(1)},${(height - padding).toFixed(1)} L${padding},${(height - padding).toFixed(1)} Z`
    : '';

  // Trend direction for aria
  const trend = data[data.length - 1] > data[0] ? 'up' : data[data.length - 1] < data[0] ? 'down' : 'flat';

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={className}
      role="img"
      aria-label={`Trend: ${trend}`}
    >
      {filled && (
        <path
          d={areaPath}
          fill={color}
          className={styles.area}
        />
      )}
      <path
        d={linePath}
        fill="none"
        stroke={color}
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        className={styles.line}
      />
      {/* End dot */}
      <circle
        cx={points[points.length - 1].x}
        cy={points[points.length - 1].y}
        r="2"
        fill={color}
        className={styles.dot}
      />
    </svg>
  );
}
