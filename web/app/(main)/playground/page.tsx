'use client';

import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import {
  Bot,
  Brain,
  ChevronDown,
  Coins,
  Eraser,
  Info,
  Loader2,
  MessageSquare,
  MessagesSquare,
  RefreshCw,
  Route,
  ThumbsDown,
  ThumbsUp,
  User as UserIcon,
} from 'lucide-react';

import {PageHeader} from '@/components/common/layout/PageHeader';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {AiChatInput, type ChatModelOption} from '@/components/ui/ai-chat-input';
import {CopyButton} from '@/components/ui/copy-button';
import {modelApi, errText} from '@/lib/api';
import {notify} from '@/lib/toast';
import {useT} from '@/lib/i18n/provider';
import {fmtCredit} from '@/lib/format';
import {cn} from '@/lib/utils';
import {useAuth} from '@/lib/auth-context';
import {useRealm} from '@/lib/realm-context';

/** 测试台消息的最小形态（构造请求体用，脱掉 credit/thinking 等渲染字段） */
interface WireMsg {
  role: 'user' | 'assistant';
  content: string;
}

/**
 * 把测试台的消息列表构造为 Anthropic Messages 请求体。
 *
 * 对齐网关 internal/server/anthropic.go 的解析面：
 *   - content 简化为纯 text block（本页没有图片/工具块，text 最稳）；
 *   - max_tokens 必填（缺失网关直接 400）：测试台无对应设置，固定 4096；
 *   - thinking.type 用 "adaptive"：网关对 enabled/adaptive 同样算启用
 *     （anthThinkingEnabled），adaptive 对不在官方能力表里的第三方模型名
 *     也成立，是 Claude Code 官方客户端的现行写法；
 *   - 思考强度经 output_config.effort 透传（anthReasoningEffort 的第一
 *     优先来源），档位降级交给上游管线。
 */
function buildAnthropicBody(msgs: WireMsg[], model: string, effort: string): Record<string, unknown> {
  const body: Record<string, unknown> = {
    model,
    max_tokens: 4096,
    stream: true,
    // Anthropic 要求首条消息是 user；测试台天然如此（用户先开口）。
    messages: msgs.map((m) => ({
      role: m.role,
      content: [{type: 'text', text: m.content}],
    })),
  };
  if (effort) {
    // output_config.effort 是网关 anthReasoningEffort 的第一优先档位来源；
    // thinking 开关决定网关是否回 thinking 块（enabled 与 adaptive 等价）。
    body.output_config = {effort};
    body.thinking = {type: 'adaptive'};
  }
  return body;
}

/**
 * 把消息列表构造为 OpenAI Responses 请求体（对齐 responses.go）：
 * instructions 承载 system（不在 input 里），input 是 {role, content}
 * 扁平数组（字符串 content 即合法形态）；思考强度经 reasoning.effort
 * 透传成上游 reasoning_effort。
 */
function buildResponsesBody(msgs: WireMsg[], model: string, effort: string): Record<string, unknown> {
  const body: Record<string, unknown> = {
    model,
    stream: true,
    input: msgs.map((m) => ({role: m.role, content: m.content})),
  };
  if (effort) {
    body.reasoning = {effort};
  }
  return body;
}

/**
 * 测试台支持的三种网关协议。值即端点路径段：
 *   chat      → POST /v1/chat/completions（OpenAI Chat 格式）
 *   anthropic → POST /v1/messages（Anthropic Messages 格式）
 *   responses → POST /v1/responses（OpenAI Responses 格式）
 * 鉴权三条路完全同口径（会话 cookie 等价 Bearer），切换只影响请求体
 * 构造与 SSE 事件解析。
 */
type Protocol = 'chat' | 'anthropic' | 'responses';

const PROTOCOLS: Protocol[] = ['chat', 'anthropic', 'responses'];

/** 协议 → 工具条图标（融入现有胶囊风格的小图标） */
const PROTOCOL_ICONS: Record<Protocol, typeof MessageSquare> = {
  chat: MessageSquare,
  anthropic: MessagesSquare,
  responses: Route,
};

