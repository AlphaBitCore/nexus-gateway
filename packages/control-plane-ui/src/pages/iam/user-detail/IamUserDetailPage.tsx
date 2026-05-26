import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import {
  PageHeader, Skeleton, ErrorBanner, AlertDialog, Breadcrumb, Button, Stack,
  Tabs, TabsList, TabsTrigger, TabsContent, Dialog, FormField, Input,
} from '@/components/ui';
import { useIamUserDetail } from './useIamUserDetail';
import { UserInfoTab } from './UserInfoTab';
import { UserPermissionsTab } from './UserPermissionsTab';
import { UserDevicesTab } from './UserDevicesTab';

export function IamUserDetailPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const state = useIamUserDetail();

  const {
    user,
    loading,
    error,
    refetch,
    deletingUser,
    setDeletingUser,
    deleteUser,
    isEditing,
    startEditing,
    isResettingPassword,
    setIsResettingPassword,
    resetPassword,
    setResetPassword,
    resetPasswordConfirm,
    setResetPasswordConfirm,
    resetPasswordLoading,
    handleResetPassword,
  } = state;

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!user) return <ErrorBanner message={t('pages:iam.userNotFound')} />;

  const passwordMismatch = resetPassword !== resetPasswordConfirm && resetPasswordConfirm !== '';

  return (
    <Stack gap="lg">
      <Stack direction="horizontal" gap="sm" align="center">
        <Button variant="secondary" size="sm" onClick={() => navigate(-1)}>
          ← {t('common:back')}
        </Button>
        <Breadcrumb items={[
          { label: t('pages:iam.users'), to: '/iam/users' },
          { label: user.displayName },
        ]} />
      </Stack>

      <PageHeader
        title={user.displayName}
        subtitle={user.email || undefined}
        action={
          <Stack direction="horizontal" gap="sm" align="center">
            {!isEditing && (
              <Button variant="secondary" onClick={startEditing}>
                {t('common:edit')}
              </Button>
            )}
            <Button variant="secondary" onClick={() => setIsResettingPassword(true)}>
              {t('pages:iam.resetPassword')}
            </Button>
            <Button variant="danger" onClick={() => setDeletingUser(true)}>
              {t('common:delete')}
            </Button>
          </Stack>
        }
      />

      <Tabs defaultValue="info">
        <TabsList>
          <TabsTrigger value="info">{t('pages:iam.info')}</TabsTrigger>
          <TabsTrigger value="permissions">{t('pages:iam.permissions')}</TabsTrigger>
          <TabsTrigger value="devices">{t('pages:userDetail.tabs.devices')}</TabsTrigger>
        </TabsList>

        <TabsContent value="info">
          <UserInfoTab
            user={state.user}
            isEditing={state.isEditing}
            setIsEditing={state.setIsEditing}
            editDisplayName={state.editDisplayName}
            setEditDisplayName={state.setEditDisplayName}
            editEmail={state.editEmail}
            setEditEmail={state.setEditEmail}
            editEnabled={state.editEnabled}
            setEditEnabled={state.setEditEnabled}
            editOrgId={state.editOrgId}
            setEditOrgId={state.setEditOrgId}
            editCanAccessCP={state.editCanAccessCP}
            setEditCanAccessCP={state.setEditCanAccessCP}
            saveLoading={state.saveLoading}
            handleSave={state.handleSave}
          />
        </TabsContent>

        <TabsContent value="permissions">
          <UserPermissionsTab
            allPolicies={user.policyAttachments ?? []}
            showAddRole={state.showAddRole}
            setShowAddRole={state.setShowAddRole}
            selectedGroupId={state.selectedGroupId}
            setSelectedGroupId={state.setSelectedGroupId}
            removingRole={state.removingRole}
            setRemovingRole={state.setRemovingRole}
            currentRoles={state.currentRoles}
            availableGroups={state.availableGroups}
            addToGroup={state.addToGroup}
            addGroupLoading={state.addGroupLoading}
            removeFromGroup={state.removeFromGroup}
            removeGroupLoading={state.removeGroupLoading}
            directPolicies={state.directPolicies}
            showAttachPolicy={state.showAttachPolicy}
            setShowAttachPolicy={state.setShowAttachPolicy}
            selectedPolicyId={state.selectedPolicyId}
            setSelectedPolicyId={state.setSelectedPolicyId}
            detachingPolicy={state.detachingPolicy}
            setDetachingPolicy={state.setDetachingPolicy}
            availablePolicies={state.availablePolicies}
            attachPolicy={state.attachPolicy}
            attachPolicyLoading={state.attachPolicyLoading}
            detachPolicy={state.detachPolicy}
            detachPolicyLoading={state.detachPolicyLoading}
          />
        </TabsContent>

        <TabsContent value="devices">
          {user.id && <UserDevicesTab userId={user.id} />}
        </TabsContent>


      </Tabs>

      <Dialog
        open={isResettingPassword}
        onOpenChange={(open) => {
          if (!open) {
            setIsResettingPassword(false);
            setResetPassword('');
            setResetPasswordConfirm('');
          }
        }}
        title={t('pages:iam.resetPassword')}
        size="sm"
      >
        <Stack gap="md">
          <FormField label={t('pages:iam.newPassword')}>
            <Input
              name="resetPassword"
              type="password"
              value={resetPassword}
              onChange={(e) => setResetPassword(e.target.value)}
              placeholder={t('pages:iam.newPasswordPlaceholder')}
            />
          </FormField>
          <FormField
            label={t('pages:iam.confirmPassword')}
            error={passwordMismatch ? t('pages:iam.passwordMismatch') : undefined}
          >
            <Input
              name="resetPasswordConfirm"
              type="password"
              value={resetPasswordConfirm}
              onChange={(e) => setResetPasswordConfirm(e.target.value)}
              placeholder={t('pages:iam.confirmPasswordPlaceholder')}
            />
          </FormField>
          <Stack direction="horizontal" gap="sm" justify="end">
            <Button
              variant="secondary"
              onClick={() => {
                setIsResettingPassword(false);
                setResetPassword('');
                setResetPasswordConfirm('');
              }}
            >
              {t('common:cancel')}
            </Button>
            <Button
              onClick={handleResetPassword}
              disabled={resetPasswordLoading || !resetPassword || passwordMismatch}
            >
              {resetPasswordLoading ? t('pages:iam.saving') : t('pages:iam.resetPassword')}
            </Button>
          </Stack>
        </Stack>
      </Dialog>

      <AlertDialog
        open={deletingUser}
        onOpenChange={(open) => { if (!open) setDeletingUser(false); }}
        title={t('pages:iam.deleteUser')}
        description={t('pages:iam.deleteUserConfirm', { name: user.displayName })}
        confirmLabel={t('common:delete')}
        onConfirm={() => deleteUser(undefined as never)}
        variant="danger"
      />
    </Stack>
  );
}
