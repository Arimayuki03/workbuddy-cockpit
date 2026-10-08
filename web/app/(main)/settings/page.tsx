'use client';

/**
 * 设置页外壳：二级导航 + 当前 Tab 的内容。
 *
 * **为什么用 `?tab=` searchParams 而不是 `/settings/<tab>` 动态段**：
 * (main)/layout.tsx 的切页动画以 `pathname` 为 key 挂载内容区——若拆成
 * `/settings/<tab>` 子路由，每个 Tab 切换都是一次 pathname 变化，整个设置页
 * （含所有表单 state）会整树卸载重挂载，未保存的编辑全部丢失，与旧版
 * Tabs 的状态保活行为不一致。`?tab=` 只动 query，pathname 恒为 `/settings`，
 * 外壳与 SettingsProvider 不随 Tab 切换重挂载：跨 Tab 的状态、数据缓存与
 * 「重新加载」语义与拆分前完全相同，静态导出下也无需 generateStaticParams。
 *
 * 深链：`/settings?tab=models` 直达对应分区；未知 / 缺省 slug 落到第一个
 * 分区（DEFAULT_SETTINGS_TAB）。Tab 清单见 settings-tabs.ts。
 * ManagementBar / 命令面板等入口仍指向 `/settings`（= 默认分区），无需改动。
 *
 * 拆分原则：只拆不重构。原四个 TabsContent 分区原样搬到了
 * web/components/settings/ 下的 ConfigTab / ModelsTab / SystemTab / AboutTab，
 * 共享状态与请求在 settings-context.tsx（Provider 挂在下面这层，Tab 切换不丢）。
 */
import {Suspense, useEffect, useState} from 'react';
import {useSearchParams} from 'next/navigation';
import {RefreshCw, Server, Shuffle, Info, Sparkles, KeyRound} from 'lucide-react';
import {useI18n} from '@/lib/i18n/provider';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {Button} from '@/components/ui/button';
import {Tabs, TabsContent, TabsList, TabsTrigger} from '@/components/ui/tabs';
import {SettingsProvider, useSettings} from '@/components/settings/settings-context';
import {ConfigTab} from '@/components/settings/ConfigTab';
import {ModelsTab} from '@/components/settings/ModelsTab';
import {TokensTab} from '@/components/settings/TokensTab';
import {SystemTab} from '@/components/settings/SystemTab';
import {AboutTab} from '@/components/settings/AboutTab';
import {
  DEFAULT_SETTINGS_TAB,
  SETTINGS_TAB_LABEL_KEYS,
  settingsTabFromSearch,
  type SettingsTab,
} from '@/components/settings/settings-tabs';

const TAB_ICONS = {
  config: <Server className="mr-1.5 h-3.5 w-3.5" />,
  models: <Shuffle className="mr-1.5 h-3.5 w-3.5" />,
  tokens: <KeyRound className="mr-1.5 h-3.5 w-3.5" />,
  system: <Sparkles className="mr-1.5 h-3.5 w-3.5" />,
  about: <Info className="mr-1.5 h-3.5 w-3.5" />,
} as const;

/** 顶部「重新加载」：状态与动作在 settings-context（原 PageHeader actions）。 */
function ReloadButton() {
  const {t} = useI18n();
  const {busy, reloadAll} = useSettings();
  return (
    <Button
      variant="outline"
      size="sm"
      className="rounded-full"
      title={t('settings.reloadTitle')}
      disabled={busy}
      onClick={reloadAll}
    >
      <RefreshCw className={busy ? 'animate-spin' : ''} />
      {t('settings.reload')}
    </Button>
  );
}

function SettingsBody() {
  const {t} = useI18n();
  const searchParams = useSearchParams();
  const search = searchParams.toString();

  // `?tab=` 解析在客户端进行：静态导出下首帧先渲染默认分区，挂载后若有合法
  // 的 tab 参数则同步过去。无效 slug 一律视为缺省（第一个分区）。
  const [tab, setTab] = useState<SettingsTab>(DEFAULT_SETTINGS_TAB);
  useEffect(() => {
    const parsed = settingsTabFromSearch(search);
    setTab(parsed ?? DEFAULT_SETTINGS_TAB);
  }, [search]);

  // Tab 切换时把 slug 写回地址栏（replace 不留历史）：浏览器后退/刷新、分享
  // 链接都能定位到当前分区。错误/缺省 slug 不清洗地址，避免与历史导航打架。
  function switchTab(next: string) {
    const v = next as SettingsTab;
    setTab(v);
    const url = new URL(window.location.href);
    url.searchParams.set('tab', v);
    window.history.replaceState(null, '', url.toString());
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('settings.title')}
        description={t('settings.description')}
        actions={
          <ReloadButton />
        }
      />

      <Tabs value={tab} onValueChange={switchTab}>
        {/* 标签较多，手机上会撑破容器，这里允许横向滚动 */}
        <div className="-mx-1 overflow-x-auto px-1 pb-1">
          <TabsList className="w-max">
            {(Object.keys(TAB_ICONS) as (keyof typeof TAB_ICONS)[]).map((key) => (
              <TabsTrigger key={key} value={key}>
                {TAB_ICONS[key]}
                {t(SETTINGS_TAB_LABEL_KEYS[key])}
              </TabsTrigger>
            ))}
          </TabsList>
        </div>

        {/* 原版 TabsContent 自带 mt-3/mt-4 分区上边距；搬迁进各 Tab 组件根部，
            外壳统一清零避免双倍间距。 */}
        <TabsContent value="config" className="mt-0">
          <ConfigTab />
        </TabsContent>
        <TabsContent value="models" className="mt-0">
          <ModelsTab />
        </TabsContent>
        {/* 访问令牌是独立 CRUD 资源，不走 SettingsProvider 的配置保存逻辑，
            理由见 TokensTab 组件头注释。 */}
        <TabsContent value="tokens" className="mt-0">
          <TokensTab />
        </TabsContent>
        <TabsContent value="system" className="mt-0">
          <SystemTab />
        </TabsContent>
        <TabsContent value="about" className="mt-0">
          <AboutTab />
        </TabsContent>
      </Tabs>
    </div>
  );
}

export default function SettingsPage() {
  return (
    <SettingsProvider>
      {/* useSearchParams 在静态导出预渲染时必须包 Suspense 边界（CSR bailout）：
          首帧渲染默认分区，客户端水合后再按 ?tab= 同步。 */}
      <Suspense fallback={<div className="flex flex-col gap-4 md:gap-6" aria-hidden />}>
        <SettingsBody />
      </Suspense>
    </SettingsProvider>
  );
}
