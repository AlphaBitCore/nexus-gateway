import { useState, useRef, useEffect, useCallback, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import type { OrgTreeSelectProps } from './types';
import { useOrgTree, flattenVisibleIds } from './useOrgTree';
import { OrgTreeNodeItem } from './OrgTreeNode';
import styles from './OrgTreeSelect.module.css';

export function OrgTreeSelect({
  mode = 'single',
  cascade = false,
  value,
  onChange,
  placeholder,
  disabled = false,
  allowClear = true,
  inlineSearch = false,
  className,
  excludeIds,
}: OrgTreeSelectProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [focusedId, setFocusedId] = useState<string | null>(null);
  const wrapRef = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const inlineSearchRef = useRef<HTMLInputElement>(null);
  const treeRef = useRef<HTMLDivElement>(null);

  const {
    loading,
    visibleTree,
    matchedIds,
    searchQuery,
    setSearchQuery,
    expandedIds,
    toggleExpand,
    expandNode,
    collapseNode,
    getLabel,
    getDescendantIds,
    getParentId,
    areSomeChildrenSelected,
    findNode,
  } = useOrgTree({ excludeIds });

  // Normalize value to Set<string>
  const selectedIds = useMemo(() => {
    if (Array.isArray(value)) return new Set(value);
    return value ? new Set([value]) : new Set<string>();
  }, [value]);

  // Close on outside click
  useEffect(() => {
    const onDoc = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, []);

  // Focus search input when dropdown opens
  useEffect(() => {
    if (open && inlineSearch && inlineSearchRef.current) {
      inlineSearchRef.current.focus();
      return;
    }
    if (open && searchRef.current) {
      searchRef.current.focus();
    }
  }, [inlineSearch, open]);

  // Scroll focused node into view
  useEffect(() => {
    if (!focusedId || !treeRef.current) return;
    const el = treeRef.current.querySelector(
      `[data-node-id="${focusedId}"]`,
    );
    el?.scrollIntoView({ block: 'nearest' });
  }, [focusedId]);

  // Handle selection
  const handleSelect = useCallback(
    (nodeId: string) => {
      if (mode === 'single') {
        onChange(nodeId);
        setOpen(false);
        setSearchQuery('');
        return;
      }

      // Multiple mode
      const next = new Set(selectedIds);

      if (cascade) {
        const descendants = getDescendantIds(nodeId);
        if (next.has(nodeId)) {
          // Deselect self + all descendants
          next.delete(nodeId);
          for (const d of descendants) next.delete(d);
          // Deselect ancestors that are no longer fully selected
          let parentId = getParentId(nodeId);
          while (parentId) {
            next.delete(parentId);
            parentId = getParentId(parentId);
          }
        } else {
          // Select self + all descendants
          next.add(nodeId);
          for (const d of descendants) next.add(d);
          // Auto-select ancestors if all their children are now selected
          let parentId = getParentId(nodeId);
          while (parentId) {
            const node = findNode(parentId);
            if (node && node.children.every((c) => next.has(c.id))) {
              next.add(parentId);
            } else {
              break;
            }
            parentId = getParentId(parentId);
          }
        }
      } else {
        if (next.has(nodeId)) {
          next.delete(nodeId);
        } else {
          next.add(nodeId);
        }
      }

      onChange(Array.from(next));
    },
    [
      mode,
      selectedIds,
      cascade,
      onChange,
      getDescendantIds,
      getParentId,
      findNode,
      setSearchQuery,
    ],
  );

  // Keyboard navigation
  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (!open) {
        if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          setOpen(true);
          return;
        }
        return;
      }

      const visibleIds = flattenVisibleIds(visibleTree, expandedIds);
      const currentIdx = focusedId ? visibleIds.indexOf(focusedId) : -1;

      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault();
          if (currentIdx < visibleIds.length - 1) {
            setFocusedId(visibleIds[currentIdx + 1]);
          } else if (visibleIds.length > 0) {
            setFocusedId(visibleIds[0]);
          }
          break;
        case 'ArrowUp':
          e.preventDefault();
          if (currentIdx > 0) {
            setFocusedId(visibleIds[currentIdx - 1]);
          } else if (visibleIds.length > 0) {
            setFocusedId(visibleIds[visibleIds.length - 1]);
          }
          break;
        case 'ArrowRight':
          e.preventDefault();
          if (focusedId) {
            const node = findNode(focusedId);
            if (node && node.children.length > 0) {
              if (!expandedIds.has(focusedId)) {
                expandNode(focusedId);
              } else {
                // Move to first child
                setFocusedId(node.children[0].id);
              }
            }
          }
          break;
        case 'ArrowLeft':
          e.preventDefault();
          if (focusedId) {
            if (expandedIds.has(focusedId)) {
              collapseNode(focusedId);
            } else {
              // Move to parent
              const parentId = getParentId(focusedId);
              if (parentId) setFocusedId(parentId);
            }
          }
          break;
        case 'Enter':
        case ' ':
          e.preventDefault();
          if (focusedId) {
            handleSelect(focusedId);
          }
          break;
        case 'Escape':
          e.preventDefault();
          setOpen(false);
          break;
        case 'Home':
          e.preventDefault();
          if (visibleIds.length > 0) setFocusedId(visibleIds[0]);
          break;
        case 'End':
          e.preventDefault();
          if (visibleIds.length > 0)
            setFocusedId(visibleIds[visibleIds.length - 1]);
          break;
      }
    },
    [
      open,
      visibleTree,
      expandedIds,
      focusedId,
      findNode,
      expandNode,
      collapseNode,
      getParentId,
      handleSelect,
    ],
  );

  // Clear all
  const handleClear = useCallback(
    (e: React.MouseEvent) => {
      e.stopPropagation();
      if (mode === 'single') {
        onChange('');
      } else {
        onChange([]);
      }
      setSearchQuery('');
    },
    [mode, onChange, setSearchQuery],
  );

  // Remove a single tag
  const handleRemoveTag = useCallback(
    (id: string, e: React.MouseEvent) => {
      e.stopPropagation();
      const next = new Set(selectedIds);
      next.delete(id);
      onChange(Array.from(next));
    },
    [selectedIds, onChange],
  );

  // Display content for the trigger
  const hasValue =
    mode === 'single' ? Boolean(value) : (value as string[]).length > 0;
  const resolvedPlaceholder =
    placeholder ?? t('common:orgTreeSelect.placeholder');

  return (
    <div
      ref={wrapRef}
      className={clsx(styles.root, className)}
      onKeyDown={handleKeyDown}
    >
      {/* Trigger area */}
      <div
        className={clsx(
          styles.trigger,
          disabled && styles.triggerDisabled,
          open && styles.triggerOpen,
        )}
        onClick={() => !disabled && setOpen(!open)}
        role="combobox"
        aria-expanded={open}
        aria-haspopup="tree"
        aria-disabled={disabled}
        tabIndex={disabled ? -1 : 0}
      >
        <div className={styles.triggerContent}>
          {mode === 'single' ? (
            inlineSearch && open ? (
              <input
                ref={inlineSearchRef}
                type="text"
                className={styles.inlineSearchInput}
                placeholder={hasValue ? getLabel(value as string) : resolvedPlaceholder}
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
                onClick={(e) => e.stopPropagation()}
                onKeyDown={(e) => e.stopPropagation()}
                aria-label={t('common:orgTreeSelect.search')}
              />
            ) : (
              <span className={clsx(styles.singleValue, !hasValue && styles.placeholder)}>
                {hasValue
                  ? getLabel(value as string)
                  : resolvedPlaceholder}
              </span>
            )
          ) : (
            <>
              {(value as string[]).length === 0 && (
                <span className={styles.placeholder}>
                  {resolvedPlaceholder}
                </span>
              )}
              {(value as string[]).map((id) => (
                <span key={id} className={styles.tag}>
                  {getLabel(id)}
                  <button data-design-system-escape="primitive-internal"
                    type="button"
                    className={styles.tagRemove}
                    onClick={(e) => handleRemoveTag(id, e)}
                    aria-label={t('common:remove')}
                    tabIndex={-1}
                  >
                    &times;
                  </button>
                </span>
              ))}
            </>
          )}
        </div>

        {allowClear && hasValue && !disabled && (
          <button data-design-system-escape="primitive-internal"
            type="button"
            className={styles.clearBtn}
            onClick={handleClear}
            aria-label={t('common:orgTreeSelect.clear')}
            tabIndex={-1}
          >
            &times;
          </button>
        )}
      </div>

      {/* Dropdown */}
      {open && !disabled && (
        <div className={styles.dropdown}>
          {/* Search input */}
          {!inlineSearch && (
            <div className={styles.searchWrapper}>
            <input
              ref={searchRef}
              type="text"
              className={styles.searchInput}
              placeholder={t('common:orgTreeSelect.search')}
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              aria-label={t('common:orgTreeSelect.search')}
            />
            </div>
          )}

          {/* Tree */}
          <div ref={treeRef} className={styles.treeContainer} role="tree">
            {loading ? (
              <div className={styles.statusMessage}>
                {t('common:loading')}
              </div>
            ) : visibleTree.length === 0 ? (
              <div className={styles.statusMessage}>
                {t('common:orgTreeSelect.noMatch')}
              </div>
            ) : (
              visibleTree.map((node) => (
                <OrgTreeNodeItem
                  key={node.id}
                  node={node}
                  level={0}
                  mode={mode}
                  selectedIds={selectedIds}
                  focusedId={focusedId}
                  expandedIds={expandedIds}
                  matchedIds={matchedIds}
                  searchQuery={searchQuery}
                  cascade={cascade}
                  onToggleExpand={toggleExpand}
                  onSelect={handleSelect}
                  areSomeChildrenSelected={areSomeChildrenSelected}
                />
              ))
            )}
          </div>
        </div>
      )}
    </div>
  );
}
