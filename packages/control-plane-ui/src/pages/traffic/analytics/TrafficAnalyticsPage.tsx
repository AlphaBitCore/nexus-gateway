import { useCallback } from 'react';
import { useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  PageHeader,
  Stack,
  Tabs,
  TabsList,
  TabsTrigger,
  TabsContent,
} from '@/components/ui';
import { TrafficTab, type TrafficSourceFilter } from '../list/TrafficTab';
import css from './TrafficAnalyticsPage.module.css';

/* -- Source sub-tabs --
 *
 * Tab state lives in the `?source=` URL param so deep-links (e.g. from the
 * node detail "View all traffic" link) land on the right tab instead of
 * always defaulting to "All". Param values: `''` (omitted) = All, `'vk'`,
 * `'proxy'`, `'agent'`. Stored as a single param so the browser back/forward
 * buttons walk tab history naturally.
 */

/**
 * Built inside the component so the labels resolve through i18n. The values
 * stay literal because they're route-param tokens, not user-visible strings.
 */
function getSourceTabs(t: (key: string) => string): { value: TrafficSourceFilter; label: string }[] {
  return [
    { value: '', label: t('pages:traffic.sourceTabAll') },
    { value: 'vk', label: t('pages:traffic.sourceTabVk') },
    { value: 'proxy', label: t('pages:traffic.sourceTabProxy') },
    { value: 'agent', label: t('pages:traffic.sourceTabAgent') },
  ];
}

const VALID_TAB_VALUES = new Set<TrafficSourceFilter>(['', 'vk', 'proxy', 'agent']);
const TAB_LOCAL_FILTER_URL_KEYS = ['thingId', 'thingName', 'eventId', 'status', 'model'];

/* -- Main Page -- */

export function TrafficAnalyticsPage() {
  const { t } = useTranslation();
  const [searchParams, setSearchParams] = useSearchParams();
  const SOURCE_TABS = getSourceTabs(t);

  // Read the active tab from `?source=`; empty string (no param) = All.
  // Coerce unknown values back to '' so a bookmarked typo doesn't break the
  // Tabs control (which compares value strings exactly).
  const rawSource = searchParams.get('source') ?? '';
  const sourceTab: TrafficSourceFilter =
    VALID_TAB_VALUES.has(rawSource as TrafficSourceFilter)
      ? (rawSource as TrafficSourceFilter)
      : '';

  const handleTabChange = useCallback(
    (v: string) => {
      const next = new URLSearchParams(searchParams);
      if (v === '') {
        next.delete('source');
      } else {
        next.set('source', v);
      }
      for (const key of TAB_LOCAL_FILTER_URL_KEYS) {
        next.delete(key);
      }
      // Replace so rapid tab-clicking doesn't pollute browser history.
      setSearchParams(next, { replace: true });
    },
    [searchParams, setSearchParams],
  );

  return (
    <Stack gap="lg">
      <div className={css.liveTrafficHeader}>
        <PageHeader title={t('pages:traffic.liveTraffic')} subtitle={t('pages:traffic.subtitle')} />
      </div>

      <Tabs value={sourceTab} onValueChange={handleTabChange}>
        <TabsList className={css.sourceTabsList}>
          {SOURCE_TABS.map((st) => (
            <TabsTrigger key={st.value} value={st.value} className={css.sourceTabsTrigger}>{st.label}</TabsTrigger>
          ))}
        </TabsList>
        <TabsContent key={sourceTab || 'all'} value={sourceTab}>
          <TrafficTab source={sourceTab} />
        </TabsContent>
      </Tabs>
    </Stack>
  );
}
