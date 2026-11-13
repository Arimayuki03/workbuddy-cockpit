# 第三方声明（THIRD PARTY NOTICES）

本仓库 v1.25.0 的「成长中心 / 活动任务」端点配方参考了以下两个 MIT 许可项目。
仅移植**逆向得到的端点调用顺序与事件形状**，未复制其 HTTP/账号/调度实现。

## 1. 88lin/workbuddy-auto-signin

- 项目：https://github.com/88lin/workbuddy-auto-signin （v1.8.3）
- 用途：端点交叉验证（签到、成长中心、Buddy 盲盒 / 旅行的服务端状态语义）。
- 声明：`Copyright (c) 2026 88lin`

## 2. L0NE-6/WorkBuddy-Daily

- 项目：https://github.com/L0NE-6/WorkBuddy-Daily （v50 / 脚本 v3.49）
- 用途：任务配方主要增量来源（`expert_use_*` 动态发现、`skill_share` 分享渠道、
  Campaign 活动任务、盲盒与旅行顺序、未确认领奖去重、`credit_delayed` 语义）。
- 声明：`Copyright (c) 2026 L0NE-6`

## MIT License（两项目共同适用）

```
MIT License

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## 使用边界

- 两项目均在 README 声明「仅供学习与个人自动化，风险自负」；本仓库沿用同等免责口径。
- L0NE-6 的 NOTICE 仅约束品牌冒用与付费二次包装，不改变 MIT 授予的权利。
  本仓库不使用其项目名、作者身份或收款方式做背书。
- 未移植：`sym-v1` 本机凭证解密、桌面客户端换血、`verify=False`/`trust_env=False`、
  青龙/通知渠道、微信关注任务、手写多账号注册表（理由见
  `docs/DESIGN-v1.25.0-growth-coverage.md` §5）。
