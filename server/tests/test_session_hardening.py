"""管理端会话加固：总时长上限 + 空闲滑动续期 + 一键吊销 + Secure 告警。

起因是用户报「Token 有效期太长了，不会自动失效和轮换，一年内都免密进后台」。

实测澄清：会话是 **7 天**（不是一年 —— 一年那个数字是 `/_next/static/` 的缓存头
`max-age=31536000`，与会话无关）。但报告指出的缺口是真的：7 天里会话不轮换、
也不随活跃度重置，且**没有主动吊销入口**（`sv` 只在改密码/改角色时递增）。

本版三件事，每条都对应一个具体风险：
  1. 缩短总时长（7 天 → 1 天）：限制凭证的有效期上限；
  2. 空闲滑动续期：常用的人不被打扰、放着不用的会话自己过期
     （只减总时长会惩罚常用者、放过闲置者，正好反了）；
  3. 一键吊销所有会话：怀疑凭证被拿到时不必改密码（改密码会连带影响其它用途）。
"""
from __future__ import annotations

import json
import sys
import tempfile
import time
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server import config, security  # noqa: E402


class _UsersCase(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self._orig = config.USERS_FILE
        config.USERS_FILE = Path(self._tmp.name) / 'users.json'
        security.save_users({
            'secret': 'S',
            'users': [{'username': 'admin', 'role': 'admin',
                       'pwd_hash': security.make_hash('p')}],
            'api_keys': [],
        })

    def tearDown(self) -> None:
        config.USERS_FILE = self._orig
        try:
            self._tmp.cleanup()
        except PermissionError:
            pass

    def _payload(self, token: str) -> dict:
        cfg = security.load_users()
        return security._unsign(token, cfg['secret']) or {}


class TokenBaselineTest(_UsersCase):
    """载荷里的两个时间基准。"""

    def test_fresh_token_has_iat_and_orig_equal(self) -> None:
        obj = self._payload(security.issue_token('admin', 'admin'))
        self.assertEqual(obj['iat'], obj['orig'])
        self.assertFalse(security.idle_expired(obj))
        self.assertFalse(security.needs_renewal(obj))

    def test_renewal_keeps_orig_but_moves_iat(self) -> None:
        """续期只推后空闲截止，**不延长总寿命**。"""
        base = int(time.time()) - 3600
        first = security.issue_token('admin', 'admin', orig=base)
        o1 = self._payload(first)
        self.assertEqual(o1['orig'], base)
        # 模拟一小时后续期
        renewed = security.issue_token('admin', 'admin', orig=o1['orig'])
        o2 = self._payload(renewed)
        self.assertEqual(o2['orig'], base, '续期不该推后总寿命的起点')
        self.assertEqual(o2['exp'], o1['exp'], 'exp 由 orig 决定，续期后不变')

    def test_exp_is_capped_by_total_days(self) -> None:
        obj = self._payload(security.issue_token('admin', 'admin'))
        self.assertLessEqual(obj['exp'] - obj['orig'], config.SESSION_DAYS * 86400 + 1)


class IdleExpiryTest(_UsersCase):
    def test_idle_expired_boundaries(self) -> None:
        now = int(time.time())
        idle = config.SESSION_IDLE_HOURS * 3600
        # 刚好在窗口内 → 未过期；超出 → 过期
        self.assertFalse(security.idle_expired({'iat': now - idle + 60}))
        self.assertTrue(security.idle_expired({'iat': now - idle - 60}))

    def test_missing_iat_is_treated_as_expired(self) -> None:
        """缺 iat 一律判失效 —— 无法判断活动时间就不能当它「刚活动过」（fail-open）。

        代价是本次升级前签发的旧 cookie 需要重登一次；这是安全修复的合理代价，
        已在 CHANGELOG 说明。
        """
        for bad in ({}, {'iat': None}, {'iat': 0}, {'iat': -1}, {'iat': 'x'}):
            self.assertTrue(security.idle_expired(bad), repr(bad))

    def test_renewal_triggers_after_one_third_of_window(self) -> None:
        now = int(time.time())
        idle = config.SESSION_IDLE_HOURS * 3600
        # 未到 1/3：不续（避免每次请求都重签 cookie）
        self.assertFalse(security.needs_renewal({'iat': now - idle // 4}))
        # 超过 1/3：该续了
        self.assertTrue(security.needs_renewal({'iat': now - idle // 2}))


class RevokeTest(_UsersCase):
    def test_revoke_bumps_sv_and_invalidates_old_token(self) -> None:
        token = security.issue_token('admin', 'admin')
        sv_before = self._payload(token)['sv']
        security.revoke_sessions('admin')
        sv_after = security.session_version(security.load_users(), 'admin')
        self.assertEqual(sv_after, sv_before + 1, 'sv 必须递增')
        # 旧 token 的 sv 已落后 → 鉴权处应判失效（这里直接比对语义）
        self.assertNotEqual(self._payload(token)['sv'], sv_after)

    def test_revoke_only_affects_that_user(self) -> None:
        cfg = security.load_users()
        cfg['users'].append({'username': 'viewer', 'role': 'viewer',
                             'pwd_hash': security.make_hash('p'), 'sv': 3})
        security.save_users(cfg)
        security.revoke_sessions('admin')
        self.assertEqual(security.session_version(security.load_users(), 'viewer'), 3,
                         '吊销某个用户不该影响其它用户')


class SecureWarningTest(unittest.TestCase):
    """反代存在但 cookie 拿不到 Secure 时要告警 —— 这类「以为安全其实没有」很难自查。"""

    def _req(self, headers: dict, scheme: str = 'http'):
        class R:
            def __init__(self) -> None:
                self.headers = headers
                self.url = type('U', (), {'scheme': scheme})()
        return R()

    def test_warns_behind_proxy_without_proto(self) -> None:
        for h in ({'x-forwarded-for': '1.2.3.4'},
                  {'x-real-ip': '1.2.3.4'},
                  {'x-forwarded-host': 'wb.example.com'}):
            self.assertTrue(security.insecure_cookie_warning(self._req(h)),
                            f'{h} 应告警')

    def test_no_warning_when_secure_already(self) -> None:
        h = {'x-forwarded-for': '1.2.3.4', 'x-forwarded-proto': 'https'}
        self.assertEqual(security.insecure_cookie_warning(self._req(h, 'https')), '')

    def test_no_warning_for_direct_local_access(self) -> None:
        """本地直连 HTTP（无反代）是正常调试形态，不该唠叨。"""
        self.assertEqual(security.insecure_cookie_warning(self._req({})), '')

    def test_explicit_false_is_respected(self) -> None:
        """用户显式关掉 Secure 时不再告警（尊重其选择）。"""
        orig = config.SECURE_COOKIE
        config.SECURE_COOKIE = 'false'
        try:
            self.assertEqual(
                security.insecure_cookie_warning(self._req({'x-forwarded-for': '1.2.3.4'})), '')
        finally:
            config.SECURE_COOKIE = orig


class RevokeEndpointTest(_UsersCase):
    """接口层：吊销端点必须仅管理员可用，且返回 relogin_required。"""

    def setUp(self) -> None:
        super().setUp()
        self._orig_db = config.DB_PATH
        config.DB_PATH = Path(self._tmp.name) / 'm.db'
        from server import db
        db._conn = None
        db.connect()
        self._db = db
        cfg = security.load_users()
        cfg['users'].append({'username': 'viewer', 'role': 'viewer',
                             'pwd_hash': security.make_hash('p')})
        security.save_users(cfg)
        from fastapi.testclient import TestClient
        from server.main import app
        self.client = TestClient(app)

    def tearDown(self) -> None:
        if self._db._conn is not None:
            self._db._conn.close()
        self._db._conn = None
        config.DB_PATH = self._orig_db
        super().tearDown()

    def test_requires_admin_and_returns_relogin(self) -> None:
        r = self.client.post('/api/sessions/revoke')
        self.assertEqual(r.status_code, 401, '匿名应被拒')

        self.client.post('/api/login', json={'username': 'viewer', 'password': 'p'})
        self.assertEqual(self.client.post('/api/sessions/revoke').status_code, 403,
                         '只读用户不该能吊销会话')
        self.client.post('/api/logout')

        r = self.client.post('/api/login', json={'username': 'admin', 'password': 'p'})
        self.assertEqual(r.status_code, 200, r.text)
        r = self.client.post('/api/sessions/revoke')
        self.assertEqual(r.status_code, 200, r.text)
        self.assertTrue(r.json()['relogin_required'])
        # 吊销后本会话也应失效
        self.assertEqual(self.client.get('/api/me').status_code, 401,
                         '吊销后旧 cookie 应不再被接受')


if __name__ == '__main__':
    unittest.main()
