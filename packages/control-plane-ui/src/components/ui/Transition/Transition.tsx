import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import clsx from 'clsx';

export interface TransitionProps {
  /** Whether children are visible. */
  show: boolean;
  /** CSS class applied on enter. @default 'anim-fade-in' */
  enter?: string;
  /** CSS class applied on exit. @default 'anim-fade-out' */
  exit?: string;
  /** Content to animate. */
  children: ReactNode;
  /** Additional class name. */
  className?: string;
}

/**
 * Returns true when the user prefers reduced motion.
 * Evaluated once per mount to avoid layout thrashing.
 */
function prefersReducedMotion(): boolean {
  if (typeof window === 'undefined') return false;
  return window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

export function Transition({
  show,
  enter = 'anim-fade-in',
  exit = 'anim-fade-out',
  children,
  className,
}: TransitionProps) {
  const [mounted, setMounted] = useState(show);
  const [animClass, setAnimClass] = useState(show ? enter : '');
  const wrapperRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (show) {
      setMounted(true);
      setAnimClass(enter);
    } else if (mounted) {
      // Begin exit
      if (prefersReducedMotion()) {
        setMounted(false);
        return;
      }
      setAnimClass(exit);
    }
  }, [show, enter, exit, mounted]);

  const handleAnimationEnd = useCallback(() => {
    if (!show) {
      setMounted(false);
    }
  }, [show]);

  if (!mounted) return null;

  return (
    <div
      ref={wrapperRef}
      className={clsx(animClass, className)}
      onAnimationEnd={handleAnimationEnd}
    >
      {children}
    </div>
  );
}
