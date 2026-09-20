# namespace 双向兼容（本地补丁）

核对日期：2026-09-20。这是工具桥兼容，**不是早停根治**。

## 行为

- Responses `type=namespace` 的子工具会递归展开成 Chat `function`。
- 子工具优先保留原名；与顶层或其他子工具冲突时，后面的改成 `name_2`。
- 历史里的 `function_call` / `custom_tool_call` 用同一套出站名；
  返回时还原客户端原名。custom 子工具仍走 `{input:string}` 包装。
- `tool_choice` 的 named/custom 会映射到展开后的 Chat 函数名。
- length / 缺 finish / EOF 的残缺 custom 仍然不会作为可执行调用发出。

## 验证

`python3 -B -m unittest server.tests.test_namespace_compat server.tests.test_local_custom_compact_trace server.tests.test_responses_api server.tests.test_reasoning_credential_roundtrip -q`

90 passed。作为 Responses Chat 桥的上游缺陷修复提交到 PR #42；不是家里特制，也不是早停根治。
