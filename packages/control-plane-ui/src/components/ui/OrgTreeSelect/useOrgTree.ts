import { useState, useMemo, useCallback } from 'react';
import { useApi } from '@/hooks/useApi';
import { organizationApi } from '@/api/services';
import type { Organization } from '@/api/types';
import type { OrgTreeNode } from './types';
import { toTreeNodes } from './types';

interface UseOrgTreeOptions {
  excludeIds?: string[];
}

/** Collect all node IDs in a subtree (inclusive). */
function collectDescendantIds(node: OrgTreeNode): string[] {
  const ids = [node.id];
  for (const child of node.children) {
    ids.push(...collectDescendantIds(child));
  }
  return ids;
}

/** Find a node by ID in the tree. */
function findNode(nodes: OrgTreeNode[], id: string): OrgTreeNode | null {
  for (const n of nodes) {
    if (n.id === id) return n;
    const found = findNode(n.children, id);
    if (found) return found;
  }
  return null;
}

/** Build a map of nodeId -> parentId for the entire tree. */
function buildParentMap(
  nodes: OrgTreeNode[],
  parentId?: string,
): Map<string, string | undefined> {
  const map = new Map<string, string | undefined>();
  for (const n of nodes) {
    map.set(n.id, parentId);
    const childMap = buildParentMap(n.children, n.id);
    for (const [k, v] of childMap) map.set(k, v);
  }
  return map;
}

/** Get ancestor IDs of a node (not including itself). */
function getAncestorIds(
  parentMap: Map<string, string | undefined>,
  nodeId: string,
): string[] {
  const ancestors: string[] = [];
  let current = parentMap.get(nodeId);
  while (current) {
    ancestors.push(current);
    current = parentMap.get(current);
  }
  return ancestors;
}

/**
 * Filter tree: keep nodes matching query or having matching descendants.
 * Returns the filtered tree, matched IDs, and ancestor IDs that should be auto-expanded.
 */
function filterTree(
  nodes: OrgTreeNode[],
  query: string,
  parentMap: Map<string, string | undefined>,
): { filtered: OrgTreeNode[]; matchedIds: Set<string>; searchExpandedIds: Set<string> } {
  const lowerQ = query.toLowerCase();
  const matchedIds = new Set<string>();

  function filter(items: OrgTreeNode[]): OrgTreeNode[] {
    const result: OrgTreeNode[] = [];
    for (const node of items) {
      const selfMatch =
        node.name.toLowerCase().includes(lowerQ) ||
        node.code.toLowerCase().includes(lowerQ);
      const filteredChildren = filter(node.children);

      if (selfMatch || filteredChildren.length > 0) {
        if (selfMatch) matchedIds.add(node.id);
        result.push({ ...node, children: filteredChildren });
      }
    }
    return result;
  }

  const filtered = filter(nodes);

  // Compute ancestor IDs to auto-expand (derived, no side effect)
  const searchExpandedIds = new Set<string>();
  for (const id of matchedIds) {
    for (const ancestorId of getAncestorIds(parentMap, id)) {
      searchExpandedIds.add(ancestorId);
    }
  }

  return { filtered, matchedIds, searchExpandedIds };
}

/** Exclude nodes by ID (and their subtrees). */
function excludeNodes(nodes: OrgTreeNode[], ids: Set<string>): OrgTreeNode[] {
  return nodes
    .filter((n) => !ids.has(n.id))
    .map((n) => ({ ...n, children: excludeNodes(n.children, ids) }));
}

/** Flatten a tree to a list of all visible node IDs (expanded only). */
export function flattenVisibleIds(
  nodes: OrgTreeNode[],
  expandedIds: Set<string>,
): string[] {
  const result: string[] = [];
  for (const node of nodes) {
    result.push(node.id);
    if (node.children.length > 0 && expandedIds.has(node.id)) {
      result.push(...flattenVisibleIds(node.children, expandedIds));
    }
  }
  return result;
}

