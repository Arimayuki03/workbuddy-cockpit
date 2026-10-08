'use client';

/**
 * 设置页共享状态（从原 settings/page.tsx **原样搬移**，只拆不重构）。
 *
 * 原来整页是单个组件，四个 Tab 分区共用同一份 state / 保存函数。拆成子组件后
 * 这些东西挂在 Provider 上：Provider 在外壳（page.tsx）渲染一次，四个 Tab
 * 共享同一个实例——顶部「重新加载」能刷新所有数据源，modelMap 保存后写缓存
 * 的约定也保持原样。各 hook 的行为与原实现完全一致，只有归属变了。
 */
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import {notify} from '@/lib/toast';
import {useI18n} from '@/lib/i18n/provider';
import {settingsApi, modelApi, errText} from '@/lib/api';
import {peekCache, putCache, useCachedAsync} from '@/lib/data-cache';
import type {ConfigGetResponse, ModelMapResponse, UpdateCheck} from '@/lib/types';
import {useRealm} from '@/lib/realm-context';
import {
  FIELD_BY_KEY,
  GROUPS,
  needsRestartBySection,
  pickValues,
  emptyForm,
  toWire,
  type FieldValue,
  type Section,
} from './settings-fields';

interface SettingsContextValue {
  /** 可视化表单状态 */
  form: Record<Section, Record<string, FieldValue>>;
  setField: (group: Section, key: string, value: FieldValue) => void;
  isDirty: (group: Section) => boolean;
  saveGroup: (group: Section) => Promise<void>;
  resetGroup: (group: Section) => void;
  /** 高级模式（直接编辑整个配置 JSON） */
  advanced: boolean;
  setAdvanced: (v: boolean) => void;
  rawText: string;
  setRawText: (v: string) => void;
  rawDirty: boolean;
  saveJson: (text: string) => Promise<void>;
  /** 配置快照与加载态 */
  cfg: ConfigGetResponse | null;
  cfgReady: boolean;
  configLoading: boolean;
  /** 模型映射 */
  modelMap: Record<string, string>;
  setModelMap: React.Dispatch<React.SetStateAction<Record<string, string>>>;
  saveModelMap: (next: Record<string, string>) => Promise<void>;
  /** 模型列表（映射目标下拉用） */
  models: string[];
  /** 新增映射表单（alias → target） */
  mapAlias: string;
  setMapAlias: React.Dispatch<React.SetStateAction<string>>;
  mapTarget: string;
  setMapTarget: React.Dispatch<React.SetStateAction<string>>;
  /** Upstash（Redis 持久化）表单 */
  upstashForm: {url: string; token: string};
  setUpstashForm: React.Dispatch<React.SetStateAction<{url: string; token: string}>>;
  setUpstashBusy: React.Dispatch<React.SetStateAction<boolean>>;
  upstashBusy: boolean;
  upstashDirty: boolean;
  saveUpstash: () => Promise<void>;
  /** 版本检查 */
  update: UpdateCheck | null;
  updateBusy: boolean;
  recheck: () => Promise<void>;
  /** 顶部「重新加载」 */
  busy: boolean;
  reloadAll: () => Promise<void>;
}

const SettingsContext = createContext<SettingsContextValue | null>(null);

export function useSettings(): SettingsContextValue {
  const ctx = useContext(SettingsContext);
  if (!ctx) throw new Error('useSettings must be used within <SettingsProvider>');
  return ctx;
}

