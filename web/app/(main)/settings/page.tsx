'use client';

import {useCallback, useEffect, useState} from 'react';
import {Settings as SettingsIcon, RefreshCw, Plus, Trash2, Save, Users, Server, Shuffle, Info} from 'lucide-react';
import {toast} from 'sonner';
import {settingsApi, upstreamApi, errText} from '@/lib/api';
import type {ModelInfo, UpstreamConfig, UserItem} from '@/lib/types';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {useAuth} from '@/lib/auth-context';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {Input} from '@/components/ui/input';
import {Label} from '@/components/ui/label';
import {Textarea} from '@/components/ui/textarea';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import {Tabs, TabsContent, TabsList, TabsTrigger} from '@/components/ui/tabs';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';

export default function SettingsPage() {
  const {isAdmin} = useAuth();
  const [cfg, setCfg] = useState<UpstreamConfig | null>(null);
  const [models, setModels] = useState<ModelInfo[]>([]);
  const [schedText, setSchedText] = useState('');
  const [poolText, setPoolText] = useState('');
  const [modelMap, setModelMap] = useState<Record<string, string>>({});
  const [mapAlias, setMapAlias] = useState('');
  const [mapTarget, setMapTarget] = useState('');
  const [users, setUsers] = useState<UserItem[]>([]);
  const [newUser, setNewUser] = useState({username: '', password: '', role: 'viewer'});
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    const [c, m, mm, u] = await Promise.allSettled([
      settingsApi.upstream(),
      upstreamApi.models(),
      settingsApi.modelMap(),
      settingsApi.users(),
    ]);
    if (c.status === 'fulfilled') {
      setCfg(c.value);
      setSchedText(JSON.stringify(c.value.schedule ?? {}, null, 2));
      setPoolText(JSON.stringify(c.value.pool ?? {}, null, 2));
    }
    if (m.status === 'fulfilled') setModels(m.value);
    if (mm.status === 'fulfilled') setModelMap(mm.value);
    if (u.status === 'fulfilled') setUsers(u.value);
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  async function saveSchedule() {
    await saveJson('schedule', schedText);
  }
  async function savePool() {
    await saveJson('pool', poolText);
  }
  async function saveJson(field: 'schedule' | 'pool', text: string) {
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch {
      toast.error('JSON 格式错误');
      return;
    }
    setBusy(true);
    try {
      await settingsApi.saveUpstream({[field]: parsed});
      toast.success('已保存');
      load();
    } catch (e) {
      toast.error(errText(e));
    } finally {
      setBusy(false);
    }
  }

  async function saveModelMap(next: Record<string, string>) {
    try {
      await settingsApi.saveModelMap(next);
      setModelMap(next);
      toast.success('模型映射已保存');
    } catch (e) {
      toast.error(errText(e));
    }
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title="设置"
        description="上游代理配置、模型别名映射与管理端用户"
        actions={
          <Button variant="outline" size="sm" className="rounded-full" onClick={load}>
            <RefreshCw />
            刷新
          </Button>
        }
      />

      <Tabs defaultValue="upstream">
        <TabsList>
          <TabsTrigger value="upstream"><Server className="mr-1.5 h-3.5 w-3.5" />上游配置</TabsTrigger>
          <TabsTrigger value="models"><Shuffle className="mr-1.5 h-3.5 w-3.5" />模型映射</TabsTrigger>
          <TabsTrigger value="users"><Users className="mr-1.5 h-3.5 w-3.5" />管理用户</TabsTrigger>
          <TabsTrigger value="about"><Info className="mr-1.5 h-3.5 w-3.5" />关于</TabsTrigger>
        </TabsList>

        {/* 上游配置 */}
        <TabsContent value="upstream" className="mt-4 space-y-4">
          <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
            <div className="rounded-[20px] bg-muted p-4">
              <div className="mb-3 text-sm font-medium">基础信息</div>
              <div className="space-y-2 text-xs">
                {([
                  ['监听地址', cfg?.listen || '—'],
                  ['API Key', cfg?.api_key_masked || '—'],
                  ['授权目录', cfg?.auth_dir || '—'],
                ] as [string, string][]).map(([k, v]) => (
                  <div key={k} className="flex items-center justify-between gap-3">
                    <span className="text-muted-foreground">{k}</span>
                    <span className="truncate font-mono">{v}</span>
                  </div>
                ))}
              </div>
            </div>
            <div className="rounded-[20px] bg-muted p-4 lg:col-span-2">
              <div className="mb-3 text-sm font-medium">可用模型（来自上游 /v1/models）</div>
              <div className="flex flex-wrap gap-1.5">
                {models.length ? (
                  models.map((m) => (
                    <Badge key={m.id} variant="secondary" className="rounded-full font-mono text-[10px]">
                      {m.id}
                    </Badge>
                  ))
                ) : (
                  <span className="text-xs text-muted-foreground">未获取到模型列表</span>
                )}
              </div>
            </div>
          </div>

          <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
            <div className="rounded-[20px] bg-muted p-4">
              <div className="mb-3 flex items-center justify-between">
                <div className="text-sm font-medium">schedule（签到 / 保活）</div>
                <Button size="sm" variant="outline" className="rounded-full" disabled={!isAdmin || busy} onClick={saveSchedule}>
                  <Save className="h-3.5 w-3.5" />保存
                </Button>
              </div>
              <Textarea
                rows={9}
                spellCheck={false}
                disabled={!isAdmin}
                value={schedText}
                onChange={(e) => setSchedText(e.target.value)}
                className="bg-background font-mono text-xs"
              />
            </div>
            <div className="rounded-[20px] bg-muted p-4">
              <div className="mb-3 flex items-center justify-between">
                <div className="text-sm font-medium">pool（并发 / 熔断）</div>
                <Button size="sm" variant="outline" className="rounded-full" disabled={!isAdmin || busy} onClick={savePool}>
                  <Save className="h-3.5 w-3.5" />保存
                </Button>
              </div>
              <Textarea
                rows={9}
                spellCheck={false}
                disabled={!isAdmin}
                value={poolText}
                onChange={(e) => setPoolText(e.target.value)}
                className="bg-background font-mono text-xs"
              />
            </div>
          </div>
          {!isAdmin && <p className="text-[11px] text-muted-foreground">只读角色无法修改上游配置。</p>}
        </TabsContent>

        {/* 模型映射 */}
        <TabsContent value="models" className="mt-4 space-y-4">
          <div className="rounded-[20px] bg-muted p-4">
            <div className="mb-3 text-sm font-medium">新增别名映射</div>
            <div className="grid grid-cols-1 items-end gap-3 sm:grid-cols-4">
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">对外别名</Label>
                <Input value={mapAlias} onChange={(e) => setMapAlias(e.target.value)} placeholder="gpt-4o-mini" className="bg-background" disabled={!isAdmin} />
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">目标模型</Label>
                <Select value={mapTarget} onValueChange={setMapTarget} disabled={!isAdmin}>
                  <SelectTrigger className="bg-background"><SelectValue placeholder="选择上游模型" /></SelectTrigger>
                  <SelectContent>
                    {models.map((m) => (
                      <SelectItem key={m.id} value={m.id}>{m.id}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="sm:col-span-2">
                <Button
                  className="rounded-full"
                  disabled={!isAdmin || !mapAlias.trim() || !mapTarget}
                  onClick={() => {
                    saveModelMap({...modelMap, [mapAlias.trim()]: mapTarget});
                    setMapAlias('');
                    setMapTarget('');
                  }}
                >
                  <Plus />
                  添加映射
                </Button>
              </div>
            </div>
          </div>

          <div className="overflow-hidden rounded-[20px] bg-muted">
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-4 text-[11px] text-muted-foreground">对外别名</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">目标模型</TableHead>
                  {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">操作</TableHead>}
                </TableRow>
              </TableHeader>
              <TableBody>
                {Object.entries(modelMap).map(([alias, target]) => (
                  <TableRow key={alias} className="border-b border-border/40">
                    <TableCell className="pl-4 font-mono text-xs">{alias}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">{target}</TableCell>
                    {isAdmin && (
                      <TableCell className="pr-4 text-right">
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7 rounded-md text-red-500 hover:text-red-600"
                          onClick={() => {
                            const next = {...modelMap};
                            delete next[alias];
                            saveModelMap(next);
                          }}
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                        </Button>
                      </TableCell>
                    )}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            {!Object.keys(modelMap).length && (
              <div className="py-10 text-center text-xs text-muted-foreground">暂无映射，客户端可直接使用上游模型名</div>
            )}
          </div>
        </TabsContent>

        {/* 管理用户 */}
        <TabsContent value="users" className="mt-4 space-y-4">
          {isAdmin && (
            <div className="rounded-[20px] bg-muted p-4">
              <div className="mb-3 text-sm font-medium">新增管理用户</div>
              <div className="grid grid-cols-1 items-end gap-3 sm:grid-cols-4">
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">用户名</Label>
                  <Input value={newUser.username} onChange={(e) => setNewUser({...newUser, username: e.target.value})} className="bg-background" />
                </div>
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">密码</Label>
                  <Input type="password" value={newUser.password} onChange={(e) => setNewUser({...newUser, password: e.target.value})} className="bg-background" />
                </div>
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">角色</Label>
                  <Select value={newUser.role} onValueChange={(v) => setNewUser({...newUser, role: v})}>
                    <SelectTrigger className="bg-background"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="admin">管理员（可读写）</SelectItem>
                      <SelectItem value="viewer">只读用户</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <Button
                  className="rounded-full"
                  disabled={busy}
                  onClick={async () => {
                    if (!newUser.username.trim() || !newUser.password) {
                      toast.error('请填写用户名与密码');
                      return;
                    }
                    setBusy(true);
                    try {
                      await settingsApi.addUser(newUser);
                      toast.success('用户已创建');
                      setNewUser({username: '', password: '', role: 'viewer'});
                      load();
                    } catch (e) {
                      toast.error(errText(e));
                    } finally {
                      setBusy(false);
                    }
                  }}
                >
                  <Plus />
                  创建
                </Button>
              </div>
            </div>
          )}

          <div className="overflow-hidden rounded-[20px] bg-muted">
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-4 text-[11px] text-muted-foreground">用户名</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">角色</TableHead>
                  {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">操作</TableHead>}
                </TableRow>
              </TableHeader>
              <TableBody>
                {users.map((u) => (
                  <TableRow key={u.username} className="border-b border-border/40">
                    <TableCell className="pl-4 text-sm font-medium">{u.username}</TableCell>
                    <TableCell>
                      <Badge variant="secondary" className="rounded-full">
                        {u.role === 'admin' ? '管理员' : '只读'}
                      </Badge>
                    </TableCell>
                    {isAdmin && (
                      <TableCell className="pr-4 text-right">
                        <div className="flex justify-end gap-1">
                          <Button
                            variant="ghost"
                            size="sm"
                            className="h-7 rounded-full text-xs"
                            onClick={async () => {
                              const pwd = window.prompt(`为「${u.username}」设置新密码：`);
                              if (!pwd) return;
                              try {
                                await settingsApi.updateUser(u.username, {password: pwd});
                                toast.success('密码已更新');
                              } catch (e) {
                                toast.error(errText(e));
                              }
                            }}
                          >
                            重置密码
                          </Button>
                          <ConfirmDialog
                            title={`删除用户「${u.username}」？`}
                            description="删除后该用户将无法登录管理端。"
                            confirmText="删除"
                            destructive
                            onConfirm={async () => {
                              try {
                                await settingsApi.removeUser(u.username);
                                toast.success('已删除');
                                load();
                              } catch (e) {
                                toast.error(errText(e));
                              }
                            }}
                            trigger={
                              <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md text-red-500 hover:text-red-600">
                                <Trash2 className="h-3.5 w-3.5" />
                              </Button>
                            }
                          />
                        </div>
                      </TableCell>
                    )}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            {!users.length && (
              <EmptyState
                icon={Users}
                title="暂无管理用户"
                description="至少保留一个 admin 账号"
                className="flex flex-col items-center justify-center py-12 text-center"
              />
            )}
          </div>
        </TabsContent>

        {/* 关于 */}
        <TabsContent value="about" className="mt-4">
          <div className="rounded-[20px] bg-muted p-5 text-xs leading-6 text-muted-foreground">
            <div className="mb-2 flex items-center gap-2 text-sm font-medium text-foreground">
              <SettingsIcon className="h-4 w-4" />
              WorkBuddy Manager
            </div>
            <p>
              本项目为 <span className="font-mono">workbuddy2api</span>（腾讯 CodeBuddy → OpenAI 兼容代理）提供 Web 管理控制台与对外反代网关。
              底层账号轮询、并发与熔断由 workbuddy2api 负责；本管理端负责账号纳管（扫码添加 / 自动签到 / Token 监控）、密钥分发、
              入站 IP 管控、请求日志与用量统计。
            </p>
            <p className="mt-3">
              UI 视觉与底栏交互参考 <span className="font-mono">linux-do/cdk</span>（MIT），特此致谢。
            </p>
            <p className="mt-3">
              下游接入示例：Base URL <span className="font-mono">https://&lt;你的域名&gt;/v1</span>，Key 使用本面板生成的{' '}
              <span className="font-mono">wbk_...</span>。
            </p>
          </div>
        </TabsContent>
      </Tabs>
    </div>
  );
}
