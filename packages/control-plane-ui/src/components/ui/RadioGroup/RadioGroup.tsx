import * as RadixRadioGroup from '@radix-ui/react-radio-group';
import clsx from 'clsx';
import styles from './RadioGroup.module.css';

export interface RadioGroupProps {
  value: string;
  onValueChange: (value: string) => void;
  className?: string;
  children: React.ReactNode;
}

export function RadioGroup({ value, onValueChange, className, children }: RadioGroupProps) {
  return (
    <RadixRadioGroup.Root
      value={value}
      onValueChange={onValueChange}
      className={clsx(styles.root, className)}
    >
      {children}
    </RadixRadioGroup.Root>
  );
}

export interface RadioGroupItemProps {
  value: string;
  disabled?: boolean;
  className?: string;
  id?: string;
}

export function RadioGroupItem({ value, disabled, className, id }: RadioGroupItemProps) {
  return (
    <RadixRadioGroup.Item
      value={value}
      disabled={disabled}
      id={id}
      className={clsx(styles.item, className)}
    >
      <RadixRadioGroup.Indicator className={styles.indicator} />
    </RadixRadioGroup.Item>
  );
}