/** thinking 块累积的临时结构（流式期间逐步拼，结束再落进 Msg） */
interface Thinking {
  content: string;
  collapsed: boolean;
}

interface Msg {
  role: 'user' | 'assistant';
  content: string;
  /** 本条回答的实测消耗（上游 usage.credit） */
  credit?: number | null;
  tokens?: number;
  /** 思考过程（Anthropic thinking_delta / Responses reasoning_summary_text.delta） */
  thinking?: Thinking;
  /** 本条回答使用的协议（气泡徽标展示，便于对照） */
  protocol?: Protocol;
  /** 是否由流式拼出来的（用于显示光标/反馈按钮） */
  done?: boolean;
  error?: boolean;
}

export default function PlaygroundPage() {
  const t = useT();
  const {isAdmin} = useAuth();
  const {realm, label: realmName} = useRealm();
  const [models, setModels] = useState<ChatModelOption[]>([]);
  const [model, setModel] = useState('');
  const [effort, setEffort] = useState('');
  const [input, setInput] = useState('');
  const [msgs, setMsgs] = useState<Msg[]>([]);
  const [streaming, setStreaming] = useState(false);
  /** 本次会话累计消耗，实时显示在右下角 */
  const [sessionCredit, setSessionCredit] = useState(0);
  /**
   * 会话内是否真正见过 usage.credit。只有 Chat 协议的上游回 credit；
   * Anthropic/Responses 只有 input/output_tokens，若照常渲染徽章会把
   * 初始 0 当成「消耗了 0 积分」误导用户，故未见 credit 时整个隐藏。
   */
  const [sessionHasCredit, setSessionHasCredit] = useState(false);
  /** 当前协议（chat = Chat Completions，anthropic = Messages，responses = Responses） */
  const [protocol, setProtocol] = useState<Protocol>('chat');

  const abortRef = useRef<AbortController | null>(null);
  const scrollerRef = useRef<HTMLDivElement>(null);
  const pinToBottom = useRef(true);

  /**
   * 切协议：立即中止在途流（三种协议请求体互不相认，旧流的剩余分片对
   * 新协议毫无意义），并丢弃已渲染的思考折叠状态，避免下一个协议解析出
   * 混不上的事件类型。对话内容保留——对比同一问题在三种协议下的表现
   * 正是测试台的核心用途。
   */
  const switchProtocol = useCallback((next: Protocol) => {
    setProtocol((cur) => {
      if (cur !== next) {
        abortRef.current?.abort();
        setMsgs((prev) => prev.map((m) => (m.thinking ? {...m, thinking: {...m.thinking, collapsed: true}} : m)));
        // 新协议的 usage 形态不同（无 credit）：重置徽章可见性，避免
        // 沿用上一协议的累计值继续显示造成口径混淆。
        setSessionHasCredit(false);
      }
      return next;
    });
  }, []);

  const loadModels = useCallback(async () => {
    try {
      const r = await modelApi.models(realm);
      // panel models 每条 id 自带 cn:/global: 前缀（即调用时的完整 model 值）；
      // playground 直连 /v1/chat/completions，按当前域过滤后原样使用。
      const prefix = realm === 'global' ? 'global:' : 'cn:';
      const list = (r.models ?? [])
        .filter((m) => m.id.startsWith(prefix))
        .map((m) => ({
          id: m.id,
          name: m.name || m.id,
          efforts: m.supported_efforts ?? [],
          series: m.id.slice(prefix.length).split('-')[0],
        }));
      setModels(list);
      // 切版本后旧模型多半不在新列表里，直接选第一个，避免发出去被上游拒
      setModel(list[0]?.id || '');
      setEffort('');
    } catch (e) {
      notify.err(errText(e));
    }
  }, [realm]);

  useEffect(() => {
    loadModels();
  }, [loadModels]);

  // 切换版本 = 换了一套账号池与模型，旧对话留着会造成误解（模型不同、额度不同）。
  // 同时中止仍在进行的流式请求：realm 变化只触发本 effect（组件并不重挂），
  // 不主动 abort 的话旧流会继续读、后台继续耗上游积分，且 setMsgs 会写到空数组
  // 的 [-1] 下标产生幽灵属性。
  useEffect(() => {
    abortRef.current?.abort();
    setMsgs([]);
    setSessionCredit(0);
    setSessionHasCredit(false);
  }, [realm]);

  // 自动滚到底部，但用户主动向上翻看时不要抢滚动位置
  useEffect(() => {
    const el = scrollerRef.current;
    if (!el || !pinToBottom.current) return;
    el.scrollTop = el.scrollHeight;
  }, [msgs]);

  // 卸载时中止仍在进行的流式请求：离开页面（或切版本导致本组件重挂）后
  // 残留的 reader 会继续收 SSE 并对已卸载组件 setState。AbortError 分支
  // 只更新气泡文案，卸载后 setState 被 React 忽略，无副作用。
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
    };
  }, []);

  const onScroll = () => {
    const el = scrollerRef.current;
    if (!el) return;
    pinToBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
  };

  const send = useCallback(async () => {
    const text = input.trim();
    if (!text || streaming) return;
    setInput('');
    pinToBottom.current = true;

    const nextMsgs: Msg[] = [...msgs, {role: 'user', content: text}];
    setMsgs([...nextMsgs, {role: 'assistant', content: '', done: false}]);
    setStreaming(true);

    const ac = new AbortController();
    abortRef.current = ac;
    try {
      // 直连本网关原生 /v1 端点（同源、withCredentials，走会话 cookie 换发的
      // 等价 Bearer 鉴权），不经任何 /api 代理。三种协议鉴权口径一致，
      // 差异只在 URL 与请求体形态。
      const wire: WireMsg[] = nextMsgs.map((m) => ({role: m.role, content: m.content}));
      let url: string;
      let body: Record<string, unknown>;
      if (protocol === 'anthropic') {
        url = '/v1/messages';
        body = buildAnthropicBody(wire, model, effort);
      } else if (protocol === 'responses') {
        url = '/v1/responses';
        body = buildResponsesBody(wire, model, effort);
      } else {
        url = '/v1/chat/completions';
        body = {
          model,
          reasoning_effort: effort || undefined,
          stream: true,
          messages: wire,
        };
      }
      const res = await fetch(url, {
        method: 'POST',
        credentials: 'include',
        headers: {'Content-Type': 'application/json'},
        signal: ac.signal,
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        let detail = `HTTP ${res.status}`;
        try {
          const j = await res.json();
          detail = j.detail || j.error?.message || detail;
        } catch {
          /* 响应不是 JSON，保留状态码 */
        }
        setMsgs((cur) => {
          const copy = [...cur];
          copy[copy.length - 1] = {role: 'assistant', content: detail, error: true, done: true};
          return copy;
        });
        return;
      }

      const reader = res.body?.getReader();
      if (!reader) throw new Error(t('playground.unreadable'));
      const decoder = new TextDecoder();
      let buf = '';
      let answer = '';
      let thinking = '';
      let credit: number | null = null;
      let tokens = 0;

      // 三协议 usage 形态各异，映射到统一的（credit, tokens）：
      //   chat      → usage.credit + prompt/completion_tokens
      //   anthropic → usage.input_tokens/output_tokens（credit 上游不回，置 null 隐藏显示）
      //   responses → usage.input_tokens/output_tokens（同上）
      const ingestUsage = (u: unknown) => {
        if (typeof u !== 'object' || u === null) return;
        const usage = u as Record<string, unknown>;
        if (typeof usage.credit === 'number') credit = usage.credit;
        const input =
          typeof usage.input_tokens === 'number'
            ? usage.input_tokens
            : typeof usage.prompt_tokens === 'number'
              ? usage.prompt_tokens
              : 0;
        const output =
          typeof usage.output_tokens === 'number'
            ? usage.output_tokens
            : typeof usage.completion_tokens === 'number'
              ? usage.completion_tokens
              : 0;
        const total = input + output;
        if (total > 0) tokens = total;
      };

      // 边读边解析 SSE，逐段追加到气泡上（真实流式体验）。三种协议的
      // 事件形态在这里分派；未知/畸形事件一律静默跳过——测试台不该
      // 因为上游多出一种事件类型就崩。
      for (;;) {
        const {done, value} = await reader.read();
        if (done) break;
        buf += decoder.decode(value, {stream: true});
        let idx: number;
        while ((idx = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, idx).trim();
          buf = buf.slice(idx + 1);
          if (!line.startsWith('data:')) continue;
          const payload = line.slice(5).trim();
          if (!payload || payload === '[DONE]') continue;
          try {
            const obj = JSON.parse(payload) as Record<string, unknown>;
            const type = typeof obj.type === 'string' ? obj.type : '';
            if (protocol === 'anthropic') {
              // Anthropic Messages SSE（internal/server/anthropic.go）：
              // message_start{message.usage} / content_block_delta{delta} /
              // message_delta{usage} / message_stop
              if (type === 'content_block_delta') {
                const delta = obj.delta as Record<string, unknown> | undefined;
                if (delta?.type === 'text_delta' && typeof delta.text === 'string') {
                  answer += delta.text;
                } else if (delta?.type === 'thinking_delta' && typeof delta.thinking === 'string') {
                  thinking += delta.thinking;
                }
              } else if (type === 'message_start') {
                const message = obj.message as Record<string, unknown> | undefined;
                if (message) ingestUsage(message.usage);
              } else if (type === 'message_delta') {
                ingestUsage(obj.usage);
              }
            } else if (protocol === 'responses') {
              // OpenAI Responses SSE（internal/server/responses.go）：
              // response.created / response.output_text.delta / 未来的
              // response.reasoning_summary_text.delta / response.completed
              if (type === 'response.output_text.delta' && typeof obj.delta === 'string') {
                answer += obj.delta;
              } else if (
                type === 'response.reasoning_summary_text.delta' &&
                typeof obj.delta === 'string'
              ) {
                thinking += obj.delta;
              } else if (type === 'response.completed' || type === 'response.incomplete') {
                const response = obj.response as Record<string, unknown> | undefined;
                if (response) ingestUsage(response.usage);
              }
            } else {
              // Chat Completions：choices[0].delta.content 增量 + usage
              const choices = obj.choices as Array<Record<string, unknown>> | undefined;
              const delta = choices?.[0]?.delta as Record<string, unknown> | undefined;
              if (typeof delta?.content === 'string' && delta.content) {
                answer += delta.content;
              }
              if (obj.usage) ingestUsage(obj.usage);
            }
            setMsgs((cur) => {
              const copy = [...cur];
              // thinking 只在新协议下累积；折叠状态取当前值（用户流式期间
              // 手动折叠/展开不被后续分片覆盖）
              const lastThinking = copy[copy.length - 1].thinking;
              copy[copy.length - 1] = {
                ...copy[copy.length - 1],
                content: answer,
                thinking:
                  protocol === 'chat'
                    ? undefined
                    : {content: thinking, collapsed: lastThinking?.collapsed ?? false},
                done: false,
              };
              return copy;
            });
          } catch {
            /* 单个分片解析失败不影响后续 */
          }
        }
      }

      const thinkingMsg: Thinking | undefined =
        protocol === 'chat' ? undefined : {content: thinking, collapsed: true};
      setMsgs((cur) => {
        const copy = [...cur];
        copy[copy.length - 1] = {
          role: 'assistant',
          content: answer || t('playground.emptyAnswer'),
          credit,
          tokens,
          thinking: thinkingMsg,
          protocol,
          done: true,
        };
        return copy;
      });
      if (typeof credit === 'number') {
        setSessionCredit((v) => v + credit!);
        setSessionHasCredit(true);
      }
    } catch (e) {
      if ((e as Error).name === 'AbortError') {
        setMsgs((cur) => {
          const copy = [...cur];
          const last = copy[copy.length - 1];
          copy[copy.length - 1] = {...last, done: true, content: last.content || t('playground.stopped')};
          return copy;
        });
      } else {
        setMsgs((cur) => {
          const copy = [...cur];
          copy[copy.length - 1] = {
            role: 'assistant',
            content: errText(e),
            error: true,
            done: true,
          };
          return copy;
        });
      }
    } finally {
      setStreaming(false);
      abortRef.current = null;
    }
  }, [input, msgs, model, effort, protocol, streaming, t]);

  const stop = () => abortRef.current?.abort();

  const clear = () => {
    if (streaming) return;
    setMsgs([]);
    setSessionCredit(0);
    setSessionHasCredit(false);
  };

  const currentModel = useMemo(() => models.find((m) => m.id === model), [models, model]);

  return (
    // flex-1 + min-h-0：让页面占满可用高度，对话区随剩余空间自适应。
    // 原先给对话区写死 calc(100dvh-290px)，一旦底部再加说明块就会顶到浮动
    // 底栏下面被遮住（实测遮了 43px）。改用 flex 后不再依赖魔法数字，
    // 任何视口高度都不会重叠。
    <div className="flex min-h-0 flex-1 flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('playground.title')}
        description={t('playground.description', {realm: realmName})}
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              className="rounded-full"
              onClick={() => {
                loadModels();
                notify.info(t('playground.modelsRefreshed'));
              }}
            >
              <RefreshCw />
              {t('playground.refreshModels')}
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="rounded-full"
              disabled={streaming || msgs.length === 0}
              onClick={clear}
            >
              <Eraser />
              {t('playground.clearChat')}
            </Button>
          </>
        }
      />

      {!isAdmin ? (
        <div className="flex items-start gap-2.5 rounded-[20px] border border-amber-500/30 bg-amber-500/10 p-4 text-xs">
          <Info className="mt-0.5 h-4 w-4 shrink-0 text-amber-500" />
          <div className="space-y-1">
            <div className="font-medium">{t('playground.adminRequired')}</div>
            <div className="text-muted-foreground">
              {t('playground.adminRequiredDesc')}
            </div>
          </div>
        </div>
      ) : (
        <>
          {/* 协议选择器：同一对话对照三种网关协议。样式对齐工具条的
              outline 胶囊（rounded-full + bg-muted 选中态），深浅色都走
              主题 token，不写死颜色。 */}
          <div className="flex shrink-0 flex-wrap items-center gap-1.5">
            <span className="inline-flex items-center gap-1.5 text-[11px] font-medium text-muted-foreground">
              <Route className="h-3.5 w-3.5" />
              {t('playground.protocolLabel')}
            </span>
            <div className="flex items-center gap-1 rounded-full border border-border/60 p-0.5">
              {PROTOCOLS.map((p) => {
                const Icon = PROTOCOL_ICONS[p];
                const active = protocol === p;
                return (
                  <button
                    key={p}
                    type="button"
                    title={t('playground.protocolHint', {protocol: t(`playground.protocol_${p}`)})}
                    onClick={() => switchProtocol(p)}
                    className={cn(
                      'inline-flex items-center gap-1.5 rounded-full px-3 py-1 text-[11px] font-medium transition-colors',
                      active
                        ? 'bg-foreground text-background'
                        : 'text-muted-foreground hover:bg-muted hover:text-foreground',
                    )}
                  >
                    <Icon className="h-3 w-3" />
                    {t(`playground.protocol_${p}`)}
                  </button>
                );
              })}
            </div>
            {protocol !== 'chat' && (
              <span className="text-[10px] text-muted-foreground/70">
                {t('playground.protocolEndpoint', {
                  endpoint: protocol === 'anthropic' ? 'POST /v1/messages' : 'POST /v1/responses',
                })}
              </span>
            )}
          </div>

          {/* 对话区。不加卡片底色：消息直接落在页面背景上更清爽，
              也让输入框成为视觉焦点（原先整块 bg-muted 显得很重）。 */}
          <section className="flex min-h-[240px] min-w-0 flex-1 flex-col overflow-hidden">
            <div ref={scrollerRef} onScroll={onScroll} className="scroll-slim min-h-0 flex-1 overflow-y-auto px-1 py-4">
              {msgs.length === 0 ? (
                <div className="flex h-full items-center justify-center">
                  <div className="max-w-md space-y-2 text-center">
                    <Bot className="mx-auto h-7 w-7 text-muted-foreground/60" />
                    <div className="text-sm font-medium">{t('playground.startTitle')}</div>
                    <p className="text-[11px] leading-5 text-muted-foreground">
                      {t('playground.startDesc')}
                    </p>
                    {currentModel && (
                      <p className="text-[10px] text-muted-foreground/70">
                        {t('playground.currentModel')}{' '}
                        <span className="font-mono">{currentModel.id}</span>
                        {currentModel.efforts.length > 0 &&
                          t('playground.supportsEfforts', {efforts: currentModel.efforts.join(' / ')})}
                      </p>
                    )}
                  </div>
                </div>
              ) : (
                <div className="mx-auto w-full max-w-3xl space-y-4">
                  {msgs.map((m, i) => (
                    <div
                      key={i}
                      className={cn('flex gap-2.5', m.role === 'user' ? 'justify-end' : 'justify-start')}
                    >
                      {m.role === 'assistant' && (
                        <div className="mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-muted">
                          <Bot className="h-3.5 w-3.5 text-muted-foreground" />
                        </div>
                      )}
                      <div className={cn('min-w-0 max-w-[82%]', m.role === 'user' && 'flex flex-col items-end')}>
                        <div
                          className={cn(
                            'whitespace-pre-wrap break-words rounded-2xl px-3.5 py-2 text-xs leading-5',
                            m.role === 'user'
                              ? 'bg-foreground text-background'
                              : m.error
                                ? 'border border-red-500/30 bg-red-500/10 text-red-600 dark:text-red-400'
                                // 助手气泡用 muted 底：对话区去掉灰色卡片后，
                                // 若仍是 bg-background 就会与页面同色而「隐形」
                                : 'bg-muted',
                          )}
                        >
                          {/* 思考折叠区：Anthropic thinking_delta / Responses
                              reasoning_summary_text.delta 的实时流。chat 协议
                              不产生思考内容（reasoning_content 网关本就不回），
                              因此该区块只在两种新协议下出现。 */}
                          {m.role === 'assistant' && m.thinking && m.thinking.content && (
                            <div className="mb-2 rounded-xl bg-background/60">
                              <button
                                type="button"
                                onClick={() =>
                                  setMsgs((cur) => {
                                    const copy = [...cur];
                                    const th = copy[i]?.thinking;
                                    if (th) {
                                      copy[i] = {...copy[i], thinking: {...th, collapsed: !th.collapsed}};
                                    }
                                    return copy;
                                  })
                                }
                                className="flex w-full items-center gap-1.5 px-2.5 py-1.5 text-left text-[10px] font-medium text-muted-foreground transition-colors hover:text-foreground"
                              >
                                <Brain className="h-3 w-3 shrink-0" />
                                {m.thinking.collapsed
                                  ? t('playground.thinkingCollapsed')
                                  : t('playground.thinkingExpanded')}
                                <ChevronDown
                                  className={cn(
                                    'h-3 w-3 shrink-0 transition-transform',
                                    !m.thinking.collapsed && 'rotate-180',
                                  )}
                                />
                              </button>
                              {!m.thinking.collapsed && (
                                <div className="scroll-slim max-h-48 overflow-y-auto whitespace-pre-wrap break-words px-2.5 pb-2 text-[10px] leading-4 text-muted-foreground">
                                  {m.thinking.content}
                                </div>
                              )}
                            </div>
                          )}
                          {m.content || <Loader2 className="h-3.5 w-3.5 animate-spin" />}
                          {m.role === 'assistant' && !m.done && m.content && (
                            <span className="ml-0.5 inline-block h-3 w-1.5 animate-pulse bg-foreground/50 align-middle" />
                          )}
                        </div>

                        {/* 回答下方：消耗与反馈（对齐参考图里的操作条） */}
                        {m.role === 'assistant' && m.done && !m.error && (
                          <div className="mt-1.5 flex items-center gap-2.5 px-1 text-muted-foreground">
                            {m.protocol && m.protocol !== 'chat' && (
                              <span
                                className="text-[10px] font-medium text-muted-foreground/70"
                                title={t('playground.protocolBadgeTitle')}
                              >
                                {t(`playground.protocol_${m.protocol}`)}
                              </span>
                            )}
                            <CopyButton
                              value={m.content}
                              label=""
                              title={t('playground.copyAnswer')}
                              className="h-6 w-6"
                            />
                            <button
                              type="button"
                              title={t('playground.goodAnswer')}
                              className="transition-colors hover:text-foreground"
                              onClick={() => notify.ok(t('playground.feedbackRecorded'))}
                            >
                              <ThumbsUp className="h-3 w-3" />
                            </button>
                            <button
                              type="button"
                              title={t('playground.badAnswer')}
                              className="transition-colors hover:text-foreground"
                              onClick={() =>
                                notify.info(t('playground.feedbackRecorded'), t('playground.feedbackDetail'))
                              }
                            >
                              <ThumbsDown className="h-3 w-3" />
                            </button>
                            {typeof m.credit === 'number' && (
                              <span
                                className="inline-flex items-center gap-1 text-[10px] tabular-nums"
                                title={t('playground.creditTitle')}
                              >
                                <Coins className="h-3 w-3" />
                                {fmtCredit(m.credit)}
                              </span>
                            )}
                            {!!m.tokens && (
                              <span className="text-[10px] tabular-nums text-muted-foreground/70">
                                {m.tokens} tokens
                              </span>
                            )}
                          </div>
                        )}
                      </div>
                      {m.role === 'user' && (
                        <div className="mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-muted">
                          <UserIcon className="h-3.5 w-3.5 text-muted-foreground" />
                        </div>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>

            {/* 输入区 + 右下角实时累计消耗。不再画顶部分隔线：对话区已无卡片底色，
                一条横贯的线会显得多余。 */}
            <div className="shrink-0 px-3 pb-3 pt-2.5">
              <div className="mx-auto w-full max-w-3xl">
                <AiChatInput
                  value={input}
                  onChange={setInput}
                  onSend={send}
                  onStop={stop}
                  streaming={streaming}
                  disabled={!isAdmin}
                  models={models}
                  model={model}
                  onModelChange={setModel}
                  effort={effort}
                  onEffortChange={setEffort}
                />
                <div className="mt-1.5 flex items-center justify-between gap-2 px-1">
                  <span className="truncate text-[11px] text-muted-foreground">
                    {models.length > 0
                      ? t('playground.modelsAvailable', {count: models.length, n: models.length})
                      : t('playground.noModels')}
                  </span>
                  {/* 实时消耗：本次会话累计。仅在本会话真的收到过
                      usage.credit（Chat 协议）时显示——两种新协议只有
                      token 数，渲染徽章会拿初始 0 冒充「消耗 0 积分」。 */}
                  {sessionHasCredit && (
                    <Badge
                      variant="secondary"
                      className="shrink-0 rounded-full text-[10px] tabular-nums"
                      title={t('playground.sessionCreditTitle')}
                    >
                      <Coins className="mr-1 h-3 w-3" />
                      {t('playground.sessionCredit', {v: fmtCredit(sessionCredit)})}
                    </Badge>
                  )}
                </div>
              </div>
            </div>
          </section>

          {/*
            说明文字：居中的一段小字，限制宽度让它自然折行。
            不做成通栏宽度的卡片——那样在宽屏下就是「一条横」，
            既占地方又不好读；居中窄栏反而更像一句注脚。
            字号 11px / 行高 20px：中文在这个组合下不挤，也不至于淡到看不清。
          */}
          <p className="mx-auto max-w-lg shrink-0 text-center text-[11px] leading-5 text-muted-foreground">
            {t('playground.footnote1')}
            <span className="mx-1.5 text-muted-foreground/40">·</span>
            {t('playground.footnote2')}
            <span className="mx-1.5 text-muted-foreground/40">·</span>
            {t('playground.footnote3')}
          </p>
        </>
      )}
    </div>
  );
}
