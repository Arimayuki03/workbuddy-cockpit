"""推理凭据的**端到端**往返：输出侧给凭据 → 客户端带回 → 出站带 reasoning_content。

这是 issue #36 要求的测试，也补上了 v1.0.51 修复的真正缺口。

现场（用户报的）：用 Codex 走 Responses 协议连着聊，第二轮起上游回

    400 code=11155 reasoning_content_missing
    the reasoning content from the previous turn must be passed back in thinking mode

而把客户端协议换成 OpenAI chat completions 就完全正常。

根因：v1.0.51 的修复只做了**输入侧**——能把客户端发来的推理挂到 assistant 消息的
`reasoning_content` 上。但**输出侧从来没给过客户端可以回传的东西**：

  · Responses 在 `store:false` 下只回传带 `encrypted_content` 的 reasoning 项；
  · Anthropic 只回传带 `signature` 的 thinking 块。

没有这两个字段，客户端下一轮就一点推理痕迹都不带 —— 输入侧那段解析代码成了
**死代码**，上游照旧报 11155。

为什么 v1.0.51 的自测没发现：当时是**手工构造**了一个已经带推理痕迹的请求，
再检查发出去的 body 对不对。那只测了输入侧的转换，**绕过了「客户端到底能不能
拿到这个项」这一步**。所以本文件刻意按「一整轮」来测：

    上游响应 → 我们的输出 → （客户端原样带回）→ 下一轮出站 body

中间那一步用**真实的编码串**（不是手写的假值）穿过，才能发现「凭据没发出去」
这类断链。
"""
from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server.routers import anthropic as A  # noqa: E402
from server.routers import responses as R  # noqa: E402

REASONING = '先看用户想问什么，再看要不要查资料。\n然后组织答案。'


def _upstream_response(reasoning: str = REASONING, text: str = '答案') -> dict:
    """上游（OpenAI Chat Completions 形状）的一次完整响应。"""
    return {
        'choices': [{
            'message': {'role': 'assistant', 'content': text,
                        'reasoning_content': reasoning},
            'finish_reason': 'stop',
        }],
        'usage': {'prompt_tokens': 10, 'completion_tokens': 5},
    }


