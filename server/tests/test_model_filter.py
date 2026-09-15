"""模型清单的「非对话模型」过滤与新字段透出。

背景：上游 2026-09-14 起在它自己的模型解析里加了 `nonChatModel` 过滤——把
嵌入 / 补全 / 代码专用（`nes-` / `completion-` / `codewise-` 前缀）、
输出上限过小（`maxOutputTokens <= 256`）、以及图片生成（tags 含
`text-to-image`）这三类从可选列表剔除，理由是「选了会报 code=11102」。

管理端**直连腾讯**同一接口（比上游多拿显示名与推理档位），所以上游的过滤
不会自动惠及我们——必须自己同步，否则模型中心会列出选不了的东西，
用户点进去必然失败一次。

同时上游新增解析两个字段并透出到 /v1/models：
  * `reasoning.defaultEffort` —— thinking 决策用；空 = 未声明（上游回退硬编码）
  * `supportsImages` —— 多模态能力
"""
from __future__ import annotations

import asyncio
import json
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server import config  # noqa: E402
from server.services import modelcatalog, tencent  # noqa: E402


class _Resp:
    def __init__(self, payload) -> None:
        self._p = payload
        self.status_code = 200

    def json(self):
        return self._p


class _Client:
    """返回预置的模型接口响应。"""

    payload: dict = {}

    def __init__(self, *a, **k) -> None:
        pass

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    async def get(self, url, **kw):
        return _Resp(_Client.payload)

    async def aclose(self):
        return None


def _models_payload(models: list[dict], cli_ids: list[str] | None = None) -> dict:
    return {
        'code': 0,
        'data': {
            'models': models,
            'agents': [{'name': 'cli', 'models': cli_ids or [m['id'] for m in models]}],
        },
    }


