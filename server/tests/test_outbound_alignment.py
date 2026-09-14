"""出站请求头的端到端核对：管理端直连腾讯 vs 上游参照实现。

为什么要单写这个文件：管理端有一批请求**绕过 workbuddy2api 直连腾讯**
（扫码登录、签到、查积分、地区注册、trial、探测）。上游 Go 侧有一套头构造
（CommonHeaders / ChatHeaders / BillingHeaders），管理端这套必须与之同形 ——
否则这批流量会成为唯一「不像官方客户端」的请求，风控挑出来的代价是封号。

这些缺口**不会报错**：接口照样返回 200，只是形态不对。所以只能靠本文件
把「每个端点该带哪些头」逐条钉死。

上游参照位置（2026-09-14，commit 6cee564c）：
  * internal/upstream/headers.go  CommonHeaders / ChatHeaders / BillingHeaders
  * internal/upstream/trial_test.go  断言 trial 必须带 X-User-Id
  * scripts/global_region.py         地区注册的请求体形状（注释标注「实测」）
"""
from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server import config  # noqa: E402
from server.services import realm, tencent  # noqa: E402


class FakeResp:
    """最小 httpx.Response 替身：把请求原样记下来供断言。"""

    def __init__(self, payload=None, status_code=200) -> None:
        self._payload = payload if payload is not None else {'code': 0, 'data': {}}
        self.status_code = status_code

    def json(self):
        return self._payload

    @property
    def text(self) -> str:
        return json.dumps(self._payload)

    async def aread(self) -> bytes:
        return self.text.encode()

    async def aclose(self) -> None:
        return None


class RecordingClient:
    """记录出站请求（method/url/json/headers）的 httpx.AsyncClient 替身。"""

    calls: list = []

    def __init__(self, *a, **k) -> None:
        pass

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    async def post(self, url, **kw):
        RecordingClient.calls.append(('POST', url, kw))
        return FakeResp()

    async def get(self, url, **kw):
        RecordingClient.calls.append(('GET', url, kw))
        return FakeResp()

    def stream(self, method, url, **kw):
        RecordingClient.calls.append((method, url, kw))
        outer = self

        class _Ctx:
            async def __aenter__(self_inner):
                return FakeResp({'code': 0, 'data': {'choices': []}}, 200)

            async def __aexit__(self_inner, *exc):
                return False

        return _Ctx()

    async def aclose(self) -> None:
        return None


def _run(coro):
    import asyncio
    return asyncio.run(coro)


class _Base(unittest.TestCase):
    def setUp(self) -> None:
        RecordingClient.calls = []
        self._p = mock.patch.object(config, 'http_client', RecordingClient)
        self._p.start()
        self.addCleanup(self._p.stop)
        realm.invalidate()

    def _last(self):
        self.assertTrue(RecordingClient.calls, '没有任何出站请求')
        return RecordingClient.calls[-1]


class BillingHeaderTest(_Base):
    """billing 域（签到/积分/trial/注册）必须带身份头 —— 对齐上游 BillingHeaders。

    上游 Go 测试明确断言 trial 请求携带 X-User-Id（trial_test.go:46），
    签到与查积分同理。此前管理端这条路只发通用头，缺全部身份头。
    """

    AUTH = {
        'access_token': 'TOK',
        'uid': 'u123',
        'enterprise_id': 'ent-9',
        'domain': 'copilot.tencent.com',
        'realm': 'cn',
    }

    def test_checkin_carries_identity_headers(self) -> None:
        code, msg = _run(tencent.checkin(self.AUTH, 'cn'))
        self.assertEqual(code, 0, msg)
        _, url, kw = self._last()
        h = kw['headers']
        self.assertIn('/billing/meter/daily-checkin', url)
        self.assertEqual(h.get('X-User-Id'), 'u123')
        self.assertEqual(h.get('X-Enterprise-Id'), 'ent-9')
        self.assertEqual(h.get('X-Tenant-Id'), 'ent-9', '上游企业账号发两个同值头')
        self.assertEqual(h.get('X-Domain'), 'copilot.tencent.com')
        self.assertEqual(h.get('Authorization'), 'Bearer TOK')
        self.assertEqual(h.get('X-CodeBuddy-Request'), '1', 'D1 风控闸门头')

    def test_fetch_credits_carries_identity_headers(self) -> None:
        _run(tencent.fetch_credits(self.AUTH))
        _, url, kw = self._last()
        h = kw['headers']
        self.assertIn('get-user-resource', url)
        self.assertEqual(h.get('X-User-Id'), 'u123')
        self.assertEqual(h.get('X-Domain'), 'copilot.tencent.com')

    def test_trial_carries_user_id(self) -> None:
        auth = dict(self.AUTH, realm='global', domain='www.workbuddy.ai')
        _run(tencent.claim_trial(auth))
        _, url, kw = self._last()
        self.assertIn('/billing/ide/trial', url)
        self.assertEqual(kw['headers'].get('X-User-Id'), 'u123',
                         '上游 trial_test.go 断言必须携带 X-User-Id')

    def test_identity_headers_absent_when_unknown(self) -> None:
        """字段缺失时不发空头 —— 上游也是「非空才发」。"""
        _run(tencent.checkin({'access_token': 'TOK'}, 'cn'))
        h = self._last()[2]['headers']
        for k in ('X-User-Id', 'X-Enterprise-Id', 'X-Tenant-Id', 'X-Domain'):
            self.assertNotIn(k, h, f'{k} 不该以空值发出')

    def test_accept_language_follows_realm(self) -> None:
        _run(tencent.checkin(self.AUTH, 'cn'))
        self.assertEqual(self._last()[2]['headers'].get('Accept-Language'), 'zh-CN')