class ResponsesRoundTripTest(unittest.TestCase):
    """Responses 协议：整轮往返。"""

    def test_nonstream_output_carries_encrypted_content(self) -> None:
        """第一轮：非流式输出的 reasoning 项必须带 `encrypted_content`。

        客户端在 `store:false` 下只认这个字段——没有它整项被丢弃。这正是
        issue #36 报的「非流式 output 里只有 ['message']」。
        """
        obj = R.to_responses_object(_upstream_response(), 'm', 'resp_1')
        types = [i['type'] for i in obj['output']]
        self.assertIn('reasoning', types, f'输出里没有 reasoning 项：{types}')
        item = next(i for i in obj['output'] if i['type'] == 'reasoning')
        self.assertTrue(item.get('encrypted_content'),
                        'reasoning 项没有 encrypted_content —— 客户端会丢弃它')
        self.assertEqual(item['summary'][0]['text'], REASONING, 'summary 也要有（给人看）')

    def test_stream_output_carries_encrypted_content(self) -> None:
        """第一轮（流式）：reasoning 项的 done 事件也要带凭据。

        凭据在**收尾**时才算出（要等推理全文到齐），所以只检查
        `output_item.done` 上的那一份。
        """
        t = R._StreamTranslator('m', 'resp_1')
        events: list[dict] = []
        for obj in (
            {'choices': [{'delta': {'reasoning_content': REASONING}}]},
            {'choices': [{'delta': {'content': '答案'}}]},
            {'choices': [{'delta': {}, 'finish_reason': 'stop'}]},
        ):
            for raw in t.feed(obj):
                for line in raw.decode().splitlines():
                    if line.startswith('data:'):
                        events.append(json.loads(line[5:].strip()))
        done = [e for e in events
                if e.get('type') == 'response.output_item.done'
                and e.get('item', {}).get('type') == 'reasoning']
        self.assertEqual(len(done), 1, '没有 reasoning 的 output_item.done')
        self.assertTrue(done[0]['item'].get('encrypted_content'),
                        '流式 reasoning 项缺 encrypted_content')

    def test_whole_turn_roundtrip(self) -> None:
        """**核心断言**：第一轮的凭据原样带回，第二轮出站必须带 reasoning_content。

        这是 issue #36 建议的端到端形态——它覆盖了 v1.0.51 漏掉的那一步。
        """
        # 第一轮：我们产出给客户端
        first = R.to_responses_object(_upstream_response(), 'm', 'resp_1')
        reasoning_item = next(i for i in first['output'] if i['type'] == 'reasoning')
        credential = reasoning_item['encrypted_content']

        # 客户端把整个项原样带回（这是 Responses 的正常行为）
        second_body = {
            'model': 'deepseek-v4.1-flash',
            'input': [
                {'type': 'message', 'role': 'user', 'content': '第一个问题'},
                {'type': 'reasoning', 'id': reasoning_item['id'],
                 'summary': reasoning_item['summary'],
                 'encrypted_content': credential},
                {'type': 'message', 'role': 'assistant',
                 'content': [{'type': 'output_text', 'text': '答案'}]},
                {'type': 'message', 'role': 'user', 'content': '第二个问题'},
            ],
        }
        payload = R.to_chat_request(second_body)
        assistant = [m for m in payload['messages'] if m['role'] == 'assistant']
        self.assertTrue(assistant, '第二轮出站没有 assistant 消息')
        self.assertIn('reasoning_content', assistant[0],
                      '出站 assistant 消息缺 reasoning_content —— 上游会报 11155')
        self.assertEqual(assistant[0]['reasoning_content'], REASONING,
                         '带回去的推理内容不是原文')

    def test_decoded_from_credential_even_without_summary(self) -> None:
        """凭据能独立还原推理原文 —— 客户端截断 summary 时仍不丢。"""
        obj = R.to_responses_object(_upstream_response(), 'm', 'resp_1')
        item = next(i for i in obj['output'] if i['type'] == 'reasoning')
        body = {'input': [
            {'type': 'message', 'role': 'user', 'content': 'q'},
            # summary 被客户端截断成空，只剩凭据
            {'type': 'reasoning', 'summary': [],
             'encrypted_content': item['encrypted_content']},
            {'type': 'message', 'role': 'assistant',
             'content': [{'type': 'output_text', 'text': '答案'}]},
        ]}
        payload = R.to_chat_request(body)
        assistant = [m for m in payload['messages'] if m['role'] == 'assistant'][0]
        self.assertEqual(assistant['reasoning_content'], REASONING,
                         'summary 被截断时没能从凭据还原原文')

    def test_foreign_credential_does_not_break(self) -> None:
        """真·OpenAI 的加密串（不是本面板生成的）不该让解析报错。"""
        body = {'input': [
            {'type': 'message', 'role': 'user', 'content': 'q'},
            {'type': 'reasoning', 'encrypted_content': 'gAAAAABforeign-not-ours',
             'summary': [{'type': 'summary_text', 'text': '来自别处的推理'}]},
            {'type': 'message', 'role': 'assistant',
             'content': [{'type': 'output_text', 'text': '答案'}]},
        ]}
        payload = R.to_chat_request(body)
        assistant = [m for m in payload['messages'] if m['role'] == 'assistant'][0]
        self.assertEqual(assistant['reasoning_content'], '来自别处的推理',
                         '外来凭据解不开时应回落到 summary')


