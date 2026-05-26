import * as React from 'react';
import { cva, type VariantProps } from 'class-variance-authority';
import { Slot } from 'radix-ui';

import { cn } from '../lib/cn';

const buttonVariants = cva(
  "inline-flex shrink-0 cursor-pointer select-none items-center justify-center gap-[var(--button-gap)] rounded-[var(--button-radius)] border border-transparent text-base font-medium leading-6 whitespace-nowrap transition-[color,background-color,border-color,box-shadow,transform,filter] duration-150 outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 active:scale-[0.97] active:duration-100 disabled:pointer-events-none disabled:opacity-70 disabled:active:scale-100 aria-invalid:border-destructive aria-invalid:ring-destructive/20 dark:aria-invalid:ring-destructive/40 [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  {
    variants: {
      variant: {
        default:
          'border-[var(--color-primary)] bg-[var(--color-primary)] text-[var(--color-primary-foreground)] hover:border-[var(--color-primary-hover)] hover:bg-[var(--color-primary-hover)] active:border-[var(--color-primary-hover)] active:bg-[var(--color-primary-hover)]',
        destructive:
          'border-[var(--color-danger)] bg-[var(--color-danger)] text-[var(--g-white)] hover:border-[var(--color-danger-dark)] hover:bg-[var(--color-danger-dark)] active:border-[var(--color-danger-dark)] active:bg-[var(--color-danger-dark)] focus-visible:ring-destructive/20 dark:focus-visible:ring-destructive/40',
        outline:
          'border-[var(--color-border-strong)] bg-[var(--color-surface-raised)] text-[var(--color-text-primary)] hover:bg-[var(--color-surface-hover)] active:bg-[var(--color-surface-hover)]',
        secondary:
          'border-[var(--color-border-strong)] bg-[var(--color-surface-raised)] text-[var(--color-text-primary)] hover:bg-[var(--color-surface-hover)] active:bg-[var(--color-surface-hover)]',
        ghost:
          'text-[var(--color-text-secondary)] hover:bg-[var(--color-surface-hover)] hover:text-[var(--color-text-primary)] active:bg-[var(--color-surface-hover)]',
        link: 'scale-100 text-primary underline-offset-4 hover:underline active:scale-100 active:opacity-80',
      },
      size: {
        default: 'h-[var(--button-height-md)] px-[var(--button-padding-x-md)] py-2 has-[>svg]:px-[var(--button-padding-x-md)]',
        xs: "h-6 gap-1 rounded-[var(--button-radius-sm)] px-2 text-xs has-[>svg]:px-1.5 [&_svg:not([class*='size-'])]:size-3",
        sm: 'h-[var(--button-height-sm)] gap-1 rounded-[var(--button-radius-sm)] px-[var(--button-padding-x-sm)] text-xs has-[>svg]:px-[var(--button-padding-x-sm)]',
        lg: 'h-[var(--button-height-lg)] rounded-[var(--button-radius)] px-[var(--button-padding-x-lg)] has-[>svg]:px-[var(--button-padding-x-lg)]',
        icon: 'size-[var(--button-height-md)]',
        'icon-xs': "size-6 rounded-[var(--button-radius-sm)] [&_svg:not([class*='size-'])]:size-3",
        'icon-sm': 'size-[var(--button-height-sm)] rounded-[var(--button-radius-sm)]',
        'icon-lg': 'size-[var(--button-height-lg)]',
      },
    },
    defaultVariants: {
      variant: 'default',
      size: 'default',
    },
  },
);

export type ShadcnButtonProps = React.ComponentPropsWithoutRef<'button'>
  & VariantProps<typeof buttonVariants> & {
    asChild?: boolean;
  };

/**
 * shadcn-style Button aligned with prime-console. Prefer this for new Nexus UI;
 * legacy `@nexus-gateway/ui-shared` Button remains for gradual migration.
 */
const ShadcnButton = React.forwardRef<HTMLButtonElement, ShadcnButtonProps>(({
  className,
  variant = 'default',
  size = 'default',
  asChild = false,
  ...props
}, ref) => {
  const Comp = asChild ? Slot.Root : 'button';

  return (
    <Comp
      data-slot="button"
      data-variant={variant}
      data-size={size}
      className={cn(buttonVariants({ variant, size, className }))}
      ref={ref}
      {...props}
    />
  );
});
ShadcnButton.displayName = 'ShadcnButton';

export { ShadcnButton, buttonVariants };
