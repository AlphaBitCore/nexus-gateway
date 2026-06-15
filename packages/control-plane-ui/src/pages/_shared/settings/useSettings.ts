import { useState, useMemo, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../../../auth/context/AuthContext';
import { useApi } from '../../../hooks/useApi';
import { systemApi, iamApi } from '@/api/services';
import { useMutation } from '../../../hooks/useMutation';
import { useToast } from '../../../context/ToastContext';
import { useZodForm } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import { z } from 'zod';
import type { AdminApiKey, WhoAmI } from '../../../api/types';

interface ApiKeysOwnedResponse {
  data: AdminApiKey[];
}

export function toDatetimeLocalValue(iso?: string | null): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export { formatDateTime as fmtDate } from '@/lib/format';

export function principalTypeLabelText(pt?: string): string {
  if (pt === 'admin_user') return 'Password account (dashboard user)';
  if (pt === 'api_key') return 'API key';
  return pt ? String(pt) : '—';
}

export const MIN_PASSWORD_LEN = 8;

/* ── Zod schemas ─────────────────────────────────────────────────────── */

export const profileSchema = z.object({
  profileUsername: z.string().min(1),
  profileEmail: z.string().optional().default(''),
});
export type ProfileValues = z.infer<typeof profileSchema>;

export const passwordSchema = z.object({
  currentPassword: z.string().min(1),
  newPassword: z.string().min(MIN_PASSWORD_LEN),
  confirmPassword: z.string().min(MIN_PASSWORD_LEN),
});
export type PasswordValues = z.infer<typeof passwordSchema>;

export const apiKeyEditSchema = z.object({
  editName: z.string().min(1),
  editExpiresLocal: z.string().optional().default(''),
  editEnabled: z.boolean(),
});
export type ApiKeyEditValues = z.infer<typeof apiKeyEditSchema>;

export function useSettings() {
  const { keyName, roles, refreshSession } = useAuth();
  const { addToast } = useToast();
  const { t } = useTranslation();

  // ── Tab state ──────────────────────────────────────────────────────
  const [mainTab, setMainTab] = useState('general');

  // ── API key CRUD state ─────────────────────────────────────────────
  const [showCreate, setShowCreate] = useState(false);
  const [editing, setEditing] = useState<AdminApiKey | null>(null);
  const [deleting, setDeleting] = useState<AdminApiKey | null>(null);

  // ── Form instances ────────────────────────────────────────────────
  const profileForm = useZodForm<ProfileValues>({
    schema: profileSchema,
    defaultValues: { profileUsername: '', profileEmail: '' },
  });

  const passwordForm = useZodForm<PasswordValues>({
    schema: passwordSchema,
    defaultValues: { currentPassword: '', newPassword: '', confirmPassword: '' },
  });

  const apiKeyEditForm = useZodForm<ApiKeyEditValues>({
    schema: apiKeyEditSchema,
    defaultValues: { editName: '', editExpiresLocal: '', editEnabled: true },
  });

  useUnsavedChangesWarning(
    profileForm.formState.isDirty ||
    passwordForm.formState.isDirty ||
    apiKeyEditForm.formState.isDirty,
  );

  // ── Profile/password UI state ──────────────────────────────────────
  const [profileEditing, setProfileEditing] = useState(false);
  const [passwordEditing, setPasswordEditing] = useState(false);

  // ── Who am I ───────────────────────────────────────────────────────
  const { data: whoami, loading: whoamiLoading, error: whoamiError, refetch: refetchWhoami } = useApi<WhoAmI>(
    () => systemApi.me(),
    ['admin', 'me'],
  );

  const canEditProfile = whoami?.authPrincipalType === 'admin_user';

  useEffect(() => {
    if (!whoami || profileEditing) return;
    profileForm.reset({
      profileUsername: whoami.keyName,
      profileEmail: whoami.email ?? '',
    });
  }, [whoami, profileEditing]);

  // ── Profile editing helpers ────────────────────────────────────────
  const startEditProfile = () => {
    setPasswordEditing(false);
    passwordForm.reset();
    setProfileEditing(true);
  };

  const startChangePassword = () => {
    setProfileEditing(false);
    profileForm.reset({
      profileUsername: whoami?.keyName ?? '',
      profileEmail: whoami?.email ?? '',
    });
    setPasswordEditing(true);
  };

  const cancelProfileEdit = () => {
    setProfileEditing(false);
    profileForm.reset({
      profileUsername: whoami?.keyName ?? '',
      profileEmail: whoami?.email ?? '',
    });
  };

  const cancelPasswordEdit = () => {
    setPasswordEditing(false);
    passwordForm.reset();
  };

  // ── API keys data ──────────────────────────────────────────────────
  const { data: keysRes, loading: keysLoading, error: keysError, refetch: refetchKeys } = useApi<ApiKeysOwnedResponse>(
    () => iamApi.listApiKeys({ scope: 'owned' }),
    ['admin', 'api-keys', 'owned'],
    { skip: !whoami },
  );

  const keys = useMemo(() => keysRes?.data ?? [], [keysRes]);

  // ── Mutations ──────────────────────────────────────────────────────
  const { mutate: patchKey, loading: patchLoading } = useMutation(
    (payload: { id: string; body: Record<string, unknown> }) =>
      iamApi.patchApiKey(payload.id, payload.body) as Promise<{ data: AdminApiKey }>,
    {
      onSuccess: () => {
        setEditing(null);
        void refetchKeys();
      },
      successMessage: 'API key updated',
    },
  );

  const { mutate: deleteKey, loading: deleteLoading } = useMutation(
    (id: string) => iamApi.deleteApiKey(id),
    {
      onSuccess: () => {
        setDeleting(null);
        void refetchKeys();
      },
      successMessage: 'API key revoked',
    },
  );

  const afterMePatch = useCallback(() => {
    void refetchWhoami();
    void refreshSession();
  }, [refetchWhoami, refreshSession]);

  const { mutate: patchProfile, loading: profileSaving } = useMutation(
    (body: Record<string, unknown>) => iamApi.patchMe(body) as Promise<WhoAmI>,
    {
      successMessage: 'Profile updated',
      onSuccess: () => {
        setProfileEditing(false);
        afterMePatch();
      },
    },
  );

  const { mutate: patchPassword, loading: passwordSaving } = useMutation(
    (body: Record<string, unknown>) => iamApi.patchMe(body) as Promise<WhoAmI>,
    {
      successMessage: 'Password updated',
      onSuccess: () => {
        setPasswordEditing(false);
        passwordForm.reset();
        afterMePatch();
      },
    },
  );

  const meSaving = profileSaving || passwordSaving;

  const saveProfile = () => {
    if (!canEditProfile) return;
    const v = profileForm.getValues();
    const u = v.profileUsername.trim();
    if (!u) {
      addToast(t('common:validation.usernameRequired'), 'error');
      return;
    }
    const em = v.profileEmail.trim();
    void patchProfile({ username: u, email: em === '' ? null : em });
  };

  const changePassword = async () => {
    if (!canEditProfile) return;
    const v = passwordForm.getValues();
    if (v.newPassword !== v.confirmPassword) {
      addToast(t('common:validation.passwordsDoNotMatch'), 'error');
      return;
    }
    if (v.newPassword.length < MIN_PASSWORD_LEN) {
      addToast(t('common:validation.passwordTooShort', { count: MIN_PASSWORD_LEN }), 'error');
      return;
    }
    if (!v.currentPassword) {
      addToast(t('common:validation.currentPasswordRequired'), 'error');
      return;
    }
    try {
      await patchPassword({ currentPassword: v.currentPassword, newPassword: v.newPassword });
    } catch {
      /* error toast from useMutation */
    }
  };

  // ── API key edit helpers ───────────────────────────────────────────
  const openEdit = (k: AdminApiKey) => {
    setEditing(k);
    apiKeyEditForm.reset({
      editName: k.name,
      editExpiresLocal: toDatetimeLocalValue(k.expiresAt),
      editEnabled: k.enabled,
    });
  };

  const saveEdit = () => {
    if (!editing) return;
    const v = apiKeyEditForm.getValues();
    const body: Record<string, unknown> = {
      name: v.editName.trim(),
      enabled: v.editEnabled,
    };
    if (v.editExpiresLocal) {
      body.expiresAt = new Date(v.editExpiresLocal).toISOString();
    } else {
      body.expiresAt = null;
    }
    patchKey({ id: editing.id, body });
  };

  return {
    // Auth context values
    keyName,
    roles,

    // Tab
    mainTab,
    setMainTab,

    // Who am I
    whoami,
    whoamiLoading,
    whoamiError,
    canEditProfile,

    // Profile form
    profileEditing,
    profileForm,
    profileSaving,
    meSaving,
    startEditProfile,
    cancelProfileEdit,
    saveProfile,

    // Password form
    passwordEditing,
    passwordForm,
    passwordSaving,
    startChangePassword,
    cancelPasswordEdit,
    changePassword,

    // API keys
    keys,
    keysLoading,
    keysError,
    keysRes,
    showCreate,
    setShowCreate,
    refetchKeys,

    // API key editing
    editing,
    setEditing,
    apiKeyEditForm,
    patchLoading,
    openEdit,
    saveEdit,

    // API key deletion
    deleting,
    setDeleting,
    deleteKey,
    deleteLoading,
  };
}

export type UseSettingsReturn = ReturnType<typeof useSettings>;
