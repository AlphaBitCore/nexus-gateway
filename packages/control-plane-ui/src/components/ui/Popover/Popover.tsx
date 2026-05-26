import * as RadixPopover from '@radix-ui/react-popover';
import clsx from 'clsx';
import styles from './Popover.module.css';

export const Popover = RadixPopover.Root;
export const PopoverTrigger = RadixPopover.Trigger;

export interface PopoverContentProps
  extends React.ComponentPropsWithoutRef<typeof RadixPopover.Content> {
  className?: string;
  children: React.ReactNode;
}

export function PopoverContent({
  className,
  children,
  side,
  sideOffset = 4,
  align,
  ...props
}: PopoverContentProps) {
  return (
    <RadixPopover.Portal>
      <RadixPopover.Content
        className={clsx(styles.content, className)}
        side={side}
        sideOffset={sideOffset}
        align={align}
        {...props}
      >
        {children}
      </RadixPopover.Content>
    </RadixPopover.Portal>
  );
}
