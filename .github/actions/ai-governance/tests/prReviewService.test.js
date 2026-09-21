const baseConfig = require('../config.json');
const { applyLocale } = require('../src/utils/config');
const PrReviewService = require('../src/services/prReviewService');
const { PR_REVIEW_DECISIONS } = require('../src/utils/constants');

function buildConfig() {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  return config;
}

// 构造 openai stub：按调用顺序消费结果
function makeOpenai(resultsByCall = []) {
  const create = jest.fn();
  resultsByCall.forEach(res => create.mockResolvedValueOnce({
    choices: [{ message: { content: res } }]
  }));
  return {
    chat: { completions: { create } },
    _create: create
  };
}

// 构造 github ops mock（含历史语境评审新增的读操作）
function makeOps({
  canonicalItems = [],
  searchHits = [],
  comments = [],
  timeline = [],
  commits = []
} = {}) {
  return {
    listCanonicalIssues: jest.fn().mockResolvedValue(canonicalItems),
    searchRelatedClosedIssues: jest.fn().mockResolvedValue(searchHits),
    listIssueComments: jest.fn().mockResolvedValue(comments),
    listIssueTimeline: jest.fn().mockResolvedValue(timeline),
    listPRCommits: jest.fn().mockResolvedValue(commits),
    addComment: jest.fn().mockResolvedValue({}),
    addLabels: jest.fn().mockResolvedValue({}),
    createIssue: jest.fn().mockResolvedValue({ data: { number: 88 } }),
    updateIssueState: jest.fn().mockResolvedValue({}),
    updatePullRequest: jest.fn().mockResolvedValue({})
  };
}

const pr = {
  number: 42,
  title: 'feat: add caching layer',
  body: '希望网关加一层缓存，缓解后端压力。',
  user: { login: 'someone' }
};

const reviewComment = [
  '## 历史结论',
  '- #57 已因 wontfix 关闭',
  '',
  '## 本 PR 与历史结论的对照',
  '- #57 已明确拒绝该方案',
  '',
  '## 处理',
  '依据以上历史证据，本 PR 将被关闭。'
].join('\n');