class AnthropicRoundTripTest(unittest.TestCase):
    """Anthropic 协议：整轮往返。"""

    def test_nonstream_thinking_block_carries_signature(self) -> None:
        obj = A.to_anthropic_response(_upstream_response(), 'm', thinking=True)
        types = [b['type'] for b in obj['content']]
        self.assertIn('thinking', types, f'content 里没有 thinking 块：{types}')
        block = next(b for b in obj['content'] if b['type'] == 'thinking')
        self.assertEqual(block['thinking'], REASONING)
        self.assertTrue(block.get('signature'), 'thinking 块没有 signature')

    def test_thinking_block_omitted_when_not_enabled(self) -> None:
        """没启用思考的客户端不该收到 thinking 块（严格客户端会当成异常）。"""
        obj = A.to_anthropic_response(_upstream_response(), 'm', thinking=False)
        types = [b['type'] for b in obj['content']]
        self.assertNotIn('thinking', types, f'未启用思考却回了 thinking：{types}')

    def test_stream_signature_delta_before_stop(self) -> None:
        """流式：签名必须**在** content_block_stop 之前发（否则被客户端丢弃）。"""
        t = A._StreamTranslator('m', thinking=True)
        events: list[dict] = []
        for obj in (
            {'choices': [{'delta': {'reasoning_content': REASONING}}]},
            {'choices': [{'delta': {'content': '答案'}}]},
            {'choices': [{'delta': {}, 'finish_reason': 'stop'}]},
        ):
            for raw in t.feed(obj):
                for line in raw.decode().splitlines():
                    if line.startswith('data:'):
                        events.append(json.loads(line[5:].strip()))

        starts = [e for e in events if e.get('type') == 'content_block_start'
                  and e.get('content_block', {}).get('type') == 'thinking']
        self.assertEqual(len(starts), 1, '没有 thinking 的 content_block_start')

        # 找到签名 delta 与思考块的 stop，断言顺序
        sig_at = next((i for i, e in enumerate(events)
                       if e.get('type') == 'content_block_delta'
                       and e.get('delta', {}).get('type') == 'signature_delta'), None)
        self.assertIsNotNone(sig_at, '流式没有发 signature_delta')
        stop_at = next((i for i, e in enumerate(events)
                        if e.get('type') == 'content_block_stop'
                        and e.get('index') == starts[0]['index']), None)
        self.assertIsNotNone(stop_at, 'thinking 块没有 content_block_stop')
        self.assertLess(sig_at, stop_at,
                        'signature_delta 发在 content_block_stop 之后 —— 客户端会丢弃')

        # 签名要能解回原文
        sig = events[sig_at]['delta']['signature']
        self.assertEqual(R._decode_credential(sig), REASONING)

    def test_stream_thinking_omitted_when_not_enabled(self) -> None:
        t = A._StreamTranslator('m', thinking=False)
        out: list[dict] = []
        for raw in t.feed({'choices': [{'delta': {'reasoning_content': REASONING}}]}):
            for line in raw.decode().splitlines():
                if line.startswith('data:'):
                    out.append(json.loads(line[5:].strip()))
        kinds = [e.get('content_block', {}).get('type') for e in out
                 if e.get('type') == 'content_block_start']
        self.assertNotIn('thinking', kinds, f'未启用思考却开了 thinking 块：{kinds}')

    def test_whole_turn_roundtrip(self) -> None:
        """**核心断言**：Anthropic 整轮往返。"""
        first = A.to_anthropic_response(_upstream_response(), 'm', thinking=True)
        block = next(b for b in first['content'] if b['type'] == 'thinking')

        second_body = {
            'model': 'deepseek-v4.1-flash',
            'max_tokens': 100,
            'thinking': {'type': 'enabled', 'budget_tokens': 2000},
            'messages': [
                {'role': 'user', 'content': '第一个问题'},
                {'role': 'assistant', 'content': [
                    {'type': 'thinking', 'thinking': block['thinking'],
                     'signature': block['signature']},
                    {'type': 'text', 'text': '答案'},
                ]},
                {'role': 'user', 'content': '第二个问题'},
            ],
        }
        payload = A.to_openai_request(second_body)
        assistant = [m for m in payload['messages'] if m['role'] == 'assistant']
        self.assertTrue(assistant, '第二轮出站没有 assistant 消息')
        self.assertIn('reasoning_content', assistant[0],
                      '出站 assistant 消息缺 reasoning_content')
        self.assertEqual(assistant[0]['reasoning_content'], REASONING)

    def test_signature_only_thinking_block_still_works(self) -> None:
        """只有签名、没有明文（部分客户端脱敏）时也能还原。"""
        first = A.to_anthropic_response(_upstream_response(), 'm', thinking=True)
        block = next(b for b in first['content'] if b['type'] == 'thinking')
        payload = A.to_openai_request({
            'model': 'm', 'max_tokens': 10,
            'messages': [
                {'role': 'user', 'content': 'q'},
                {'role': 'assistant', 'content': [
                    {'type': 'thinking', 'thinking': '', 'signature': block['signature']},
                ]},
            ],
        })
        assistant = [m for m in payload['messages'] if m['role'] == 'assistant'][0]
        self.assertEqual(assistant['reasoning_content'], REASONING,
                         '只有签名时没能还原推理原文')


