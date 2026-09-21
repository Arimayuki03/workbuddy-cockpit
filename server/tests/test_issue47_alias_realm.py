"""版本归属必须判**映射后**的模型名（issue #47）。

## 问题

鉴权跑在模型映射**之前**，于是版本检查拿到的是客户端发来的别名。别名不带
`global:` 前缀 → 被判成国内版 → 限定国际版的密钥拿着别名请求被 400 打回，
而映射根本没机会生效：

    model_map 里配 claude-fable-5 → global:deepseek-v4.1-flash，
    请求 claude-fable-5 却报「该密钥仅限国际版模型，当前请求是国内版模型」

## 修法的边界（这里钉住的就是这条边界）

两处判据的**对象不同**，不能一起挪：

  · **版本归属判映射后的名字** —— 决定走哪个账号池的是它（同 `db.realm_of_model`，
    日志与统计也按它归档）；
  · **模型白名单判请求名** —— 白名单约束的是「客户端能发哪些名字」，而
    `/v1/models` 的裁剪就是这么算的：别名条目以**别名**为 id 下发（issue #46）。
    两处一起挪，会让「白名单里写别名」的密钥反而被拒，与列表自相矛盾。

## 为什么还要一条结构守卫

四个调用点（网关对话、网关映射、Responses、Anthropic 两个端点）都要「先映射再
鉴权」。`_authorize` 默认会自己算映射，所以漏传不会出错；但如果将来有人把那个
默认值删掉（觉得没人用），漏传的调用点就会**静默退回旧行为**。所以额外钉一条
源码结构检查：任何按模型名鉴权的调用点都必须显式传入映射。
"""
from __future__ import annotations

import re
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server import db, keysvc  # noqa: E402
from server.routers import gateway as G  # noqa: E402

ROOT = Path(__file__).resolve().parents[2]
ALIAS = 'claude-fable-5'
TARGET = 'global:deepseek-v4.1-flash'
MAP = {ALIAS: TARGET}


def _key(**kw) -> dict:
    """一把只让「版本/白名单」参与判定的密钥（其它校验一律满足）。"""
    base = {'enabled': True, 'expires_at': 0, 'quota': 0, 'quota_credit': 0,
            'used_tokens': 0, 'used_credit': 0, 'ip_allowlist': [], 'max_ips': 0,
            'id': 1, 'realm': '', 'models': []}
    base.update(kw)
    return base


class AliasRealmTest(unittest.TestCase):
    """报告者的场景与它的边界。"""

    def test_reporter_case_passes_with_mapping(self) -> None:
        """限定国际版 + 别名指向国际版模型 → 放行（修复前是 400）。"""
        key = _key(realm='global')
        reason = keysvc.validate(key, '1.2.3.4', ALIAS, mapped_model=TARGET)
        self.assertIsNone(reason, f'别名仍被版本检查打回：{reason}')

    def test_without_mapping_it_would_still_reject(self) -> None:
        """反证：不传映射时**必须**还是拒——证明确实是「传映射」修好了它，
        而不是这条检查被谁悄悄放宽了。"""
        reason = keysvc.validate(_key(realm='global'), '1.2.3.4', ALIAS)
        self.assertIsNotNone(reason)
        self.assertEqual(getattr(reason, 'code', ''), 'realm_mismatch')

    def test_cn_key_with_global_alias_rejected(self) -> None:
        """限定国内版 + 别名指向国际版 → 仍要拒（映射不能成为绕过版本的通道）。"""
        reason = keysvc.validate(_key(realm='cn'), '1.2.3.4', ALIAS, mapped_model=TARGET)
        self.assertIsNotNone(reason)
        self.assertEqual(getattr(reason, 'code', ''), 'realm_mismatch')

    def test_reject_message_names_the_mapping(self) -> None:
        """拒的时候要指出是**映射**决定的版本。

        此时「模型名需带 global: 前缀」是误导：客户端发的是别名，加前缀它就不是
        别名了，真正该改的是映射或密钥的版本。
        """
        reason = keysvc.validate(_key(realm='cn'), '1.2.3.4', ALIAS, mapped_model=TARGET)
        self.assertIn(ALIAS, str(reason))
        self.assertIn(TARGET, str(reason))
        self.assertNotIn('需带 global: 前缀', str(reason))

    def test_prefix_hint_kept_without_mapping(self) -> None:
        """没有映射（或映射是恒等）时，提示保持原样——别把正常路径的提示改坏。"""
        reason = keysvc.validate(_key(realm='global'), '1.2.3.4', 'glm-5.2',
                                 mapped_model='glm-5.2')
        self.assertIn('需带 global: 前缀', str(reason))

    def test_whitelist_still_judges_requested_name(self) -> None:
        """白名单判**请求名**：白名单里写别名的密钥，请求别名必须放行。

        这条与 issue #46 的列表裁剪是同一个口径（别名条目以别名为 id 下发）。
        若把白名单也改成判映射后的名字，这里会红——那正是「列表里能选、一选就
        失败」的复现。
        """
        key = _key(realm='global', models=[ALIAS])
        self.assertIsNone(keysvc.validate(key, '1.2.3.4', ALIAS, mapped_model=TARGET))

    def test_whitelist_real_name_does_not_admit_alias(self) -> None:
        """白名单写的是**真名**时，别名不算命中——列表下发的也是真名。

        （客户端该发的是白名单里那个名字；这也是 `/v1/models` 对该密钥给出的
        唯一一条，两者一致。）
        """
        key = _key(realm='global', models=[TARGET])
        reason = keysvc.validate(key, '1.2.3.4', ALIAS, mapped_model=TARGET)
        self.assertIsNotNone(reason)
        self.assertEqual(getattr(reason, 'code', ''), 'model_not_allowed')

    def test_identity_mapping_is_a_no_op(self) -> None:
        """没配映射时 `_map_model` 返回原名，判定与改动前完全一致。"""
        for realm, model, want in (('', 'glm-5.2', None),
                                   ('cn', 'glm-5.2', None),
                                   ('global', 'glm-5.2', 'realm_mismatch'),
                                   ('cn', 'global:gpt-5.6-sol', 'realm_mismatch')):
            with self.subTest(realm=realm, model=model):
                got = keysvc.validate(_key(realm=realm), '1.2.3.4', model,
                                      mapped_model=model)
                self.assertEqual(getattr(got, 'code', None), want)

    def test_missing_model_still_needs_model(self) -> None:
        """缺 model 时照旧拒绝：那会走上游默认模型（国内版），限定国际版的密钥
        不能借此打到国内池。"""
        reason = keysvc.validate(_key(realm='global'), '1.2.3.4', None, mapped_model=None)
        self.assertIsNotNone(reason)
        self.assertIn('必须指定 model', str(reason))


