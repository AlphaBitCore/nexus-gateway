import * as RadixTabs from '@radix-ui/react-tabs';
import clsx from 'clsx';
import styles from './Tabs.module.css';

export const Tabs = RadixTabs.Root;

export interface TabsListProps
  extends React.ComponentPropsWithoutRef<typeof RadixTabs.List> {
  className?: string;
}

export function TabsList({ className, ...props }: TabsListProps) {
  return <RadixTabs.List className={clsx(styles.list, className)} {...props} />;
}

export interface TabsTriggerProps
  extends React.ComponentPropsWithoutRef<typeof RadixTabs.Trigger> {
  className?: string;
}

export function TabsTrigger({ className, ...props }: TabsTriggerProps) {
  return <RadixTabs.Trigger className={clsx(styles.trigger, className)} {...props} />;
}

export interface TabsContentProps
  extends React.ComponentPropsWithoutRef<typeof RadixTabs.Content> {
  className?: string;
}

export function TabsContent({ className, ...props }: TabsContentProps) {
  return <RadixTabs.Content className={clsx(styles.content, className)} {...props} />;
}