export function useOrgTree(options: UseOrgTreeOptions = {}) {
  const { excludeIds } = options;

  // Load full tree once
  const { data: rawData, loading } = useApi<{ data: Organization[] }>(
    () => organizationApi.tree(),
    ['admin', 'organizations', 'tree'],
  );

  // Convert to internal tree, excluding specified IDs
  const fullTree = useMemo(() => {
    const nodes = toTreeNodes(rawData?.data ?? []);
    if (excludeIds && excludeIds.length > 0) {
      return excludeNodes(nodes, new Set(excludeIds));
    }
    return nodes;
  }, [rawData, excludeIds]);

  const parentMap = useMemo(() => buildParentMap(fullTree), [fullTree]);

  // Search state
  const [searchQuery, setSearchQuery] = useState('');

  // Expand/collapse state (manual)
  const [expandedIds, setExpandedIds] = useState<Set<string>>(new Set());

  // Filtered tree + search-forced expand IDs (all derived, no side effects)
  const { visibleTree, matchedIds, searchExpandedIds } = useMemo(() => {
    if (!searchQuery.trim()) {
      return {
        visibleTree: fullTree,
        matchedIds: new Set<string>(),
        searchExpandedIds: new Set<string>(),
      };
    }
    const result = filterTree(fullTree, searchQuery.trim(), parentMap);
    return {
      visibleTree: result.filtered,
      matchedIds: result.matchedIds,
      searchExpandedIds: result.searchExpandedIds,
    };
  }, [fullTree, searchQuery, parentMap]);

  // Effective expanded IDs = manual + search-forced
  const effectiveExpandedIds = useMemo(() => {
    const merged = new Set(expandedIds);
    for (const id of searchExpandedIds) merged.add(id);
    return merged;
  }, [expandedIds, searchExpandedIds]);

  const toggleExpand = useCallback((nodeId: string) => {
    setExpandedIds((prev) => {
      const next = new Set(prev);
      if (next.has(nodeId)) {
        next.delete(nodeId);
      } else {
        next.add(nodeId);
      }
      return next;
    });
  }, []);

  const expandNode = useCallback((nodeId: string) => {
    setExpandedIds((prev) => {
      if (prev.has(nodeId)) return prev;
      const next = new Set(prev);
      next.add(nodeId);
      return next;
    });
  }, []);

  const collapseNode = useCallback((nodeId: string) => {
    setExpandedIds((prev) => {
      if (!prev.has(nodeId)) return prev;
      const next = new Set(prev);
      next.delete(nodeId);
      return next;
    });
  }, []);

  // Build label lookup (id -> "Name (code)")
  const labelMap = useMemo(() => {
    const map = new Map<string, string>();
    function walk(nodes: OrgTreeNode[]) {
      for (const n of nodes) {
        map.set(n.id, `${n.name} (${n.code})`);
        walk(n.children);
      }
    }
    walk(fullTree);
    return map;
  }, [fullTree]);

  const getLabel = useCallback(
    (id: string) => labelMap.get(id) ?? id,
    [labelMap],
  );

  // Cascade helpers
  const getDescendantIds = useCallback(
    (nodeId: string) => {
      const node = findNode(fullTree, nodeId);
      return node
        ? collectDescendantIds(node).filter((id) => id !== nodeId)
        : [];
    },
    [fullTree],
  );

  const getParentId = useCallback(
    (nodeId: string) => parentMap.get(nodeId) ?? null,
    [parentMap],
  );

  /** Check if some (but not all) children of a node are selected. */
  const areSomeChildrenSelected = useCallback(
    (nodeId: string, selectedIds: Set<string>) => {
      const node = findNode(fullTree, nodeId);
      if (!node || node.children.length === 0) return false;
      const childSelected = node.children.some((c) => selectedIds.has(c.id));
      const allSelected = node.children.every((c) => selectedIds.has(c.id));
      return childSelected && !allSelected;
    },
    [fullTree],
  );

  return {
    loading,
    fullTree,
    visibleTree,
    matchedIds,
    searchQuery,
    setSearchQuery,
    expandedIds: effectiveExpandedIds,
    toggleExpand,
    expandNode,
    collapseNode,
    getLabel,
    getDescendantIds,
    getParentId,
    areSomeChildrenSelected,
    findNode: (id: string) => findNode(fullTree, id),
  };
}
