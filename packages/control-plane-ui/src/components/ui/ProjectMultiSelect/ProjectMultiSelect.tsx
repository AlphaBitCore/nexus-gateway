import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { MultiSelectDropdown } from '@/components/ui/MultiSelectDropdown';
import { useApi } from '@/hooks/useApi';
import { projectApi, organizationApi } from '@/api/services';
import type { Project, Organization } from '@/api/types';

export interface ProjectMultiSelectProps {
  label: string;
  value: string[];
  onChange: (next: string[]) => void;
  emptyLabel?: string;
  disabled?: boolean;
  className?: string;
}

/**
 * Multi-select of project ids for routing rule match conditions and similar
 * admin filters. Options render as "{organization} / {project}" for
 * unambiguous identification; the emitted value is the project id.
 */
export function ProjectMultiSelect({
  label,
  value,
  onChange,
  emptyLabel,
  disabled,
  className,
}: ProjectMultiSelectProps) {
  const { t } = useTranslation();

  const { data: projectsData, loading: projectsLoading } = useApi<{ data: Project[]; total: number }>(
    () => projectApi.list({ limit: '500' }),
    ['admin', 'projects', 'list', 'match-conditions'],
  );

  const { data: orgsData, loading: orgsLoading } = useApi<{ data: Organization[] }>(
    () => organizationApi.list({ limit: '500' }),
    ['admin', 'organizations', 'list', 'match-conditions'],
  );

  const options = useMemo(() => {
    const projects = projectsData?.data ?? [];
    const orgs = orgsData?.data ?? [];
    const orgById = new Map<string, Organization>();
    for (const o of orgs) orgById.set(o.id, o);
    return projects.map((p) => {
      const orgName = p.organization?.name ?? orgById.get(p.organizationId)?.name ?? '';
      const composite = orgName ? `${orgName} / ${p.name}` : p.name;
      return { value: p.id, label: composite };
    });
  }, [projectsData, orgsData]);

  const loading = projectsLoading || orgsLoading;

  return (
    <MultiSelectDropdown
      label={label}
      options={options}
      value={value}
      onChange={onChange}
      disabled={disabled || loading}
      emptyLabel={loading ? t('common:loading') : (emptyLabel ?? t('common:selectProjects'))}
      searchable
      searchPlaceholder={t('common:searchProjects')}
      className={className}
    />
  );
}
