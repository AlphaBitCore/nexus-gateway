import { type ClassValue, clsx } from 'clsx';
import { twMerge } from 'tailwind-merge';

/** Merge Tailwind class names; shared by shadcn-style components in ui-shared. */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
