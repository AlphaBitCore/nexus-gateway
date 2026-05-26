/**
 * InterceptionPathForm — reusable modal form for Create / Edit of an
 * InterceptionPath (nested under a single InterceptionDomain on the detail
 * page). The caller owns the mutation.
 */
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Dialog,
  FormField,
  Input,
  Select,
  Stack,
  Switch,
  Textarea,
} from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import type {
  InterceptionPath,
  InterceptionPathCreatePayload,
  InterceptionPathUpdatePayload,
  PathAction,
  PathMatchType,
} from '@/api/services';

const PATH_MATCH_TYPES: PathMatchType[] = ['EXACT', 'PREFIX', 'GLOB', 'REGEX'];
const PATH_ACTIONS: PathAction[] = ['PROCESS', 'PASSTHROUGH', 'BLOCK'];

export interface InterceptionPathFormValues {
  pathPatternRaw: string;
  matchType: PathMatchType;
  action: PathAction;
  priority: number;
  description: string;
  enabled: boolean;
}

export interface InterceptionPathFormProps {
  open: boolean;
  mode: 'create' | 'edit';
  initial?: InterceptionPath | null;
  onClose: () => void;
  onSubmit: (
    payload: InterceptionPathCreatePayload | InterceptionPathUpdatePayload,
  ) => Promise<unknown>;
}

const empty: InterceptionPathFormValues = {
  pathPatternRaw: '',
  matchType: 'PREFIX',
  action: 'PROCESS',
  priority: 0,
  description: '',
  enabled: true,
};

function valuesFromPath(p: InterceptionPath): InterceptionPathFormValues {
  return {
    pathPatternRaw: (p.pathPattern ?? []).join('\n'),
    matchType: p.matchType,
    action: p.action,
    priority: p.priority,
    description: p.description ?? '',
    enabled: p.enabled,
  };
}

export function InterceptionPathForm({
  open,
  mode,
  initial,
  onClose,
  onSubmit,
}: InterceptionPathFormProps) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const [values, setValues] = useState<InterceptionPathFormValues>(empty);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (!open) return;
    setValues(initial ? valuesFromPath(initial) : empty);
  }, [open, initial]);

  const update = <K extends keyof InterceptionPathFormValues>(
    key: K,
    v: InterceptionPathFormValues[K],
  ) => setValues((prev) => ({ ...prev, [key]: v }));

  const handleSubmit = async () => {
    const pathPattern = values.pathPatternRaw
      .split('\n')
      .map((s) => s.trim())
      .filter((s) => s.length > 0);
    if (pathPattern.length === 0) {
      addToast(
        t(
          'pages:interceptionDomains.validation.pathPatternRequired',
          'At least one path pattern is required',
        ),
        'error',
      );
      return;
    }
    const payload = {
      pathPattern,
      matchType: values.matchType,
      action: values.action,
      priority: Number(values.priority) || 0,
      description: values.description.trim() === '' ? null : values.description.trim(),
      enabled: values.enabled,
    };
    setSaving(true);
    try {
      await onSubmit(payload);
      onClose();
    } catch {
      // surfaced by caller
    } finally {
      setSaving(false);
    }
  };

  const title =
    mode === 'create'
      ? t('pages:interceptionDomains.addPath', 'Add path')
      : t('pages:interceptionDomains.editPathTitle', 'Edit path');

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()} title={title}>
      <Stack gap="md">
        <FormField
          label={t(
            'pages:interceptionDomains.pathPattern',
            'Path patterns (one per line)',
          )}
        >
          <Textarea
            value={values.pathPatternRaw}
            onChange={(e) => update('pathPatternRaw', e.target.value)}
            rows={3}
            placeholder="/v1/chat/completions"
            style={{ fontFamily: 'monospace', fontSize: 'var(--g-font-size-sm)' }}
          />
        </FormField>

        <Stack direction="horizontal" gap="md">
          <div style={{ flex: 1 }}>
            <FormField
              label={t('pages:interceptionDomains.pathMatchType', 'Match type')}
            >
              <Select
                value={values.matchType}
                onValueChange={(v) => update('matchType', v as PathMatchType)}
                options={PATH_MATCH_TYPES.map((v) => ({
                  value: v,
                  label: t(`pages:interceptionDomains.enums.${v}`, v),
                }))}
              />
            </FormField>
          </div>
          <div style={{ flex: 1 }}>
            <FormField label={t('pages:interceptionDomains.action', 'Action')}>
              <Select
                value={values.action}
                onValueChange={(v) => update('action', v as PathAction)}
                options={PATH_ACTIONS.map((v) => ({
                  value: v,
                  label: t(`pages:interceptionDomains.enums.${v}`, v),
                }))}
              />
            </FormField>
          </div>
          <div style={{ flex: 1 }}>
            <FormField
              label={t('pages:interceptionDomains.pathPriority', 'Priority')}
            >
              <Input
                type="number"
                value={String(values.priority)}
                onChange={(e) => update('priority', Number(e.target.value) || 0)}
              />
            </FormField>
          </div>
        </Stack>

        <FormField label={t('pages:interceptionDomains.description', 'Description')}>
          <Textarea
            value={values.description}
            onChange={(e) => update('description', e.target.value)}
            rows={2}
          />
        </FormField>

        <FormField label={t('pages:interceptionDomains.pathEnabled', 'Enabled')}>
          <Switch
            checked={values.enabled}
            onCheckedChange={(v) => update('enabled', v)}
          />
        </FormField>

        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            {t('common:cancel', 'Cancel')}
          </Button>
          <Button variant="primary" onClick={handleSubmit} disabled={saving}>
            {saving
              ? t('common:saving', 'Saving…')
              : mode === 'create'
                ? t('pages:interceptionDomains.addPath', 'Add')
                : t('common:save', 'Save')}
          </Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
