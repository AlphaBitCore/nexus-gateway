import * as RadixDropdownMenu from '@radix-ui/react-dropdown-menu';
import clsx from 'clsx';
import styles from './DropdownMenu.module.css';

export const DropdownMenu = RadixDropdownMenu.Root;
export const DropdownMenuTrigger = RadixDropdownMenu.Trigger;

export interface DropdownMenuContentProps
  extends React.ComponentPropsWithoutRef<typeof RadixDropdownMenu.Content> {
  className?: string;
  children: React.ReactNode;
}

export function DropdownMenuContent({
  className,
  children,
  sideOffset = 4,
  ...props
}: DropdownMenuContentProps) {
  return (
    <RadixDropdownMenu.Portal>
      <RadixDropdownMenu.Content
        className={clsx(styles.content, className)}
        sideOffset={sideOffset}
        {...props}
      >
        {children}
      </RadixDropdownMenu.Content>
    </RadixDropdownMenu.Portal>
  );
}

export interface DropdownMenuItemProps
  extends React.ComponentPropsWithoutRef<typeof RadixDropdownMenu.Item> {
  className?: string;
}

export function DropdownMenuItem({ className, ...props }: DropdownMenuItemProps) {
  return <RadixDropdownMenu.Item className={clsx(styles.item, className)} {...props} />;
}

export interface DropdownMenuSeparatorProps
  extends React.ComponentPropsWithoutRef<typeof RadixDropdownMenu.Separator> {
  className?: string;
}

export function DropdownMenuSeparator({ className, ...props }: DropdownMenuSeparatorProps) {
  return (
    <RadixDropdownMenu.Separator className={clsx(styles.separator, className)} {...props} />
  );
}
