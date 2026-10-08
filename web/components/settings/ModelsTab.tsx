'use client';

/**
 * 设置页「模型映射」分区（原 TabsContent value="models"，原样搬移，只拆不重构）：
 * 新增别名 + 别名→目标 映射表。数据与保存动作在 settings-context。
 */
import {Plus, Trash2} from 'lucide-react';
import {useI18n} from '@/lib/i18n/provider';
import {RichText} from '@/lib/i18n/rich-text';
import {useAuth} from '@/lib/auth-context';
import {Button} from '@/components/ui/button';
import {Input} from '@/components/ui/input';
import {Label} from '@/components/ui/label';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import {useSettings} from './settings-context';

export function ModelsTab() {
  const {t} = useI18n();
  const {isAdmin} = useAuth();
  const s = useSettings();

  return (
    <div className="mt-4 space-y-4">
      <div className="rounded-[20px] bg-muted p-4">
        <div className="mb-1 text-sm font-medium">{t('settings.mapNewAlias')}</div>
        <div className="mb-3 text-[11px] text-muted-foreground">
          <RichText text={t('settings.mapNewAliasDesc')} />
          {/* 映射目标列表跟随当前版本域（realm）；切换版本后建议重选目标 */}
          <div>{t('settings.mapRealmNote')}</div>
        </div>
        <div className="grid grid-cols-1 items-end gap-3 sm:grid-cols-4">
          <div className="space-y-1.5">
            <Label className="text-[11px] text-muted-foreground">{t('settings.mapAlias')}</Label>
            <Input value={s.mapAlias} onChange={(e) => s.setMapAlias(e.target.value)} placeholder="gpt-4o-mini" className="bg-background" disabled={!isAdmin} />
          </div>
          <div className="space-y-1.5">
            <Label className="text-[11px] text-muted-foreground">{t('settings.mapTarget')}</Label>
            <Select value={s.mapTarget} onValueChange={s.setMapTarget} disabled={!isAdmin}>
              <SelectTrigger className="bg-background"><SelectValue placeholder={t('chat.selectModel')} /></SelectTrigger>
              <SelectContent>
                {s.models.map((m) => (
                  <SelectItem key={m} value={m}>{m}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="sm:col-span-2">
            <Button
              className="rounded-full"
              disabled={!isAdmin || !s.mapAlias.trim() || !s.mapTarget}
              onClick={() => {
                s.saveModelMap({...s.modelMap, [s.mapAlias.trim()]: s.mapTarget});
                s.setMapAlias('');
                s.setMapTarget('');
              }}
            >
              <Plus />
              {t('settings.mapAdd')}
            </Button>
          </div>
        </div>
      </div>

      <div className="overflow-hidden rounded-[20px] bg-muted">
        <Table>
          <TableHeader>
            <TableRow className="border-b border-border/60 hover:bg-transparent">
              <TableHead className="pl-4 text-[11px] text-muted-foreground">{t('settings.mapAlias')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('settings.mapTarget')}</TableHead>
              {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">{t('accounts.colActions')}</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {Object.entries(s.modelMap).map(([alias, target]) => (
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
                        const next = {...s.modelMap};
                        delete next[alias];
                        s.saveModelMap(next);
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
        {!Object.keys(s.modelMap).length && (
          <div className="py-10 text-center text-xs text-muted-foreground">
            {t('settings.mapEmpty')}
          </div>
        )}
      </div>
    </div>
  );
}
