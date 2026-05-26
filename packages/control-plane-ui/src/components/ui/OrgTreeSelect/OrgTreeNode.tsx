import { useCallback } from 'react';
import clsx from 'clsx';
import type { OrgTreeNode as TreeNodeType } from './types';
import styles from './OrgTreeSelect.module.css';

interface OrgTreeNodeProps {
  node: TreeNodeType;
  level: number;
  mode: 'single' | 'multiple';
  selectedIds: Set<string>;
  focusedId: string | null;
  expandedIds: Set<string>;
  matchedIds: Set<string>;
  searchQuery: string;
  cascade: boolean;
  onToggleExpand: (id: string) => void;
  onSelect: (id: string) => void;
  areSomeChildrenSelected: (id: string, selected: Set<string>) => boolean;
}

function highlightMatch(text: string, query: string): React.ReactNode {
  if (!query.trim()) return text;
  const lower = text.toLowerCase();
  const idx = lower.indexOf(query.toLowerCase());
  if (idx === -1) return text;
  return (
    <>
      {text.slice(0, idx)}
      <mark className={styles.highlight}>
        {text.slice(idx, idx + query.length)}
      </mark>
      {text.slice(idx + query.length)}
    </>
  );
}

export function OrgTreeNodeItem({
  node,
  level,
  mode,
  selectedIds,
  focusedId,
  expandedIds,
  matchedIds,
  searchQuery,
  cascade,
  onToggleExpand,
  onSelect,
  areSomeChildrenSelected,
}: OrgTreeNodeProps) {
  const hasChildren = node.children.length > 0;
  const isExpanded = expandedIds.has(node.id);
  const isSelected = selectedIds.has(node.id);
  const isFocused = focusedId === node.id;

  const indeterminate =
    mode === 'multiple' &&
    cascade &&
    areSomeChildrenSelected(node.id, selectedIds);

  const handleExpandClick = useCallback(
    (e: React.MouseEvent) => {
      e.stopPropagation();
      onToggleExpand(node.id);
    },
    [node.id, onToggleExpand],
  );

  const handleSelect = useCallback(() => {
    onSelect(node.id);
  }, [node.id, onSelect]);

  return (
    <>
      <div
        id={`org-tree-node-${node.id}`}
        role="treeitem"
        aria-expanded={hasChildren ? isExpanded : undefined}
        aria-selected={isSelected}
        aria-level={level + 1}
        data-node-id={node.id}
        className={clsx(
          styles.treeNode,
          isSelected && styles.treeNodeSelected,
          isFocused && styles.treeNodeFocused,
        )}
        style={{ paddingLeft: `${level * 20 + 8}px` }}
        onClick={handleSelect}
      >
        {/* Expand/collapse arrow */}
        <button data-design-system-escape="primitive-internal"
          type="button"
          className={styles.expandBtn}
          tabIndex={-1}
          onClick={hasChildren ? handleExpandClick : undefined}
          aria-hidden="true"
        >
          {hasChildren ? (isExpanded ? '\u25BC' : '\u25B6') : '\u00A0\u00A0'}
        </button>

        {/* Checkbox for multi-select */}
        {mode === 'multiple' && (
          <input
            type="checkbox"
            checked={isSelected}
            ref={(el) => {
              if (el) el.indeterminate = indeterminate;
            }}
            onChange={handleSelect}
            onClick={(e) => e.stopPropagation()}
            className={styles.checkbox}
            tabIndex={-1}
            aria-checked={indeterminate ? 'mixed' : isSelected}
          />
        )}

        {/* Label */}
        <span className={styles.nodeLabel}>
          {highlightMatch(node.name, searchQuery)}{' '}
          <span className={styles.nodeCode}>
            ({highlightMatch(node.code, searchQuery)})
          </span>
        </span>
      </div>

      {/* Render children if expanded */}
      {hasChildren && isExpanded && (
        <div role="group">
          {node.children.map((child) => (
            <OrgTreeNodeItem
              key={child.id}
              node={child}
              level={level + 1}
              mode={mode}
              selectedIds={selectedIds}
              focusedId={focusedId}
              expandedIds={expandedIds}
              matchedIds={matchedIds}
              searchQuery={searchQuery}
              cascade={cascade}
              onToggleExpand={onToggleExpand}
              onSelect={onSelect}
              areSomeChildrenSelected={areSomeChildrenSelected}
            />
          ))}
        </div>
      )}
    </>
  );
}
