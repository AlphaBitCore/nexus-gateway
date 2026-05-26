/**
 * MiniLineChart — minimal inline SVG line chart for the Stats page.
 *
 * Deliberately NOT Recharts: the Dashboard ships inside a Wails
 * WebView and Recharts (+ its React 19 friction) would add ~200 KB
 * gzipped to a bundle that only needs one chart. The implementation
 * is intentionally minimal — single line, no axes, no tooltip, no
 * theme tokens beyond what CSS variables provide — because all we
 * want here is "shape of recent activity at a glance". For richer
 * charts the admin Dashboard at CP UI's /devices/:id Stats tab is
 * the right surface.
 */

interface Point {
  /** ISO timestamp; only used for tooltip on hover. */
  bucket: string;
  value: number;
}

interface Props {
  data: Point[];
  /** Pixel height of the chart area. */
  height?: number;
  /** Stroke color; falls back to a CSS variable. */
  stroke?: string;
  /** Accessible label for screen readers. */
  ariaLabel?: string;
}

export function MiniLineChart({
  data,
  height = 120,
  stroke = 'var(--color-primary)',
  ariaLabel,
}: Props) {
  if (!data || data.length === 0) {
    return (
      <div
        style={{
          height,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          color: 'var(--color-text-muted)',
          fontStyle: 'italic',
          fontSize: 'var(--g-font-size-sm)',
        }}
      >
        No data in window
      </div>
    );
  }

  // Plot in a unit box so we can ignore container width — the parent
  // sets that via CSS. SVG `preserveAspectRatio="none"` stretches the
  // path to fit any width while keeping the height we declare.
  const width = 1000;
  const padding = 4;
  const innerW = width - padding * 2;
  const innerH = height - padding * 2;

  const values = data.map((d) => d.value);
  const max = Math.max(1, ...values);
  const min = Math.min(0, ...values);
  const range = max - min || 1;

  // Map (i, value) to (x, y) in svg space.
  const points = data.map((d, i) => {
    const x =
      data.length === 1
        ? padding + innerW / 2
        : padding + (i / (data.length - 1)) * innerW;
    const y = padding + (1 - (d.value - min) / range) * innerH;
    return { x, y, d };
  });

  const path = points
    .map((p, i) => `${i === 0 ? 'M' : 'L'} ${p.x.toFixed(2)} ${p.y.toFixed(2)}`)
    .join(' ');

  // Area fill helps the visual when there's just a small line —
  // closes the path to the baseline and lets us shade it lightly.
  const area =
    `M ${points[0].x.toFixed(2)} ${(padding + innerH).toFixed(2)} ` +
    points.map((p) => `L ${p.x.toFixed(2)} ${p.y.toFixed(2)}`).join(' ') +
    ` L ${points[points.length - 1].x.toFixed(2)} ${(padding + innerH).toFixed(2)} Z`;

  return (
    <svg
      role="img"
      aria-label={ariaLabel}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      width="100%"
      height={height}
      style={{ display: 'block' }}
    >
      <path d={area} fill={stroke} opacity={0.08} />
      <path
        d={path}
        fill="none"
        stroke={stroke}
        strokeWidth={1.5}
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}

export default MiniLineChart;