class AuthorizeResolvesMappingTest(unittest.TestCase):
    """`_authorize` 自己会把映射算出来——调用点漏传也不会退回旧行为。"""

    def test_authorize_passes_mapped_model(self) -> None:
        seen: list[dict] = []
        real = keysvc.validate

        def spy(key, ip, model, **kw):
            seen.append({'model': model, **kw})
            return real(key, ip, model, **kw)

        req = mock.Mock()
        with mock.patch.object(db, 'get_setting', lambda k, d=None: MAP if k == 'model_map' else d), \
             mock.patch.object(db, 'query', lambda *a, **k: []), \
             mock.patch.object(G, '_bearer', lambda r: 'wbk_x'), \
             mock.patch.object(keysvc, 'resolve', lambda t: _key()), \
             mock.patch.object(keysvc, 'validate', side_effect=spy), \
             mock.patch.object(G, 'get_security_config', lambda: {'enabled': False}):
            G._authorize(req, ALIAS)

        self.assertEqual(len(seen), 1,
                         f'validate 没被调用到（守卫会空转）：{seen}')
        self.assertEqual(seen[0]['mapped_model'], TARGET,
                         '_authorize 没有把映射传给 validate —— issue #47 会复发')

    def test_mapping_ignored_for_model_list(self) -> None:
        """`/v1/models` 路径没有 model，映射无从谈起，不应报错。"""
        with mock.patch.object(db, 'get_setting', lambda k, d=None: MAP if k == 'model_map' else d):
            self.assertIsNone(G._map_model(None))


class CallSitesMapBeforeAuthorizeTest(unittest.TestCase):
    """结构守卫：按模型名鉴权的调用点必须显式传 `mapped=`。

    见模块说明：`_authorize` 的默认值让漏传仍然正确，这条钉的是「映射先于鉴权」
    这个约定本身——将来谁把默认值删了，漏传的调用点不会静默退回旧行为。
    """

    SITES = ('server/routers/gateway.py', 'server/routers/responses.py',
             'server/routers/anthropic.py')

    def test_every_model_call_passes_mapped(self) -> None:
        offenders: list[str] = []
        for rel in self.SITES:
            src = (ROOT / rel).read_text(encoding='utf-8')
            for m in re.finditer(r'_authorize\(request,[^)]*\)', src):
                call = m.group(0)
                if 'is_model_list=True' in call:
                    continue          # 模型发现类请求：没有 model
                if 'mapped=' not in call:
                    offenders.append(f'{rel}: {call}')
        self.assertEqual(offenders, [],
                         '这些调用点没有把模型映射传给鉴权（版本归属会判错）：'
                         + '; '.join(offenders))

    def test_guard_actually_scans_something(self) -> None:
        """先确认扫描面有效，否则上面那条会在空集合上通过。"""
        total = 0
        for rel in self.SITES:
            src = (ROOT / rel).read_text(encoding='utf-8')
            total += len(re.findall(r'_authorize\(request,', src))
        self.assertGreaterEqual(total, 4, '没扫到调用点，守卫会空转')


if __name__ == '__main__':
    unittest.main()
