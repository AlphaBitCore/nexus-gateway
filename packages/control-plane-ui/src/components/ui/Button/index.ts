// Button has moved to @nexus-gateway/ui-shared. This barrel re-exports
// the shared implementation so existing imports
// (e.g. `import { Button } from '@/components/ui'`) keep working
// without a sweeping rename. Delete this file once every consumer
// imports directly from `@nexus-gateway/ui-shared`.
export { Button } from '@nexus-gateway/ui-shared';
export type { ButtonProps, ButtonVariant, ButtonSize } from '@nexus-gateway/ui-shared';