class RegionSubmissionTest(_Base):
    """地区注册的请求体形状 —— 照上游参照实现（scripts/global_region.py）。

    上游注释标注「实测」的形状：
        {"attributes": {"countryCode": [Code],
                        "countryFullName": [EnName],
                        "countryName": [IOS2]}}
    三个字段**取值不同源**。此前我们少了 attributes 外层，且把三者都填成 IOS2，
    会把地区归属写歪（接口通常仍返回 200，所以不会自己暴露）。
    """

    AUTH = {'access_token': 'TOK', 'uid': 'u1', 'realm': 'global',
            'domain': 'www.workbuddy.ai'}

    def _country_list(self):
        inner = {'code': 0, 'data': {'list': [
            {'IOS2': 'HK', 'IOS3': 'HKG', 'Code': '810000', 'EnName': 'China Hong Kong'},
            {'IOS2': 'SG', 'IOS3': 'SGP', 'Code': '702000', 'EnName': 'Singapore'},
        ]}}
        # 上游这一层的 data 是**内嵌 JSON 字符串**（脚本里显式 json.loads）
        return {'code': 0, 'data': json.dumps(inner)}

    def test_body_shape_matches_reference(self) -> None:
        with mock.patch.object(tencent, '_region_fields',
                               return_value=('HK', 'China Hong Kong', '810000')):
            ok, msg = _run(tencent.submit_region(self.AUTH, 'HK'))
        self.assertTrue(ok, msg)
        _, url, kw = self._last()
        self.assertIn('/console/login/account', url)
        body = kw['json']
        self.assertIn('attributes', body,
                      '缺 attributes 外层 —— 上游参照实现是 {"attributes": {...}}')
        attrs = body['attributes']
        self.assertEqual(attrs['countryName'], ['HK'])
        self.assertEqual(attrs['countryFullName'], ['China Hong Kong'])
        self.assertEqual(attrs['countryCode'], ['810000'],
                         'countryCode 是数字码，不是 IOS2')

    def test_region_fields_parses_nested_json(self) -> None:
        """地区列表的 data 是内嵌 JSON 字符串，解析要穿透一层。"""
        with mock.patch.object(config, 'http_client',
                               _ListClient(self._country_list())):
            ios2, en, code = _run(tencent._region_fields('HK'))
        self.assertEqual((ios2, en, code), ('HK', 'China Hong Kong', '810000'))

    def test_region_fields_falls_back_without_lying(self) -> None:
        """查不到就退回 IOS2 —— 宁可让上游拒绝，也不瞎猜一个数字码。"""
        with mock.patch.object(config, 'http_client', RecordingClient):
            ios2, en, code = _run(tencent._region_fields('XX'))
        self.assertEqual((ios2, en, code), ('XX', 'XX', 'XX'))


class _ListClient:
    def __init__(self, payload) -> None:
        self._payload = payload

    def __call__(self, *a, **k):
        return self

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False

    async def post(self, url, **kw):
        return FakeResp(self._payload)

    async def aclose(self):
        return None


