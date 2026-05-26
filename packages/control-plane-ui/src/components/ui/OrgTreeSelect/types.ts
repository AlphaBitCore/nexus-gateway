import type { Organization } from '@/api/types';

export interface OrgTreeNode {
  id: string;
  name: string;
  code: string;
  parentId?: string;
  enabled: boolean;
  children: OrgTreeNode[];
}

export interface OrgTreeSelectProps {
  /** Selection mode. Default: 'single' */
  mode?: 'single' | 'multiple';
  /** Multi-select: selecting parent auto-selects children. Default: false */
  cascade?: boolean;
  /** Selected org ID(s). string for single, string[] for multiple. */
  value: string | string[];
  /** Fires on selection change. */
  onChange: (value: string | string[]) => void;
  /** Input placeholder text. */
  placeholder?: string;
  /** Disable the component. */
  disabled?: boolean;
  /** Show clear button. Default: true */
  allowClear?: boolean;
  /** CSS class for the root element. */
  className?: string;
  /** Org IDs to exclude from the tree (e.g. exclude self when picking parent). */
  excludeIds?: string[];
}

/** Convert API Organization[] tree to internal OrgTreeNode[] tree. */
export function toTreeNodes(orgs: Organization[]): OrgTreeNode[] {
  return orgs.map((o) => ({
    id: o.id,
    name: o.name,
    code: o.code,
    parentId: o.parentId,
    enabled: o.enabled,
    children: o.children ? toTreeNodes(o.children) : [],
  }));
}
