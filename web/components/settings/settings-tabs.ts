/**
 * 设置页 Tab 清单与 slug 解析（从原 settings/page.tsx 的 Tabs 结构拆出）。
 *
 * Tab 的名字（value、顺序、默认项）在这里只存一份：外壳渲染导航、`?tab=` 深
 * 链解析都从这里读，加 Tab 时改一处即可，不会出现「清单里有、地址进不去」或
 * 反过来的漂移。
 *
 * 深链形状：`/settings?tab=<slug>`（kebab-case）。选 searchParams 而不是
 * `/settings/<tab>` 动态段，原因见 settings/page.tsx 顶部说明——(main) 布局
 * 以 pathname 为 key 做切页动画，子路径导航会整树重挂载、丢失未保存的表单
 * 编辑；query 变化 pathname 不动，与旧版 Tabs 的状态保活语义一致，且静态
 * 导出下无需 generateStaticParams，页数不膨胀。
 */

/** 与原 TabsTrigger 的 value 一一对应，顺序即导航顺序。kebab-case slug。 */
export const SETTINGS_TABS = [
  'config',
  'models',
  'tokens',
  'system',
  'about',
] as const;

export type SettingsTab = (typeof SETTINGS_TABS)[number];

/** `/settings` 与无 `?tab=` 时落到它。与 SETTINGS_TABS[0] 同值，语义是「默认」。 */
export const DEFAULT_SETTINGS_TAB: SettingsTab = 'config';

export function isSettingsTab(value: unknown): value is SettingsTab {
  return typeof value === 'string' && (SETTINGS_TABS as readonly string[]).includes(value);
}

/** 解析 `?tab=` 的取值；未知或缺省返回 null（调用方回落默认 Tab）。 */
export function settingsTabFromSearch(search: string | null | undefined): SettingsTab | null {
  if (!search) return null;
  const raw = new URLSearchParams(search).get('tab');
  return isSettingsTab(raw ?? undefined) ? raw as SettingsTab : null;
}

/**
 * 每个 Tab 的**标签 i18n key** 与**图标 i18n 无关部分**分开存：命令面板 /
 * ManagementBar 若要按 Tab 列条目，标签从这里读，避免漏抄一份。
 * 图标是 JSX，留在外壳里。
 */
export const SETTINGS_TAB_LABEL_KEYS: Record<SettingsTab, string> = {
  config: 'settings.tabUpstream',
  models: 'settings.tabModels',
  tokens: 'settings.tabTokens',
  system: 'settings.tabSystem',
  about: 'settings.tabAbout',
};