class NonChatFilterTest(unittest.TestCase):
    """非对话模型不得进入可选清单（对齐上游 nonChatModel）。"""

    AUTH = {'access_token': 'T', 'realm': 'cn', 'uid': 'u'}

    def setUp(self) -> None:
        p = mock.patch.object(config, 'http_client', _Client)
        p.start()
        self.addCleanup(p.stop)

    def _fetch(self, models, cli_ids=None):
        _Client.payload = _models_payload(models, cli_ids)
        return asyncio.run(tencent.fetch_models(self.AUTH))

    def test_embedding_prefix_filtered(self) -> None:
        ok, out = self._fetch([
            {'id': 'glm-5.2', 'maxInputTokens': 131072, 'maxOutputTokens': 32768},
            {'id': 'nes-embedding-3', 'maxInputTokens': 8192, 'maxOutputTokens': 4096},
        ])
        self.assertTrue(ok, out)
        ids = [m['id'] for m in out]
        self.assertIn('glm-5.2', ids)
        self.assertNotIn('nes-embedding-3', ids, 'nes- 前缀是嵌入模型，选了报 11102')

    def test_completion_and_codewise_filtered(self) -> None:
        ok, out = self._fetch([
            {'id': 'glm-5.2', 'maxInputTokens': 131072, 'maxOutputTokens': 32768},
            {'id': 'completion-basic', 'maxInputTokens': 4096, 'maxOutputTokens': 2048},
            {'id': 'codewise-7b', 'maxInputTokens': 4096, 'maxOutputTokens': 2048},
        ])
        ids = [m['id'] for m in out]
        self.assertEqual(ids, ['glm-5.2'], f'补全/代码模型应被过滤，实际 {ids}')

    def test_tiny_output_filtered(self) -> None:
        """输出上限 ≤256 视为 tiny 非对话模型（上游同此判定）。"""
        ok, out = self._fetch([
            {'id': 'glm-5.2', 'maxInputTokens': 131072, 'maxOutputTokens': 32768},
            {'id': 'tiny-model', 'maxInputTokens': 4096, 'maxOutputTokens': 256},
            {'id': 'tiny-model-2', 'maxInputTokens': 4096, 'maxOutputTokens': 128},
        ])
        ids = [m['id'] for m in out]
        self.assertEqual(ids, ['glm-5.2'], f'输出过小的应被过滤，实际 {ids}')

    def test_text_to_image_filtered(self) -> None:
        ok, out = self._fetch([
            {'id': 'glm-5.2', 'maxInputTokens': 131072, 'maxOutputTokens': 32768},
            {'id': 'img-gen', 'maxInputTokens': 4096, 'maxOutputTokens': 4096,
             'tags': ['text-to-image']},
        ])
        ids = [m['id'] for m in out]
        self.assertEqual(ids, ['glm-5.2'], f'图片生成模型应被过滤，实际 {ids}')

    def test_boundary_values_kept(self) -> None:
        """边界：257 输出不算 tiny（上游是 <= 256）；无关 tag 不过滤。"""
        ok, out = self._fetch([
            {'id': 'ok-257', 'maxInputTokens': 8192, 'maxOutputTokens': 257},
            {'id': 'ok-tag', 'maxInputTokens': 8192, 'maxOutputTokens': 4096,
             'tags': ['vision', 'chat']},
        ])
        ids = sorted(m['id'] for m in out)
        self.assertEqual(ids, ['ok-257', 'ok-tag'])

    def test_internal_marker_not_leaked(self) -> None:
        """`_non_chat` 是内部标记，不能出现在返回给前端的字段里。"""
        ok, out = self._fetch([
            {'id': 'glm-5.2', 'maxInputTokens': 131072, 'maxOutputTokens': 32768},
        ])
        for m in out:
            self.assertNotIn('_non_chat', m, '内部标记漏到响应里了')

    def test_all_filtered_reports_failure(self) -> None:
        """全被过滤时不能返回空清单装作成功。"""
        ok, out = self._fetch([
            {'id': 'nes-embed', 'maxInputTokens': 8192, 'maxOutputTokens': 4096},
        ])
        self.assertFalse(ok)
        self.assertIn('未返回任何可用模型', str(out))

    def test_new_fields_extracted(self) -> None:
        """defaultEffort / supportsImages 要解析出来（上游新增）。"""
        ok, out = self._fetch([
            {'id': 'glm-5.2', 'maxInputTokens': 131072, 'maxOutputTokens': 32768,
             'supportsImages': True,
             'reasoning': {'supportedEfforts': ['low', 'high'],
                           'defaultEffort': 'high'}},
            {'id': 'plain', 'maxInputTokens': 8192, 'maxOutputTokens': 4096},
        ])
        by_id = {m['id']: m for m in out}
        self.assertEqual(by_id['glm-5.2']['default_effort'], 'high')
        self.assertTrue(by_id['glm-5.2']['supports_images'])
        # 未声明时是空值，不能瞎猜一个默认
        self.assertEqual(by_id['plain']['default_effort'], '')
        self.assertFalse(by_id['plain']['supports_images'])


class CatalogFieldPassthroughTest(unittest.TestCase):
    """模型目录要把新字段带到前端（否则 UI 拿不到）。"""

    def test_decorate_keeps_new_fields(self) -> None:
        out = modelcatalog._decorate([
            {'id': 'glm-5.2', 'name': 'GLM', 'context_length': 131072,
             'max_output_tokens': 32768, 'efforts': ['high'],
             'default_effort': 'high', 'supports_images': True},
        ])
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]['default_effort'], 'high')
        self.assertTrue(out[0]['supports_images'])

    def test_decorate_defaults_for_upstream_fallback(self) -> None:
        """回退来源（上游 /v1/models）没有这些字段 → 给安全默认值。"""
        out = modelcatalog._decorate([{'id': 'x', 'context_length': 100}])
        self.assertEqual(out[0]['default_effort'], '')
        self.assertFalse(out[0]['supports_images'])


if __name__ == '__main__':
    unittest.main()


