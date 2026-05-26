import * as RadixCheckbox from '@radix-ui/react-checkbox';
import clsx from 'clsx';
import styles from './Checkbox.module.css';

export interface CheckboxProps {
  checked: boolean | 'indeterminate';
  onCheckedChange: (checked: boolean | 'indeterminate') => void;
  disabled?: boolean;
  className?: string;
  id?: string;
}

export function Checkbox({ checked, onCheckedChange, disabled, className, id }: CheckboxProps) {
  return (
    <RadixCheckbox.Root
      checked={checked}
      onCheckedChange={onCheckedChange}
      disabled={disabled}
      id={id}
      className={clsx(styles.root, className)}
    >
      <RadixCheckbox.Indicator className={styles.indicator}>
        {checked === 'indeterminate' ? '\u2012' : '\u2713'}
      </RadixCheckbox.Indicator>
    </RadixCheckbox.Root>
  );
}