export function SettingsProvider({children}: {children: ReactNode}) {
  const {t, tp} = useI18n();
  const {realm} = useRealm();
  const [cfg, setCfg] = useState<ConfigGetResponse | null>(null);
  const [models, setModels] = useState<string[]>([]);

  /** 可视化表单状态 */
  const [form, setForm] = useState<Record<Section, Record<string, FieldValue>>>(emptyForm());
  /** 加载时的原始值，用于只提交改动过的项 */
  const original = useRef<Record<Section, Record<string, FieldValue>>>(emptyForm());
  /** 高级模式（直接编辑整个配置 JSON） */
  const [advanced, setAdvanced] = useState(false);
  const [rawText, setRawText] = useState('');
  /** 加载时的原始 JSON 文本，用于判断高级模式是否有改动 */
  const rawOriginal = useRef('');

  const [modelMap, setModelMap] = useState<Record<string, string>>({});
  const [mapAlias, setMapAlias] = useState('');
  const [mapTarget, setMapTarget] = useState('');
  const [busy, setBusy] = useState(false);
  /** Upstash（Redis 持久化）表单 */
  const [upstashForm, setUpstashForm] = useState({url: '', token: ''});
  const [upstashBusy, setUpstashBusy] = useState(false);
  /** 加载时的 Upstash 原始值，用于判断是否有改动（token 不回显，不计入比较） */
  const upstashOriginal = useRef({url: ''});
  /** 版本检查 */
  const [update, setUpdate] = useState<UpdateCheck | null>(null);
  const [updateBusy, setUpdateBusy] = useState(false);

  // 三个配置端点都是本地快读，但也接缓存：切页先出上次的表单（TTL 内不重拉），
  // 「重新加载」按钮强制刷新。config 变化是保存动作的依据，写操作成功后手动
  // 刷新缓存（见各 save 函数里的 putCache）。
  const configCache = useCachedAsync<ConfigGetResponse>(
    'settings:config',
    () => settingsApi.config(),
    {ttl: 15_000},
  );
  const mapCache = useCachedAsync<ModelMapResponse>(
    'settings:modelMap',
    () => settingsApi.modelMap(),
    {ttl: 15_000},
  );
  const updateCache = useCachedAsync<UpdateCheck>(
    'settings:update',
    () => settingsApi.checkUpdate(),
    {ttl: 60_000},
  );

  /** 把 config 快照应用到可视化表单（load 与缓存回填共用）。 */
  function applyConfig(v: ConfigGetResponse) {
    setCfg(v);
    const root = (v.config ?? {}) as Record<string, Record<string, unknown>>;
    const picked: Record<Section, Record<string, FieldValue>> = {
      schedule: pickValues(GROUPS.find((g) => g.id === 'schedule')!.fields, root.schedule),
      prompt: pickValues(GROUPS.find((g) => g.id === 'prompt')!.fields, root.prompt),
      cooldown: pickValues(GROUPS.find((g) => g.id === 'cooldown')!.fields, root.cooldown),
      pool: pickValues(GROUPS.find((g) => g.id === 'pool')!.fields, root.pool),
      features: pickValues(GROUPS.find((g) => g.id === 'features')!.fields, root.features),
      session: pickValues(GROUPS.find((g) => g.id === 'session')!.fields, root.session_sticky),
      upstream: pickValues(GROUPS.find((g) => g.id === 'upstream')!.fields, root.upstream),
      global: pickValues(GROUPS.find((g) => g.id === 'global')!.fields, root.global),
    };
    // 保存一个分组成功后 load() 会整表 applyConfig。若直接全量覆盖，
    // 其他分组里未保存的编辑会被无提示清空——这里在 setState 前先算出
    // 脏分组（isDirty 依赖当前 form 与 original.current，必须先读后写）：
    // 脏分组保留用户编辑中的值，非脏分组用服务端值；被保存的分组此刻已
    // 与 original 一致（clean），会正常拿到刚保存的服务端值。
    const dirtyGroups = new Set<Section>(
      GROUPS.map((g) => g.id).filter((id) => {
        const cur = form[id];
        const org = original.current[id];
        return Object.keys(cur).some((k) => cur[k] !== org[k]);
      }),
    );
    setForm((prev) => {
      const merged = {...prev};
      for (const id of Object.keys(picked) as Section[]) {
        // 脏分组保留编辑中的值，其余用服务端值
        merged[id] = dirtyGroups.has(id) ? {...prev[id]} : {...picked[id]};
      }
      return merged;
    });
    // original 一律用服务端值：被保存的分组会因此变 clean，其余脏分组的
    // 用户编辑相对新 original 仍然算脏，编辑不会丢也不会被误判为已保存。
    original.current = Object.fromEntries(
      Object.entries(picked).map(([k, v2]) => [k, {...v2}]),
    ) as Record<Section, Record<string, FieldValue>>;
    setRawText(JSON.stringify(v.config ?? {}, null, 2));
    // url 可回显；token 不回显明文，留空表示不修改
    const upstash = root.upstash as {url?: string; has_token?: boolean; token_masked?: string} | undefined;
    const upstashUrl = upstash?.url || '';
    setUpstashForm({url: upstashUrl, token: ''});
    upstashOriginal.current = {url: upstashUrl};
    rawOriginal.current = JSON.stringify(v.config ?? {}, null, 2);
  }

  const load = useCallback(async () => {
    const [c, mm, up] = await Promise.allSettled([
      configCache.refresh(),
      mapCache.refresh(),
      updateCache.refresh(),
    ]);
    // 配置是整页表单的数据源：拉取失败绝不能静默——否则页面会用默认值
    // 渲染出一张「看起来可保存」的表单，误导用户把默认值当成当前配置。
    // 失败时 toast 报错；渲染层看到「加载结束但 cfgReady=false」会显示
    // 错误态 + 重试按钮，不渲染默认值表单。
    if (c.status === 'rejected') {
      notify.err(errText(c.reason));
    } else if (c.value) {
      applyConfig(c.value);
    }
    if (mm.status === 'fulfilled' && mm.value) {
      // 后端响应是 {ok, map} 信封（session.go handleGetModelMap）：
      // 存整个信封会让「模型映射」表格把嵌套对象当行数据渲染，点击标签页即
      // React 崩溃（Objects are not valid as a React child）——必须拆出 map。
      setModelMap(mm.value?.map ?? {});
    }
    if (up.status === 'fulfilled' && up.value) setUpdate(up.value);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [configCache.refresh, mapCache.refresh, updateCache.refresh]);

  // 缓存命中时（含整页刷新后从 sessionStorage 回放）立即回填表单：
  // 后台刷新拿到新值后会再走一次 applyConfig——两帧内容一致，不会闪。
  const cachedConfig = configCache.data;
  useEffect(() => {
    if (cachedConfig && !cfg) applyConfig(cachedConfig);
    // 仅在首次拿到数据时应用；保存后的刷新由 load 显式触发
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cachedConfig]);

  const cachedMap = mapCache.data;
  useEffect(() => {
    if (cachedMap) setModelMap(cachedMap?.map ?? {});
  }, [cachedMap]);

  const cachedUpdate = updateCache.data;
  useEffect(() => {
    if (cachedUpdate) setUpdate(cachedUpdate);
  }, [cachedUpdate]);

  /** 模型列表（模型映射的目标下拉用）。
   *  reload 必传 true 强制绕过缓存重拉——models 页缓存的 TTL 会让「点刷新」
   *  静默短路成读旧缓存，按钮看起来毫无反应；默认挂载加载才允许读缓存。
   *  缓存键跟随当前 realm（models 页按 `models:${realm}` 分域缓存）：映射
   *  目标下拉优先展示当前版本域的模型，未命中再不带 realm 拉双域混合兜底。 */
  const loadModels = useCallback(async (reload = false) => {
    if (!reload) {
      const cached = peekCache<import('@/lib/types').PanelModelsResponse>(`models:${realm}`);
      if (cached?.data?.models?.length) {
        setModels(cached.data.models.map((m) => m.id));
        return;
      }
    }
    try {
      const res = await modelApi.models();
      setModels((res.models ?? []).map((m) => m.id));
    } catch {
      setModels([]);
    }
  }, [realm]);

  useEffect(() => {
    load();
    loadModels();
  }, [load, loadModels]);

  const cfgReady = !!cfg?.config;

  /** Upstash 是否有未保存改动（token 不回显，输入即视为改动） */
  const upstashDirty =
    upstashForm.url !== upstashOriginal.current.url || upstashForm.token.trim() !== '';
  /** 高级模式 JSON 是否有未保存改动 */
  const rawDirty = rawText !== rawOriginal.current;

  /** 保存 Upstash 配置（token 留空表示保持原值） */
  async function saveUpstash() {
    if (!upstashForm.url.trim()) {
      notify.err(t('settings.upstashUrlRequired'));
      return;
    }
    setUpstashBusy(true);
    try {
      const patch: Record<string, unknown> = {
        upstash: {
          url: upstashForm.url.trim(),
          ...(upstashForm.token.trim() ? {token: upstashForm.token.trim()} : {}),
        },
      };
      const saved = await settingsApi.saveConfig(patch);
      // upstash.* 由 redisstore 在装配期构建（后端 restartRequiredFields
      // 明确带 'upstash'），保存不会热生效——按需重启口径提示，不再一律
      // 报「正在自动应用」误导用户。
      if (saved?.restart_required?.includes('upstash')) {
        notify.warn(t('settings.savedRestart'), t('settings.restartHint'));
      } else {
        notify.ok(t('settings.upstashSaved'), t('settings.applying'));
      }
      setUpstashForm((f) => ({...f, token: ''}));
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setUpstashBusy(false);
    }
  }

  function setField(group: Section, key: string, value: FieldValue) {
    setForm((prev) => ({...prev, [group]: {...prev[group], [key]: value}}));
  }

  /** 是否有未保存的改动 */
  function isDirty(group: Section): boolean {
    const cur = form[group];
    const org = original.current[group];
    return Object.keys(cur).some((k) => cur[k] !== org[k]);
  }

  /** 只提交改动过的字段，避免覆盖其他未展示的配置项 */
  async function saveGroup(group: Section) {
    const cur = form[group];
    const org = original.current[group];
    const patch: Record<string, boolean | number | number[] | string> = {};
    for (const k of Object.keys(cur)) {
      if (cur[k] === org[k]) continue;
      const f = FIELD_BY_KEY[k];
      if (!f) continue;
      const w = toWire(f, cur[k]);
      if (!w.ok) {
        notify.err(t('settings.fieldInvalid', {label: tp(f.label)}), w.error);
        return;
      }
      patch[k] = w.value;
    }
    if (!Object.keys(patch).length) {
      notify.info(t('settings.noChanges'));
      return;
    }
    setBusy(true);
    try {
      const def = GROUPS.find((g) => g.id === group);
      if (!def) return;
      const saved = await settingsApi.saveConfig({[def.section]: patch});
      const needRestart = needsRestartBySection(saved?.restart_required);
      if (needRestart) {
        notify.warn(t('settings.savedRestart'), t('settings.restartHint'));
      } else {
        notify.ok(t('settings.saved'), t('settings.applying'));
      }
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(false);
    }
  }

  function resetGroup(group: Section) {
    setForm((prev) => ({...prev, [group]: {...original.current[group]}}));
  }

  /** 高级模式：直接保存整个配置 JSON（后端深合并 + 原子替换 + 保留未知键） */
  async function saveJson(text: string) {
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch {
      notify.err(t('settings.jsonInvalid'));
      return;
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      notify.err(t('settings.jsonNotObject'));
      return;
    }
    setBusy(true);
    try {
      const saved = await settingsApi.saveConfig(parsed as Record<string, unknown>);
      const needRestart = (saved?.restart_required ?? []).length > 0;
      if (needRestart) {
        notify.warn(t('settings.savedRestart'), t('settings.restartHint'));
      } else {
        notify.ok(t('settings.saved'), t('settings.applying'));
      }
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(false);
    }
  }

  async function saveModelMap(next: Record<string, string>) {
    try {
      // 响应为 {ok, map, error?} 信封：ok=false 是写盘失败（映射内存已生效、
      // 重启回落 config 值），必须按失败提示服务端错误——不能静默当成功。
      const res = await settingsApi.saveModelMap(next);
      setModelMap(res?.map ?? next);
      // 缓存里存的正是 {ok, map} 信封，与 GET 响应同构（load 拆信封的约定）。
      // 不写回的话 15s TTL 内切走再切回会用旧表回填，刚删的条目「复活」。
      putCache('settings:modelMap', res ?? {ok: true, map: next});
      if (res?.ok === false) {
        notify.err(res.error || t('settings.saveFailed'), t('settings.modelMapHotOnly'));
      } else {
        notify.ok(t('settings.modelMapSaved'));
      }
    } catch (e) {
      notify.err(errText(e));
    }
  }

  /** 版本检查（只读，不做自更新） */
  async function recheck() {
    setUpdateBusy(true);
    try {
      const r = await settingsApi.checkUpdate();
      setUpdate(r);
      if (r.has_update) {
        notify.warn(t('updatePanel.newVersion'), t('updatePanel.hasUpdate', {v: r.latest}));
      } else {
        notify.ok(t('updatePanel.upToDate'));
      }
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setUpdateBusy(false);
    }
  }

  /** 顶部「重新加载」：强制重拉配置/映射/版本/模型列表。
   *  busy 期间按钮转圈并禁用；完成/失败都给 toast——之前点击后毫无反馈，
   *  用户不知道有没有生效。 */
  async function reloadAll() {
    setBusy(true);
    try {
      await Promise.allSettled([load(), loadModels(true)]);
      notify.ok(t('settings.reloaded'));
    } finally {
      setBusy(false);
    }
  }

  return (
    <SettingsContext.Provider
      value={{
        form,
        setField,
        isDirty,
        saveGroup,
        resetGroup,
        advanced,
        setAdvanced,
        rawText,
        setRawText,
        rawDirty,
        saveJson,
        cfg,
        cfgReady,
        configLoading: configCache.loading,
        modelMap,
        setModelMap,
        saveModelMap,
        models,
        mapAlias,
        setMapAlias,
        mapTarget,
        setMapTarget,
        upstashForm,
        setUpstashForm,
        setUpstashBusy,
        upstashBusy,
        upstashDirty,
        saveUpstash,
        update,
        updateBusy,
        recheck,
        busy,
        reloadAll,
      }}
    >
      {children}
    </SettingsContext.Provider>
  );
}