class EffortFallbackTest(unittest.TestCase):
    """推理档位的三级解析（镜像上游 EffortListing）。

    用户报的 issue #8：模型中心的推理栏对 deepseek-v4.1-flash 显示「不支持」，
    而上游已支持三档强度。根因是**只依赖远端 supportedEfforts**，而腾讯接口
    对部分模型不返回该字段。

    上游 2026-09-15（PR #92）的解法是三级：远端权威 → 产品级静态兜底表 →
    都没有则省略。我们镜像同一套（_EFFORT_FALLBACK 逐条照抄其 effort_catalog.go）。

    另外上游把字段透出为 `reasoning_supported_efforts`（此前 /v1/models 里
    根本没有档位字段）——回退路径必须按新字段名读，否则永远拿不到。
    """

    def test_issue8_model_gets_cn_efforts(self) -> None:
        """issue #8 的那个模型：远端没给档位时，国内版应补上三档。"""
        out = modelcatalog._decorate(
            [{'id': 'deepseek-v4.1-flash', 'efforts': [], 'default_effort': ''}], 'cn')
        self.assertEqual(out[0]['efforts'], ['low', 'high', 'max'])
        self.assertEqual(out[0]['default_effort'], 'high')

    def test_realm_tables_are_not_mixed(self) -> None:
        """同一模型在两个版本的档位不同，不能混用（上游注释明确警告过）。"""
        cn = modelcatalog._decorate([{'id': 'deepseek-v4.1-flash', 'efforts': []}], 'cn')[0]
        gl = modelcatalog._decorate([{'id': 'deepseek-v4.1-flash', 'efforts': []}], 'global')[0]
        self.assertEqual(cn['efforts'], ['low', 'high', 'max'])
        self.assertEqual(gl['efforts'], ['high'], '国际版档位被国内版覆盖了')

    def test_remote_wins_over_fallback(self) -> None:
        """远端给了档位就是权威 —— 兜底表不覆盖。"""
        out = modelcatalog._decorate(
            [{'id': 'deepseek-v4.1-flash', 'efforts': ['low'], 'default_effort': 'low'}], 'cn')
        self.assertEqual(out[0]['efforts'], ['low'])
        self.assertEqual(out[0]['default_effort'], 'low')

    def test_unknown_model_gets_nothing(self) -> None:
        """两边都没有 → 空数组，不编造。"""
        out = modelcatalog._decorate([{'id': 'totally-unknown', 'efforts': []}], 'cn')
        self.assertEqual(out[0]['efforts'], [])
        self.assertEqual(out[0]['default_effort'], '')

    def test_default_must_be_in_efforts(self) -> None:
        """默认档不在支持列表内 → 清空（镜像上游 containsEffort 校验）。"""
        out = modelcatalog._decorate(
            [{'id': 'x', 'efforts': ['low'], 'default_effort': 'max'}], 'cn')
        self.assertEqual(out[0]['default_effort'], '')

    def test_upstream_new_field_names_mapped(self) -> None:
        """上游 /v1/models 的 reasoning_supported_efforts 要能被识别。"""
        mapped = modelcatalog._map_upstream_model_fields({
            'id': 'cn:deepseek-v4.1-flash',
            'reasoning_supported_efforts': ['low', 'high', 'max'],
            'reasoning_default_effort': 'high',
            'supports_images': True,
        })
        self.assertEqual(mapped['efforts'], ['low', 'high', 'max'])
        self.assertEqual(mapped['default_effort'], 'high')
        d = modelcatalog._decorate([mapped], 'cn')[0]
        self.assertEqual(d['efforts'], ['low', 'high', 'max'])
        self.assertTrue(d['supports_images'])

    def test_fallback_table_matches_upstream_shape(self) -> None:
        """兜底表的基本形态：两个版本都在、档位非空、默认档合法。"""
        for realm in ('cn', 'global'):
            table = modelcatalog._EFFORT_FALLBACK.get(realm)
            self.assertTrue(table, f'{realm} 兜底表缺失')
            for mid, cap in table.items():
                self.assertTrue(cap['efforts'], f'{realm}/{mid} 档位为空')
                d = cap.get('default')
                if d:
                    self.assertIn(d, cap['efforts'],
                                  f'{realm}/{mid} 的默认档 {d!r} 不在支持列表内')


