import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Textarea } from '@/components/ui/Textarea';
import { FormField } from '@/components/ui/FormField';
import styles from './JsonEditor.module.css';

export interface JsonEditorProps {
  label: string;
  value: string;
  onChange: (value: string) => void;
  error?: string;
  required?: boolean;
  rows?: number;
  placeholder?: string;
  className?: string;
}

export function JsonEditor({
  label,
  value,
  onChange,
  error,
  required,
  rows = 8,
  placeholder,
  className,
}: JsonEditorProps) {
  const { t } = useTranslation();
  const [jsonError, setJsonError] = useState<string | undefined>();

  const handleChange = (newValue: string) => {
    onChange(newValue);
    if (newValue.trim()) {
      try {
        JSON.parse(newValue);
        setJsonError(undefined);
      } catch (e) {
        setJsonError((e as Error).message);
      }
    } else {
      setJsonError(undefined);
    }
  };

  const handleFormat = () => {
    try {
      const parsed = JSON.parse(value);
      onChange(JSON.stringify(parsed, null, 2));
      setJsonError(undefined);
    } catch {
      /* ignore format attempt on invalid JSON */
    }
  };

  return (
    <div className={className}>
      <FormField label={label} error={error || jsonError} required={required}>
        <Textarea
          value={value}
          onChange={(e) => handleChange(e.target.value)}
          rows={rows}
          spellCheck={false}
          placeholder={placeholder}
          error={Boolean(error || jsonError)}
          className={styles.textarea}
        />
      </FormField>
      <button data-design-system-escape="primitive-internal"
        type="button"
        onClick={handleFormat}
        className={styles.formatButton}
      >
        {t('common:format')} JSON
      </button>
    </div>
  );
}
