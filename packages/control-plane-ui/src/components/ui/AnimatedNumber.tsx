import { useCountUp } from '@/hooks/useCountUp';

export interface AnimatedNumberProps {
  value: number;
  // Decimals of the input value to preserve through the integer-only useCountUp
  // animation. e.g. precision=1 animates "37.4" via the integer sequence 0..374.
  precision?: number;
  format?: (raw: number) => string;
  duration?: number;
}

const defaultFormat = (n: number) => n.toLocaleString();

export function AnimatedNumber({
  value,
  precision = 0,
  format = defaultFormat,
  duration,
}: AnimatedNumberProps) {
  const safe = Number.isFinite(value) ? value : 0;
  const scale = 10 ** precision;
  const animated = useCountUp(Math.round(safe * scale), duration);
  return <>{format(animated / scale)}</>;
}