class UpstreamFallbackPathTest(unittest.TestCase):
    """回退路径（读上游 /v1/models）必须走字段映射。

    上游 2026-09-15 起才在 /v1/models 里透出档位，字段名是
    `reasoning_supported_efforts` —— 我们的内部名字是 `efforts`。
    若回退路径忘了映射，上游明明给了档位我们也读不到
    （这一点是被反证试出来的：直接测 _map_upstream_model_fields 覆盖不到
    「调用方是否真的用了它」）。
    """

    AUTH = {'file': 'a.json', 'uid': '1', 'nickname': '甲', 'realm': 'cn',
            'is_expired': False, 'remain_seconds': 1000}

    def setUp(self) -> None:
        modelcatalog.invalidate()
        for pat in (
            mock.patch.object(modelcatalog.wb2api, 'list_auth_accounts',
                              return_value=[self.AUTH]),
            mock.patch.object(modelcatalog, '_load_token', return_value='tok'),
        ):
            p = pat.start()
            self.addCleanup(p.stop)

    def tearDown(self) -> None:
        modelcatalog.invalidate()

    def test_fallback_path_reads_new_field_names(self) -> None:
        # 刻意用**兜底表里没有**的模型名：否则「映射生效」与「兜底表兜住了」
        # 会产生相同结果，测不出映射是否真的在工作。
        # （第一版用的是 deepseek-v4.1-flash——它在兜底表里，去掉映射后测试
        #  仍然全绿；这是反证时发现的。）
        mid = 'brand-new-model-not-in-fallback-table'
        self.assertNotIn(mid, modelcatalog._EFFORT_FALLBACK['cn'],
                         '这个模型不能出现在兜底表里，否则测不出字段映射')

        async def tencent_fail(auth):
            return False, 'token 过期'

        async def upstream():
            # 上游透出的形态：带 cn: 前缀 + reasoning_supported_efforts
            return True, [{'id': f'cn:{mid}',
                           'context_length': 131072, 'max_output_tokens': 8192,
                           'reasoning_supported_efforts': ['low', 'high', 'max'],
                           'reasoning_default_effort': 'high',
                           'supports_images': True}]

        with mock.patch.object(modelcatalog.tencent, 'fetch_models', tencent_fail), \
                mock.patch.object(modelcatalog.wb2api, 'get_models', upstream):
            out = asyncio.run(modelcatalog.catalog('cn', force=True))

        self.assertEqual(out['source'], 'upstream')
        m = out['models'][0]
        self.assertEqual(m['id'], mid, '前缀应被剥掉')
        self.assertEqual(m['efforts'], ['low', 'high', 'max'],
                         '回退路径没映射新字段名 —— 上游给了档位也读不到')
        self.assertEqual(m['default_effort'], 'high')
        self.assertTrue(m['supports_images'])


