'use client';

/**
 * 页面数据缓存层：切页回来先出缓存、后台静默刷新（SWR 语义）。
 *
 * 为什么需要：面板页面每次挂载都从零拉数据，而最慢的端点（/api/packages
 * 逐账号向上游查余额、/api/tasks/scan_all 逐账号扫任务）要 1-2 秒才返回，
 * 表现为「切页后页面先空着/骨架屏，等一两秒数据才蹦出来」。
 *
 * 三层结构：
 *  - 内存 Map：本会话内的热缓存，命中零延迟；
 *  - sessionStorage：整页刷新后仍能先出上一次的数据（刷新不再从骨架屏开始）；
 *  - 网络刷新：拿到新数据后写回两层缓存。
 *
 * 登录态与语言都在 sessionStorage 之外由各自的 Provider 管理；这里缓存的是
 * 「只读快照」，对它做写操作的页面（保存配置等）成功后调 invalidate 清掉
 * 对应键即可。
 */
import {useCallback, useEffect, useRef, useState} from 'react';

const PREFIX = 'wb-cache:';

/* ── 内存层 ─────────────────────────────────────────────── */
const mem = new Map<string, {data: unknown; at: number}>();

function storageRead<T>(key: string): {data: T; at: number} | null {
  try {
    const raw = window.sessionStorage.getItem(PREFIX + key);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as {data: T; at: number};
    return typeof parsed?.at === 'number' ? parsed : null;
  } catch {
    return null;
  }
}

function storageWrite<T>(key: string, data: T, at: number): void {
  try {
    window.sessionStorage.setItem(PREFIX + key, JSON.stringify({data, at}));
  } catch {
    /* 超配额（大响应）/隐私模式：内存层照常工作 */
  }
}

/** 读缓存：内存优先，未命中再查 sessionStorage（读到的回填内存）。 */
export function peekCache<T>(key: string): {data: T; at: number} | null {
  const hit = mem.get(key);
  if (hit) return hit as {data: T; at: number};
  const stored = storageRead<T>(key);
  if (stored) mem.set(key, stored);
  return stored;
}

/** 写缓存（两层同写，时间戳取现在）。 */
export function putCache<T>(key: string, data: T): void {
  const at = Date.now();
  mem.set(key, {data, at});
  storageWrite(key, data, at);
}

/** 清缓存（内存 + sessionStorage）。数据变更后调用，避免下次切页读到旧值。 */
export function invalidateCache(key: string): void {
  mem.delete(key);
  try {
    window.sessionStorage.removeItem(PREFIX + key);
  } catch {
    /* 忽略 */
  }
}

/** 退出登录时清空全部缓存：下一个用户不应看到上一个用户的数据。 */
export function clearAllCache(): void {
  mem.clear();
  try {
    const doomed: string[] = [];
    for (let i = 0; i < window.sessionStorage.length; i++) {
      const k = window.sessionStorage.key(i);
      if (k?.startsWith(PREFIX)) doomed.push(k);
    }
    for (const k of doomed) window.sessionStorage.removeItem(k);
  } catch {
    /* 忽略 */
  }
}

/**
 * 页面级取数 Hook。
 *
 * 行为：
 *  - 挂载时若缓存命中：立刻用缓存渲染（快照超过 TTL 也先出，比骨架屏好），
 *    同时后台拉新，拿到后静默替换——「先见后新」，不做加载闪烁；
 *  - 缓存未命中：才走 loading（骨架屏），此时确实没有可看的东西；
 *  - 刷新函数签名与原页面 load 一致，页面可按需重复调用（如手动刷新按钮）。
 *
 * fetcher 返回的数据必须是可 JSON 序列化的纯对象（API 响应天然满足）。
 */
export function useCachedAsync<T>(
  key: string,
  fetcher: () => Promise<T>,
  options: {ttl?: number} = {},
): {data: T | null; loading: boolean; refreshing: boolean; refresh: () => Promise<T | null>} {
  const {ttl = 0} = options;
  // fetcher 挂到 ref：调用方通常传内联箭头函数，不该让它触发重取
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  const [data, setData] = useState<T | null>(() => peekCache<T>(key)?.data ?? null);
  // 有缓存就不是「加载中」——骨架屏只给真正空手的场景
  const [loading, setLoading] = useState(() => !peekCache<T>(key));
  // 「刷新中」（缓存已显示数据、后台在拉新）：给按钮转圈用，不触发骨架屏
  const [refreshing, setRefreshing] = useState(false);
  // 在途请求所属的 key（null=无在途）。不能是裸布尔：切时间窗（stats 页 hours）
  // 时旧 key 的在途请求若占着全局锁，新 key 的 refresh 会被整个吞掉——表格
  // 停在旧窗口的数据直到下一轮心跳才自愈，看起来就是「切筛选没反应」。
  // 同 key 并发仍去重；不同 key 各取各的，响应只落各自的缓存键。
  const busyKey = useRef<string | null>(null);
  // 当前 key 的镜像：在途响应解析回来时，key 可能已经切走——此时只写缓存，
  // 不 setData 污染新视图。
  const keyRef = useRef(key);
  keyRef.current = key;

  // key 变化（如 logs/stats 页的 limit/hours 换挡）= 换了一份快照：立即重置
  // data/loading 到新 key 的缓存态，否则旧 key 的数据会闪现在新 key 的视图里。
  useEffect(() => {
    const hit = peekCache<T>(key);
    setData(hit?.data ?? null);
    setLoading(!hit);
    // 只随 key 变化执行；refresh 的依赖含 key，此处刻意不追
  }, [key]);

  const refresh = useCallback(async (): Promise<T | null> => {
    if (busyKey.current === key) return null;
    busyKey.current = key;
    setRefreshing(true);
    try {
      const fresh = await fetcherRef.current();
      putCache(key, fresh);
      if (keyRef.current === key) {
        setData(fresh);
      }
      return fresh;
    } catch (e) {
      // 错误仍交给调用方处理（notify.err 在各页面自己调）；缓存值保留继续展示
      throw e;
    } finally {
      // 只有仍持有锁（没被别的 key 覆盖）时才清：旧请求后到时不抢新请求的账
      if (busyKey.current === key) {
        busyKey.current = null;
        setRefreshing(false);
        setLoading(false);
      }
    }
  }, [key]);

  useEffect(() => {
    // TTL 未过期时跳过后台刷新：短间隔切页不打上游（心跳定时器仍会周期刷新）
    const hit = peekCache<T>(key);
    if (hit && ttl > 0 && Date.now() - hit.at < ttl) return;
    refresh().catch(() => {/* 错误已在页面层上报；这里吞掉避免 unhandled rejection */});
  }, [key, ttl, refresh]);

  return {data, loading, refreshing, refresh};
}

/**
 * 预热：登录后 / 布局挂载后提前把慢端点拉进缓存。
 * 失败静默（未登录 / 网络抖动都不该打扰用户），并发触发不排队。
 */
export function warmCache(entries: {key: string; fetcher: () => Promise<unknown>}[]): void {
  for (const {key, fetcher} of entries) {
    const hit = mem.get(key);
    if (hit) continue;
    fetcher()
      .then((data) => putCache(key, data))
      .catch(() => {/* 预热失败留给页面自己的加载兜底 */});
  }
}