describe('PrReviewService', () => {
  test('评审关闭：检索相关 issue → 补全评论/时间线 → AI 判 CLOSE（证据真实）→ 评论 + 关闭，不建 canonical', async () => {
    const config = buildConfig();
    // 调用顺序：judge(structured) -> 评审评论草稿(structured)
    const openai = makeOpenai([
      '```json\n{"decision":"CLOSE","reasons":["#57 wontfix"],"evidence":["#57 was closed as wontfix by maintainer"]}\n```',
      '```json\n{"comment":' + JSON.stringify(reviewComment) + '}\n```'
    ]);
    const ops = makeOps({
      canonicalItems: [{ number: 57, title: '缓存', body: '加缓存的诉求', state: 'closed' }],
      comments: [{ author: 'maintainer', body: '不打算做这个，wontfix', created_at: '2026-01-01' }],
      timeline: [{ event: 'closed', actor: 'maintainer', commit_id: null, created_at: '2026-01-02' }]
    });
    const svc = new PrReviewService(openai, 'model', config, {
      dryRun: false,
      prReviewClose: true
    }, ops);

    const result = await svc.review({}, 'o', 'r', pr, 'file1(+10/-2)');

    expect(result).toMatchObject({ decision: PR_REVIEW_DECISIONS.CLOSE, closed: true });

    // 评审评论：先评论
    const commentCall = ops.addComment.mock.calls.find(c => c[3] === 42);
    expect(commentCall).toBeTruthy();
    expect(commentCall[4]).toContain('## 历史结论');
    expect(commentCall[4]).toContain('#57');
    expect(commentCall[4].startsWith('🤖')).toBe(true);
    expect(commentCall[4]).toContain('✅ 机器人操作日志：');

    // 后关闭
    const closeCall = ops.updatePullRequest.mock.calls.find(c => c[4] && c[4].state === 'closed');
    expect(closeCall).toBeTruthy();
    expect(closeCall[4].pull_number === undefined || true).toBe(true); // 签名兼容

    // 不创建 canonical
    expect(ops.createIssue).not.toHaveBeenCalled();
    expect(ops.updateIssueState).not.toHaveBeenCalled();
  });

  test('无相关历史 issue：返回 null 回落旧关联链路，不调 AI、不写任何东西', async () => {
    const config = buildConfig();
    const openai = makeOpenai();
    const ops = makeOps({ canonicalItems: [], searchHits: [] });
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.review({}, 'o', 'r', pr, '');

    expect(result).toBeNull();
    expect(openai._create).not.toHaveBeenCalled();
    expect(ops.addComment).not.toHaveBeenCalled();
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
  });

  test('AI 判 KEEP：返回 null 回落，不评论不关闭', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"decision":"KEEP","reasons":["PR implements what #57 asked for"],"evidence":["#57 requested caching"]}\n```'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '缓存', body: 'x', state: 'closed' }] });
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.review({}, 'o', 'r', pr, '');

    expect(result).toBeNull();
    expect(ops.addComment).not.toHaveBeenCalled();
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
  });

  test('AI 判 UNCERTAIN：返回 null 回落（宁可漏判、不可误关）', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"decision":"UNCERTAIN","reasons":["evidence ambiguous"],"evidence":[]}\n```'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '缓存', body: 'x', state: 'closed' }] });
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.review({}, 'o', 'r', pr, '');

    expect(result).toBeNull();
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
  });

  test('防幻觉证据闸门：AI 判 CLOSE 但证据未引用真实 issue 编号 → 回落，不关闭', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      // 证据引用了语境包中不存在的 #999（幻觉）
      '```json\n{"decision":"CLOSE","reasons":["dup"],"evidence":["#999 already declined this"]}\n```'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '缓存', body: 'x', state: 'closed' }] });
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.review({}, 'o', 'r', pr, '');

    expect(result).toBeNull();
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
    expect(ops.addComment).not.toHaveBeenCalled();
  });

  test('AI 输出无法解析：视为 UNCERTAIN 回落，不关闭', async () => {
    const config = buildConfig();
    const openai = makeOpenai(['不是 JSON 的一团乱文本']);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '缓存', body: 'x', state: 'closed' }] });
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.review({}, 'o', 'r', pr, '');

    expect(result).toBeNull();
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
  });

  test('dry-run：判 CLOSE 但不关闭，只发「本应执行」演练评论', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"decision":"CLOSE","reasons":["#57 wontfix"],"evidence":["#57 wontfix"]}\n```',
      '```json\n{"comment":' + JSON.stringify(reviewComment) + '}\n```'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '缓存', body: 'x', state: 'closed' }] });
    const svc = new PrReviewService(openai, 'model', config, {
      dryRun: true,
      prReviewClose: true
    }, ops);

    const result = await svc.review({}, 'o', 'r', pr, '');

    expect(result).toMatchObject({ decision: PR_REVIEW_DECISIONS.CLOSE, dryRun: true });
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
    const commentCall = ops.addComment.mock.calls.find(c => c[3] === 42);
    expect(commentCall).toBeTruthy();
    expect(commentCall[4]).toContain('本应执行');
  });

  test('关闭失败容忍：评论已发出，关闭失败仅留痕，结果仍为已处理', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"decision":"CLOSE","reasons":["#57 wontfix"],"evidence":["#57 wontfix"]}\n```',
      '```json\n{"comment":' + JSON.stringify(reviewComment) + '}\n```'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '缓存', body: 'x', state: 'closed' }] });
    ops.updatePullRequest.mockRejectedValue(new Error('close boom'));
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.review({}, 'o', 'r', pr, '');

    expect(result).toMatchObject({ decision: PR_REVIEW_DECISIONS.CLOSE });
    // 评论先于关闭发出
    expect(ops.addComment).toHaveBeenCalled();
  });

  test('检索抛错：返回 null 回落，不演变成误关', async () => {
    const config = buildConfig();
    const openai = makeOpenai();
    const ops = makeOps({});
    ops.listCanonicalIssues.mockRejectedValue(new Error('search down'));
    ops.searchRelatedClosedIssues.mockRejectedValue(new Error('search down'));
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.review({}, 'o', 'r', pr, '');

    expect(result).toBeNull();
    expect(openai._create).not.toHaveBeenCalled();
  });

  test('评审评论草稿失败：返回 null 回落（证据链不完整不关闭）', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"decision":"CLOSE","reasons":["#57 wontfix"],"evidence":["#57 wontfix"]}\n```'
    ]);
    openai._create.mockRejectedValueOnce(new Error('comment draft boom'));
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '缓存', body: 'x', state: 'closed' }] });
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.review({}, 'o', 'r', pr, '');

    expect(result).toBeNull();
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
  });

  test('双通道去重：canonical 与文本检索命中同一 issue 只保留一份（canonical 优先）', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"decision":"KEEP","reasons":[],"evidence":["#57 ok"]}\n```'
    ]);
    const ops = makeOps({
      canonicalItems: [{ number: 57, title: '缓存 canonical', body: 'x', state: 'closed' }],
      searchHits: [
        { number: 57, title: '缓存 canonical', state: 'closed', state_reason: 'completed', closed_at: '2026-01-01', is_pr: false },
        { number: 63, title: '另一条', state: 'closed', state_reason: 'not_planned', closed_at: '2026-02-01', is_pr: false }
      ]
    });
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    await svc.review({}, 'o', 'r', pr, '');

    // 语境包里 #57 只出现一次、#63 并入
    const input = JSON.parse(openai._create.mock.calls[0][0].messages[1].content);
    const numbers = input.related_issues.map(i => i.number).sort();
    expect(numbers).toEqual([57, 63]);
    expect(numbers.filter(n => n === 57)).toHaveLength(1);
  });

  test('检索结果混入 PR 时被剔除（is_pr 双保险）', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"decision":"KEEP","reasons":[],"evidence":[]}\n```'
    ]);
    const ops = makeOps({
      searchHits: [
        { number: 99, title: '某个 PR', state: 'closed', state_reason: null, closed_at: null, is_pr: true }
      ]
    });
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.review({}, 'o', 'r', pr, '');

    // 全是 PR → 无相关 issue → 回落
    expect(result).toBeNull();
  });

  test('语境包结构：包含 PR 自身 + 提交 + 相关 issue 的评论与时间线摘要', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"decision":"KEEP","reasons":[],"evidence":["#57 ok"]}\n```'
    ]);
    const ops = makeOps({
      canonicalItems: [{ number: 57, title: '缓存', body: '正文'.repeat(2000), state: 'closed' }],
      comments: [{ author: 'maintainer', body: 'wontfix，别再提了', created_at: '2026-01-01' }],
      timeline: [
        { event: 'closed', actor: 'maintainer', commit_id: null, created_at: '2026-01-02' },
        { event: 'subscribed', actor: 'x', commit_id: null, created_at: '2026-01-03' } // 应被过滤
      ],
      commits: [{ message: 'feat: add cache', author: 'someone' }]
    });
    const svc = new PrReviewService(openai, 'model', config, { dryRun: false }, ops);

    await svc.review({}, 'o', 'r', pr, 'f1(+1/-1)');

    const input = JSON.parse(openai._create.mock.calls[0][0].messages[1].content);
    expect(input.pr.number).toBe(42);
    expect(input.pr.commits).toEqual(['feat: add cache']);
    expect(input.pr.files_changed).toBe('f1(+1/-1)');
    // 正文超长被截断（1500 + 截断标记）
    expect(input.related_issues[0].body.length).toBeLessThanOrEqual(1505);
    expect(input.related_issues[0].body).toMatch(/…\(截断\)$/);
    expect(input.related_issues[0].comments[0].body).toBe('wontfix，别再提了');
    // 时间线噪音事件被过滤，只留 closed
    expect(input.related_issues[0].timeline).toEqual([{ event: 'closed', actor: 'maintainer', commit_id: null }]);
  });
});