class ModelCatalogFullFieldsTest(unittest.TestCase):
    """模型目录的完整字段解析（上游 2026-09-15 补齐）。

    背景：上游这次把 /v1/models 的字段大幅补齐（name / description / credits
    / tags / vendor / 能力标志），并顺手给出了它从腾讯接口解析这些字段时用的
    JSON 名（descriptionZh / credits / tags / vendor …）。我们直连腾讯，本就
    能取到这些字段——只是此前没解析。

    其中最有价值的是 **credits（积分倍率）**：同一 prompt 在不同模型上的扣费
    倍率不同，用户挑「省积分」的模型时靠它。此前界面上完全看不到。

    字段名照上游的实测解析结果，不是猜的（详见 tencent.fetch_models 的注释）。
    """

    def test_tencent_fields_parsed(self) -> None:
        payload = {
            'code': 0,
            'data': {
                'models': [{
                    'id': 'glm-5.2',
                    'name': 'GLM-5.2',
                    'descriptionZh': '通用对话模型',
                    'credits': 'x0.05',
                    'tags': ['badge:限时免费'],
                    'vendor': 'zhipu',
                    'isDefault': True,
                    'maxInputTokens': 131072,
                    'maxOutputTokens': 32768,
                    'supportsImages': True,
                    'supportsReasoning': True,
                    'supportsToolCall': True,
                    'onlyReasoning': False,
                    'reasoning': {'supportedEfforts': ['high'], 'defaultEffort': 'high',
                                  'summary': 'auto'},
                }],
                'agents': [{'name': 'cli', 'models': ['glm-5.2']}],
            },
        }

        class _Client:
            def __init__(self, *a, **k): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *exc): return False
            async def get(self, url, **kw):
                class R:
                    status_code = 200
                    def json(self): return payload
                return R()

        with mock.patch.object(config, 'http_client', _Client):
            ok, out = asyncio.run(tencent.fetch_models(
                {'access_token': 'T', 'realm': 'cn', 'uid': 'u'}))
        self.assertTrue(ok, out)
        m = out[0]
        self.assertEqual(m['description'], '通用对话模型')
        self.assertEqual(m['credits'], 'x0.05', '积分倍率没解析出来 —— 用户挑不了省积分的模型')
        self.assertEqual(m['vendor'], 'zhipu')
        self.assertEqual(m['tags'], ['badge:限时免费'])
        self.assertTrue(m['is_default'])
        self.assertTrue(m['supports_reasoning'])
        self.assertTrue(m['supports_tool_call'])
        self.assertFalse(m['only_reasoning'])
        self.assertEqual(m['reasoning_summary'], 'auto')

    def test_catalog_decorate_passes_fields_through(self) -> None:
        out = modelcatalog._decorate([{
            'id': 'glm-5.2', 'efforts': ['high'], 'credits': 'x0.05',
            'description': '通用对话模型', 'vendor': 'zhipu', 'tags': ['t'],
            'is_default': True, 'supports_reasoning': True,
            'supports_tool_call': True, 'only_reasoning': False,
            'reasoning_summary': 'auto',
        }], 'cn')[0]
        for key, want in (('credits', 'x0.05'), ('description', '通用对话模型'),
                          ('vendor', 'zhipu'), ('tags', ['t']),
                          ('is_default', True), ('supports_reasoning', True),
                          ('supports_tool_call', True), ('reasoning_summary', 'auto')):
            self.assertEqual(out[key], want, f'{key} 没透传到目录')
        self.assertIn('only_reasoning', out)

    def test_missing_fields_get_safe_defaults(self) -> None:
        """上游没给这些字段时要有安全默认（不能 KeyError、不能编造）。"""
        out = modelcatalog._decorate([{'id': 'x', 'efforts': []}], 'cn')[0]
        self.assertEqual(out['credits'], '')
        self.assertEqual(out['description'], '')
        self.assertEqual(out['tags'], [])
        self.assertFalse(out['is_default'])
        self.assertFalse(out['supports_images'])

    def test_upstream_fallback_maps_same_named_fields(self) -> None:
        """回退路径（读上游 /v1/models）的同名字段要能直接透传。

        上游这次透出的 name/description/credits/tags/vendor 与我们内部**同名**，
        不需要映射；只有推理档位名不同（reasoning_supported_efforts）。
        """
        mapped = modelcatalog._map_upstream_model_fields({
            'id': 'cn:glm-5.2', 'name': 'GLM-5.2', 'credits': 'x0.05',
            'description': '[x0.05 credit] 通用对话模型', 'vendor': 'zhipu',
            'tags': ['t'], 'is_default': True, 'supports_tool_call': True,
            'reasoning_supported_efforts': ['high'], 'reasoning_default_effort': 'high',
        })
        d = modelcatalog._decorate([mapped], 'cn')[0]
        self.assertEqual(d['credits'], 'x0.05')
        self.assertEqual(d['vendor'], 'zhipu')
        self.assertTrue(d['is_default'])
        self.assertTrue(d['supports_tool_call'])
        self.assertEqual(d['efforts'], ['high'], '档位字段名映射失效')
