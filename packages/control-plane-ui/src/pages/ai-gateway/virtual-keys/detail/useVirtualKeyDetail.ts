import { useState } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { virtualKeyApi, projectApi, systemApi } from '@/api/services';
import type { VirtualKey, VirtualKeyAllowedModelRef, TrafficEvent, AdminModelsByProvider, Project } from '@/api/types';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '@/constants/admin-api';
import { utcToDateInput } from '@/lib/format';
import { deriveUpdateExpiry } from '../expiryBounds';

export function useVirtualKeyDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  const { data: vk, loading, error, refetch } = useApi<VirtualKey>(
    () => virtualKeyApi.get(id!),
    ['admin', 'virtual-keys', 'detail', id],
  );

  const { data: modelsData } = useApi<{ data: AdminModelsByProvider[] }>(
    () => systemApi.listModels(),
    ['admin', 'models', 'by-provider'],
  );

  const { data: projectsData } = useApi<{ data: Project[] }>(
    () => projectApi.list({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'projects', 'list', 'vk-detail'],
  );

  // Regenerate key state
  const [regenConfirming, setRegenConfirming] = useState(false);
  const [newKey, setNewKey] = useState<string | null>(null);
  const [keyCopied, setKeyCopied] = useState(false);

  const { mutate: regenerateKey, loading: regenerating } = useMutation(
    () => virtualKeyApi.regenerate(id!) as Promise<{ key?: string; secretKey?: string }>,
    {
      invalidateQueries: [['api', 'admin', 'virtual-keys']],
      onSuccess: (result: { key?: string; secretKey?: string }) => {
        setRegenConfirming(false);
        setNewKey(result.key ?? '');
      },
      successMessage: 'Key regenerated',
    },
  );

  // Inline edit state
  const [isEditing, setIsEditing] = useState(false);
  const [editProjectId, setEditProjectId] = useState('');
  const [editSourceApp, setEditSourceApp] = useState('');
  const [editEnabled, setEditEnabled] = useState(true);
  const [editRateLimitRpm, setEditRateLimitRpm] = useState('');
  const [editSelectedModels, setEditSelectedModels] = useState<VirtualKeyAllowedModelRef[]>([]);
  const [editExpiresAt, setEditExpiresAt] = useState('');
  const [editNeverExpires, setEditNeverExpires] = useState(true);
  // The expiry state startEditing seeded, kept so a save can tell whether the
  // admin actually touched the field. The picker round-trips only a calendar
  // day, so a stored instant's time component cannot survive a re-derive —
  // an untouched field must be omitted from the PUT, not re-sent.
  const [initialExpiresAt, setInitialExpiresAt] = useState('');
  const [initialNeverExpires, setInitialNeverExpires] = useState(true);

  const { mutate: updateKey, loading: updating } = useMutation(
    (data: { id: string; body: unknown }) => virtualKeyApi.update(data.id, data.body as Record<string, unknown>),
    {
      invalidateQueries: [['api', 'admin', 'virtual-keys']],
      onSuccess: () => { setIsEditing(false); },
      successMessage: 'Virtual key updated',
    },
  );

  const startEditing = () => {
    if (!vk) return;
    setEditProjectId(vk.projectId ?? '');
    setEditSourceApp(vk.sourceApp ?? '');
    setEditEnabled(vk.enabled);
    setEditRateLimitRpm(vk.rateLimitRpm != null ? String(vk.rateLimitRpm) : '');
    setEditSelectedModels(Array.isArray(vk.allowedModels) ? vk.allowedModels : []);
    // Seed the picker with the calendar day the stored instant falls on in the
    // display TZ — the same day the Info tab renders. Slicing the ISO string
    // would seed the UTC day instead, so the page would show one date and the
    // picker another for every admin whose zone disagrees with UTC.
    const seededExpiresAt = vk.expiresAt ? utcToDateInput(vk.expiresAt) : '';
    // A personal VK with no expiry is currently never-expiring; seed the toggle
    // from that state so cancelling out of the form cannot silently change it.
    const seededNeverExpires = vk.vkType !== 'application' && !vk.expiresAt;
    setEditExpiresAt(seededExpiresAt);
    setEditNeverExpires(seededNeverExpires);
    setInitialExpiresAt(seededExpiresAt);
    setInitialNeverExpires(seededNeverExpires);
    setIsEditing(true);
  };

  const handleSave = () => {
    if (!vk) return;
    updateKey({
      id: vk.id,
      body: {
        projectId: editProjectId || undefined,
        sourceApp: editSourceApp || undefined,
        enabled: editEnabled,
        rateLimitRpm: editRateLimitRpm ? Number(editRateLimitRpm) : undefined,
        allowedModels: editSelectedModels,
        // Application VKs must keep a non-null expiry; personal VKs may clear
        // theirs to never-expire; an untouched expiry is omitted so saving an
        // unrelated field cannot move it. deriveUpdateExpiry owns those rules
        // and mirrors requireApplicationExpiry on the server.
        expiresAt: deriveUpdateExpiry({
          vkType: vk.vkType,
          editExpiresAt,
          editNeverExpires,
          initialExpiresAt,
          initialNeverExpires,
        }),
      },
    });
  };

  const cancelEditing = () => setIsEditing(false);

  const [activeTab, setActiveTab] = useState<string>('info');

  const { data: auditData } = useApi<{ data: TrafficEvent[] }>(
    () => activeTab === 'access-log' ? systemApi.listTrafficEvents({ virtualKeyId: id ?? '', limit: '50' }) : Promise.resolve({ data: [] as TrafficEvent[] }),
    ['admin', 'audit', 'traffic', 'vk-access-log', activeTab, id],
  );

  const project = (projectsData?.data ?? []).find(p => p.id === vk?.projectId);
  const auditLogs = auditData?.data ?? [];

  const copyNewKey = () => {
    if (!newKey) return;
    navigator.clipboard.writeText(newKey);
    setKeyCopied(true);
    setTimeout(() => setKeyCopied(false), 2000);
  };

  const dismissNewKey = () => setNewKey(null);

  return {
    // Core data
    vk,
    loading,
    error,
    refetch,
    navigate,

    // Models & projects
    modelsData,
    projectsData,
    project,

    // Regen key
    regenConfirming,
    setRegenConfirming,
    newKey,
    keyCopied,
    regenerateKey,
    regenerating,
    copyNewKey,
    dismissNewKey,

    // Edit state
    isEditing,
    editProjectId,
    setEditProjectId,
    editSourceApp,
    setEditSourceApp,
    editEnabled,
    setEditEnabled,
    editRateLimitRpm,
    setEditRateLimitRpm,
    editSelectedModels,
    setEditSelectedModels,
    editExpiresAt,
    setEditExpiresAt,
    editNeverExpires,
    setEditNeverExpires,
    updating,
    startEditing,
    handleSave,
    cancelEditing,

    // Tabs
    activeTab,
    setActiveTab,

    // Audit
    auditLogs,
  };
}