class ThinkingEnabledDetectionTest(unittest.TestCase):
    """`thinking.type == 'enabled'` 的判定（含畸形值）。"""

    def test_enabled(self) -> None:
        self.assertTrue(A._thinking_enabled({'thinking': {'type': 'enabled'}}))
        self.assertTrue(A._thinking_enabled({'thinking': {'type': 'ENABLED'}}))

    def test_not_enabled(self) -> None:
        for body in ({}, {'thinking': None}, {'thinking': {}},
                     {'thinking': {'type': 'disabled'}}, {'thinking': 'enabled'},
                     {'thinking': []}, {'thinking': 123}):
            with self.subTest(body=body):
                self.assertFalse(A._thinking_enabled(body))


if __name__ == '__main__':
    unittest.main()


class ReasoningOrderingTest(unittest.TestCase):
    """推理项**位置异常**时也不能丢（自审发现的边界）。

    原实现假设「reasoning 总是紧接在它对应的 assistant 消息之前」，于是：

      · 紧跟的不是 assistant 时**丢弃**这段推理（注释里写的是「孤立推理，
        丢掉比挂到不相关的回合上更糟」）；
      · 连续多个 reasoning item 时只保留**最后一个**。

    两个假设在真实客户端上不成立。后果与 issue #36 完全一样：客户端明明带回了
    凭据，我们却把内容丢在半路，assistant 消息上没有 `reasoning_content`，
    上游照旧报 11155 —— 也就是这个文件存在的理由。既然这一类 bug 已经咬过两次，
    这里就把「无论顺序如何，只要拿到了推理就必须落到某条 assistant 消息上」钉死。
    """

    def _assistant_reasoning(self, payload: dict) -> list[object]:
        return [m.get('reasoning_content') for m in payload['messages']
                if m['role'] == 'assistant']

    def test_reasoning_after_assistant_is_not_dropped(self) -> None:
        """推理项排在 assistant 消息**之后**时，仍要挂到那条消息上。"""
        payload = R.to_chat_request({'input': [
            {'type': 'message', 'role': 'user', 'content': 'q'},
            {'type': 'message', 'role': 'assistant',
             'content': [{'type': 'output_text', 'text': 'a'}]},
            {'type': 'reasoning',
             'encrypted_content': R._encode_credential('晚到的推理')},
        ]})
        self.assertEqual(self._assistant_reasoning(payload), ['晚到的推理'],
                         '排查到 assistant 之后的推理被丢掉了 —— 又会触发 11155')

    def test_multiple_reasoning_items_are_joined(self) -> None:
        """连续多个推理项要**合并**，不能只留最后一个。"""
        payload = R.to_chat_request({'input': [
            {'type': 'message', 'role': 'user', 'content': 'q'},
            {'type': 'reasoning', 'encrypted_content': R._encode_credential('第一段')},
            {'type': 'reasoning', 'encrypted_content': R._encode_credential('第二段')},
            {'type': 'message', 'role': 'assistant',
             'content': [{'type': 'output_text', 'text': 'a'}]},
        ]})
        got = self._assistant_reasoning(payload)[0]
        self.assertIn('第一段', got, '前一段推理被后一段顶掉了')
        self.assertIn('第二段', got)

    def test_reasoning_before_user_still_reaches_next_assistant(self) -> None:
        """推理项后面跟的是 user（顺序异常）时，留给**后面**那条 assistant。"""
        payload = R.to_chat_request({'input': [
            {'type': 'message', 'role': 'user', 'content': 'q1'},
            {'type': 'reasoning', 'encrypted_content': R._encode_credential('思考')},
            {'type': 'message', 'role': 'user', 'content': 'q2'},
            {'type': 'message', 'role': 'assistant',
             'content': [{'type': 'output_text', 'text': 'a'}]},
        ]})
        self.assertEqual(self._assistant_reasoning(payload), ['思考'])

    def test_orphan_reasoning_without_any_assistant_is_harmless(self) -> None:
        """整段历史里没有 assistant 消息时，推理无处可挂 —— 不能报错。

        这种请求本来也不合法（没有可回传的回合），只要不抛异常、消息序列正常即可。
        """
        payload = R.to_chat_request({'input': [
            {'type': 'message', 'role': 'user', 'content': 'q'},
            {'type': 'reasoning', 'encrypted_content': R._encode_credential('孤立的')},
        ]})
        self.assertEqual([m['role'] for m in payload['messages']], ['user'])


