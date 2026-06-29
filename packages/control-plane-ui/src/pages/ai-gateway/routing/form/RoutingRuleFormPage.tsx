import { useTranslation } from 'react-i18next';
import {
  Dialog, Button, Input, Switch, Tooltip, Stack,
} from '@/components/ui';
import type { RoutingRule } from '@/api/types';
import { RoutingPrimaryWinnerCallout } from '../_shared/RoutingPrimaryWinnerCallout';
import {
  ROUTING_RULE_FIELD_HELP,
  RoutingStrategyTypesHelp,
} from '../_shared/routing-rule-field-help';
import type { StrategyType } from '../_shared/routing-rule-config';
import styles from './RoutingRuleForm.module.css';
import { useRoutingRuleForm } from './useRoutingRuleForm';
import { StrategyConfigSection } from './StrategyConfigSection';
import { FallbackChainSection } from './FallbackChainSection';
import { MatchConditionsSection } from './MatchConditionsSection';
import { RetryPolicySection } from './RetryPolicySection';
import { HelpIconButton } from '@nexus-gateway/ui-shared';
import { useStrategyOptions } from '../_shared/useStrategyOptions';

interface RoutingRuleFormProps {
  rule?: RoutingRule;
  onClose: () => void;
  onSaved: () => void;
}

export function RoutingRuleForm({ rule, onClose, onSaved }: RoutingRuleFormProps) {
  const { t } = useTranslation();
  const strategyOptions = useStrategyOptions();

  const form = useRoutingRuleForm({ rule, onClose, onSaved });

  return (
    <Dialog
      open
      onOpenChange={(open) => { if (!open) onClose(); }}
      title={rule ? t('pages:routing.editRule', 'Edit Routing Rule') : t('pages:routing.createRule')}
      size="lg"
    >
      <Stack gap="md">
        {form.pipelineStage === '1' && <RoutingPrimaryWinnerCallout />}

        <div className={styles.fieldGroup}>
          <label className={styles.fieldLabel}>
            {t('pages:routing.name')} <span className={styles.required}>*</span>
          </label>
          <Input
            className={styles.textInput}
            value={form.name}
            onChange={(e) => form.setName(e.target.value)}
            required
          />
        </div>

        <div className={styles.fieldGroup}>
          <label className={styles.fieldLabel}>{t('pages:routing.description')}</label>
          <Input
            className={styles.textInput}
            value={form.description}
            onChange={(e) => form.setDescription(e.target.value)}
          />
        </div>

        {form.pipelineStage === '1' && (
          <div>
            <div className={styles.labelRow}>
              <label htmlFor="strategyType" className={styles.fieldLabel}>
                {t('pages:routing.strategyType')} <span className={styles.required}>*</span>
              </label>
              <RoutingStrategyTypesHelp />
            </div>
            <select
              id="strategyType"
              value={form.strategyType}
              onChange={(e) => form.handleStrategyChange(e.target.value as StrategyType)}
              className={styles.selectInputFull}
            >
              {strategyOptions.map(opt => (
                <option key={opt.value} value={opt.value}>{opt.label}</option>
              ))}
            </select>
            {form.strategyType === 'fallback' && (
              <div className={styles.warningBanner} role="status">
                {ROUTING_RULE_FIELD_HELP.strategyFallbackRecoveryOnly}
              </div>
            )}
          </div>
        )}

        <div className={styles.fieldGroup}>
          <div className={styles.labelRow}>
            <label className={styles.fieldLabel}>{t('pages:routing.priority')}</label>
            <Tooltip content={ROUTING_RULE_FIELD_HELP.priority}>
              <HelpIconButton aria-label={t('pages:routing.ariaHelpPriority')} />
            </Tooltip>
          </div>
          <Input
            className={styles.textInput}
            type="number"
            value={form.priority}
            onChange={(e) => form.setPriority(e.target.value)}
          />
        </div>

        <div className={styles.switchRow}>
          <label className={styles.fieldLabel}>{t('pages:routing.enabled')}</label>
          <Tooltip content={ROUTING_RULE_FIELD_HELP.enabled}>
            <HelpIconButton aria-label={t('pages:routing.ariaHelpEnabled')} />
          </Tooltip>
          <Switch checked={form.enabled} onCheckedChange={form.setEnabled} />
        </div>

        <StrategyConfigSection
          pipelineStage={form.pipelineStage}
          strategyType={form.strategyType}
          providerGroups={form.providerGroups}
          policyAllowM={form.policyAllowM}
          setPolicyAllowM={form.setPolicyAllowM}
          policyDenyM={form.policyDenyM}
          setPolicyDenyM={form.setPolicyDenyM}
          policyAllowP={form.policyAllowP}
          setPolicyAllowP={form.setPolicyAllowP}
          policyDenyP={form.policyDenyP}
          setPolicyDenyP={form.setPolicyDenyP}
          singleProvider={form.singleProvider}
          setSingleProvider={form.setSingleProvider}
          singleModel={form.singleModel}
          setSingleModel={form.setSingleModel}
          entries={form.entries}
          updateEntry={form.updateEntry}
          addEntry={form.addEntry}
          removeEntry={form.removeEntry}
          conditionalUi={form.conditionalUi}
          setConditionalUi={form.setConditionalUi}
          smartState={form.smartState}
          updateSmart={form.updateSmart}
          showWeightColumn={form.showWeightColumn}
        />

        {form.pipelineStage !== '0' && (
          <FallbackChainSection
            fallbackEntries={form.fallbackEntries}
            addFallback={form.addFallback}
            removeFallback={form.removeFallback}
            updateFallback={form.updateFallback}
            providerGroups={form.providerGroups}
          />
        )}

        {form.pipelineStage !== '0' && (
          <RetryPolicySection
            mode={form.retryPolicyMode}
            onModeChange={form.setRetryPolicyMode}
            maxAttempts={form.retryMaxAttempts}
            onMaxAttemptsChange={form.setRetryMaxAttempts}
            retryOn={form.retryOn}
            onRetryOnChange={form.setRetryOn}
          />
        )}

        <MatchConditionsSection
          models={form.models}
          setModels={form.setModels}
          matchProviders={form.matchProviders}
          setMatchProviders={form.setMatchProviders}
          matchProjectIds={form.matchProjectIds}
          setMatchProjectIds={form.setMatchProjectIds}
          matchRequestedModelLiterals={form.matchRequestedModelLiterals}
          setMatchRequestedModelLiterals={form.setMatchRequestedModelLiterals}
          matchModelTypes={form.matchModelTypes}
          setMatchModelTypes={form.setMatchModelTypes}
          matchVirtualKeys={form.matchVirtualKeys}
          setMatchVirtualKeys={form.setMatchVirtualKeys}
          providerGroups={form.providerGroups}
          configModelIds={form.configModelIds}
        />

        <Stack direction="horizontal" gap="sm" className={styles.actionsEnd}>
          <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
          <Button
            variant="primary"
            onClick={form.handleSubmit}
            disabled={form.loading || !form.name || form.retryPolicyInvalid}
          >
            {form.loading ? t('pages:routing.saving', 'Saving...') : t('common:save')}
          </Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