class DeviceTokenTest(_Base):
    """X-Device-Token 三级回退：auth 每号 > config 全局 > 文件（对齐上游）。"""

    def setUp(self) -> None:
        super().setUp()
        self._tmp = Path(__file__).resolve().parent / '_dt_tmp'
        self._tmp.mkdir(exist_ok=True)
        self.addCleanup(lambda: [p.unlink() for p in self._tmp.glob('*')])

    def _patch_cfg(self, up: dict):
        p = mock.patch.object(realm, '_read_upstream_sec', return_value=up)
        p.start()
        self.addCleanup(p.stop)

    def test_priority_auth_over_config_over_file(self) -> None:
        f = self._tmp / 'dt.txt'
        f.write_text('FROM_FILE', encoding='utf-8')
        self._patch_cfg({'device_token': 'FROM_CONFIG', 'device_token_file': str(f)})
        realm.invalidate()
        self.assertEqual(realm.device_token_for({'device_token': 'PER_ACCOUNT'}), 'PER_ACCOUNT')
        # 每号为空 → 落到 config 全局
        realm.invalidate()
        self.assertEqual(realm.device_token_for({'device_token': ''}), 'FROM_CONFIG')

    def test_file_fallback_and_size_limit(self) -> None:
        f = self._tmp / 'dt2.txt'
        f.write_text('FILE_TOK', encoding='utf-8')
        self._patch_cfg({'device_token': '', 'device_token_file': str(f)})
        realm.invalidate()
        self.assertEqual(realm.device_token_for({}), 'FILE_TOK')
        # 超过 1KB 视为没有（上游 deviceTokenFileMaxLen = 1024）
        big = self._tmp / 'dt_big.txt'
        big.write_text('x' * 2000, encoding='utf-8')
        self._patch_cfg({'device_token': '', 'device_token_file': str(big)})
        realm.invalidate()
        self.assertEqual(realm.device_token_for({}), '')

    def test_missing_everything_is_empty_not_error(self) -> None:
        self._patch_cfg({})
        realm.invalidate()
        self.assertEqual(realm.device_token_for({}), '')
        self.assertEqual(realm.device_token_for(None), '')

    def test_headers_include_device_token_when_present(self) -> None:
        self._patch_cfg({'device_token': 'DT'})
        realm.invalidate()
        h = realm.billing_headers('cn', {'access_token': 't', 'uid': 'u'})
        self.assertEqual(h.get('X-Device-Token'), 'DT')
        # 空时不发头（上游语义：空则不注入）
        self._patch_cfg({})
        realm.invalidate()
        h2 = realm.billing_headers('cn', {'access_token': 't'})
        self.assertNotIn('X-Device-Token', h2)

    def test_device_token_never_logged_or_returned(self) -> None:
        """它是凭据：不出现在任何返回结构里（只进请求头）。"""
        self._patch_cfg({'device_token': 'SECRET_DT'})
        realm.invalidate()
        h = realm.billing_headers('cn', {'access_token': 't', 'uid': 'u'})
        self.assertIn('X-Device-Token', h)
        # realm.headers（通用头，会被日志/调试打印的那套）不得含它
        plain = realm.headers('cn', 't')
        self.assertNotIn('X-Device-Token', plain)
        self.assertNotIn('SECRET_DT', json.dumps(plain))


class WriteAuthFilePreservesDeviceTokenTest(unittest.TestCase):
    """重新登录不能把用户手写的 device_token 冲掉。

    落盘是**整体覆盖**：若不复用旧值，用户配置的设备风控凭据会在下次换 token
    重登时静默消失 —— 之后所有 billing 请求都降级成「没有设备标识」的形态，
    而接口依然 200，没人会发现。
    """

    def setUp(self) -> None:
        import tempfile
        self.dir = Path(tempfile.mkdtemp())
        p = mock.patch.object(config, 'AUTH_DIR', self.dir)
        p.start()
        self.addCleanup(p.stop)

    def test_existing_device_token_survives_relogin(self) -> None:
        target = self.dir / 'workbuddy-u9.json'
        target.write_text(json.dumps({
            'account': {'uid': 'u9'},
            'auth': {'accessToken': 'old'},
            'device_token': 'MY_DEVICE_TOKEN',
        }, ensure_ascii=False), encoding='utf-8')

        tencent.write_auth_file({
            'uid': 'u9', 'access_token': 'new-token', 'refresh_token': 'r',
            'expires_at': 123, 'domain': '', 'realm': 'cn',
            'enterprise_id': '', 'nickname': 'n',
        })
        data = json.loads(target.read_text(encoding='utf-8'))
        self.assertEqual(data.get('device_token'), 'MY_DEVICE_TOKEN',
                         '重登把 device_token 冲掉了 —— 风控形态会静默降级')
        self.assertEqual(data['auth']['accessToken'], 'new-token', 'token 应已更新')

    def test_new_account_has_no_device_token_key(self) -> None:
        """没有旧值时不引入空键（与上游「非空才写」一致）。"""
        tencent.write_auth_file({
            'uid': 'u10', 'access_token': 't', 'refresh_token': 'r',
            'expires_at': 1, 'domain': '', 'realm': 'cn',
            'enterprise_id': '', 'nickname': 'n',
        })
        data = json.loads((self.dir / 'workbuddy-u10.json').read_text(encoding='utf-8'))
        self.assertNotIn('device_token', data)


if __name__ == '__main__':
    unittest.main()