class AnthropicReasoningAccumulationTest(unittest.TestCase):
    """Anthropic 侧的多 thinking 块也要合并（与 Responses 侧对齐）。

    一条 assistant 里可能有多个 thinking 块（分段推理、或 redacted 与明文并存）。
    早先的实现每见到一块就**覆盖** `thinking_text`，只有最后一块活下来 ——
    与 responses.py 的累积行为不一致。两套协议面对的是同一份推理内容，
    行为不该因客户端选了哪个协议而不同。
    """

    def test_multiple_thinking_blocks_are_joined(self) -> None:
        payload = A.to_openai_request({'model': 'm', 'max_tokens': 10, 'messages': [
            {'role': 'user', 'content': 'q'},
            {'role': 'assistant', 'content': [
                {'type': 'thinking', 'thinking': '段1',
                 'signature': R._encode_credential('段1')},
                {'type': 'thinking', 'thinking': '段2',
                 'signature': R._encode_credential('段2')},
                {'type': 'text', 'text': 'a'},
            ]},
        ]})
        assistant = [m for m in payload['messages'] if m['role'] == 'assistant'][0]
        got = assistant.get('reasoning_content', '')
        self.assertIn('段1', got, '前一个 thinking 块被后一个顶掉了')
        self.assertIn('段2', got)

    def test_plain_and_signed_blocks_are_both_kept(self) -> None:
        """明文块 + 签名块并存时，两段都要保留。"""
        payload = A.to_openai_request({'model': 'm', 'max_tokens': 10, 'messages': [
            {'role': 'user', 'content': 'q'},
            {'role': 'assistant', 'content': [
                {'type': 'thinking', 'thinking': '明文段'},
                {'type': 'thinking', 'thinking': '',
                 'signature': R._encode_credential('签名段')},
            ]},
        ]})
        assistant = [m for m in payload['messages'] if m['role'] == 'assistant'][0]
        got = assistant.get('reasoning_content', '')
        self.assertIn('明文段', got)
        self.assertIn('签名段', got)
