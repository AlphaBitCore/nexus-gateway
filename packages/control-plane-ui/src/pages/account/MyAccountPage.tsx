import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';
import { PageHeader, Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui';
import { AccountProfileTab } from './AccountProfileTab';
import { AccountApiKeysTab } from './AccountApiKeysTab';
import { AccountActivityTab } from './AccountActivityTab';
import { PersonalVKList } from '../account/personal-vks/PersonalVKList';
import styles from './Account.module.css';

const VALID_TABS = ['profile', 'apiKeys', 'virtualKeys', 'activity'];

export function MyAccountPage() {
  const { t } = useTranslation();
  const [searchParams, setSearchParams] = useSearchParams();
  const tabParam = searchParams.get('tab');
  const defaultTab = tabParam && VALID_TABS.includes(tabParam) ? tabParam : 'profile';

  return (
    <>
      <PageHeader title={t('pages:account.title')} />

      <Tabs className={styles.accountTabs} defaultValue={defaultTab} onValueChange={(v) => setSearchParams({ tab: v }, { replace: true })}>
        <TabsList>
          <TabsTrigger value="profile">{t('pages:account.tabProfile')}</TabsTrigger>
          <TabsTrigger value="apiKeys">{t('pages:account.tabApiKeys')}</TabsTrigger>
          <TabsTrigger value="virtualKeys">{t('pages:account.tabVirtualKeys', 'My Virtual Keys')}</TabsTrigger>
          <TabsTrigger value="activity">{t('pages:account.tabActivity')}</TabsTrigger>
        </TabsList>

        <TabsContent value="profile">
          <AccountProfileTab />
        </TabsContent>

        <TabsContent value="apiKeys">
          <AccountApiKeysTab />
        </TabsContent>

        <TabsContent value="virtualKeys">
          <PersonalVKList />
        </TabsContent>

        <TabsContent value="activity">
          <AccountActivityTab />
        </TabsContent>
      </Tabs>
    </>
  );
}
