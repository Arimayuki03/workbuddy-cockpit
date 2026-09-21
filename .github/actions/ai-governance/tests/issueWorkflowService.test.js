const baseConfig = require('../config.json');
const { applyLocale } = require('../src/utils/config');
const IssueWorkflowService = require('../src/services/issueWorkflowService');

// callAI 对 429/5xx/网络错误做指数退避重试；测试注入 0ms 延迟加速（见 tests/ai.test.js）
process.env.AI_RETRY_DELAY_MS = '0';

function buildConfig() {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  return config;
}

// openai stub：按调用顺序返回预设结果
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

// octokit stub：issues 系列 REST 端点
function makeOctokit() {
  return {
    rest: {
      issues: {
        createComment: jest.fn().mockResolvedValue({}),
        update: jest.fn().mockResolvedValue({}),
        lock: jest.fn().mockResolvedValue({}),
        addLabels: jest.fn().mockResolvedValue({})
      },
      repos: {
        getReadme: jest.fn().mockRejectedValue(new Error('no readme'))
      }
    }
  };
}

const issue = {
  number: 22,
  title: '登录失败怎么解决',
  body: '装完之后登录不上去。',
  user: { login: 'someone' }
};

const qualityAnalysis = {
  contentInfo: { userContent: '装完之后登录不上去。', validSections: 0 },
  templateInfo: { hasTemplate: false, templateType: '', confidence: 0 },
  quality: { level: 'low', score: 10 }
};

const labelsList = ['bug', 'enhancement', 'question'];

describe('IssueWorkflowService.classifyAndHandleIssue', () => {
  test('BASIC 问题：评论 issue_basic + 关闭 + 锁定（closeAndLock 真实存在，不再 TypeError）', async () => {
    const config = buildConfig();
    // 依次：分类 -> 内容质量
    const openai = makeOpenai(['BUG', 'BASIC']);
    const octokit = makeOctokit();
    const svc = new IssueWorkflowService(octokit, openai, 'model', config);

    const result = await svc.classifyAndHandleIssue('o', 'r', issue, qualityAnalysis, labelsList);

    expect(result.closed).toBe(true);
    expect(result.classification).toBe('BUG');
    // 评论先于关闭（安全写序）
    expect(octokit.rest.issues.createComment).toHaveBeenCalledTimes(1);
    expect(octokit.rest.issues.createComment.mock.calls[0][0].body).toContain('基础');
    expect(octokit.rest.issues.update).toHaveBeenCalledWith(
      expect.objectContaining({ state: 'closed' })
    );
    expect(octokit.rest.issues.lock).toHaveBeenCalledWith(
      expect.objectContaining({ lock_reason: 'spam' })
    );
  });

  test('UNCLEAR 问题：智能回答/标准提示，不关闭', async () => {
    const config = buildConfig();
    const openai = makeOpenai(['BUG', 'UNCLEAR']);
    const octokit = makeOctokit();
    const svc = new IssueWorkflowService(octokit, openai, 'model', config);

    const result = await svc.classifyAndHandleIssue('o', 'r', issue, qualityAnalysis, labelsList);

    expect(result.needsInfo).toBe(true);
    expect(octokit.rest.issues.update).not.toHaveBeenCalled();
    expect(octokit.rest.issues.createComment).toHaveBeenCalled();
  });

  test('分类抛错：返回 {classification:null, error:true}（FIX-D 将在 handler 消费）', async () => {
    const config = buildConfig();
    const openai = makeOpenai();
    openai._create.mockRejectedValue(new Error('classify boom'));
    const octokit = makeOctokit();
    const svc = new IssueWorkflowService(octokit, openai, 'model', config);

    const result = await svc.classifyAndHandleIssue('o', 'r', issue, qualityAnalysis, labelsList);

    expect(result.error).toBe(true);
    expect(result.classification).toBeNull();
  });
});

describe('IssueWorkflowService 构造函数与 fetchReadmeContent', () => {
  test('构造函数保存 this.octokit（此前缺失，getReadme 必抛 TypeError 被吞成 null）', () => {
    const config = buildConfig();
    const octokit = makeOctokit();
    const svc = new IssueWorkflowService(octokit, {}, 'model', config);

    expect(svc.octokit).toBe(octokit);
  });

  test('UNCLEAR 分支：fetchReadmeContent 真正取回 README 并用于智能回答', async () => {
    const config = buildConfig();
    const readmeText = '# 使用文档\n\n如何配置网关：……';
    const openai = makeOpenai(['BUG', 'UNCLEAR', 'HELPFUL_ANSWER: 请参考 README 的配置章节']);
    const octokit = makeOctokit();
    octokit.rest.repos.getReadme = jest.fn().mockResolvedValue({
      data: { content: Buffer.from(readmeText, 'utf8').toString('base64') }
    });
    const svc = new IssueWorkflowService(octokit, openai, 'model', config);

    const result = await svc.classifyAndHandleIssue('o', 'r', issue, qualityAnalysis, labelsList);

    expect(result.needsInfo).toBe(true);
    // getReadme 被真实调用（修复前 this.octokit 为 undefined，必然 TypeError）
    expect(octokit.rest.repos.getReadme).toHaveBeenCalledWith({ owner: 'o', repo: 'r' });
    // 智能回答评论带 unclear_answer_prefix（而非回落标准提示 issue_unclear）
    const comment = octokit.rest.issues.createComment.mock.calls[0][0].body;
    expect(comment).toContain('根据项目文档');
    expect(comment).toContain('请参考 README 的配置章节');
  });

  test('UNCLEAR 智能回答注入：AI 回答带 <script> 与 @user 时发布前被净化（前缀保留）', async () => {
    const config = buildConfig();
    const readmeText = '# 使用文档\n\n如何配置网关：……';
    const openai = makeOpenai([
      'BUG',
      'UNCLEAR',
      'HELPFUL_ANSWER: 请参考 README <script>alert(1)</script>章节，紧急联系 @admin：[点我](https://evil.example.com/help)'
    ]);
    const octokit = makeOctokit();
    octokit.rest.repos.getReadme = jest.fn().mockResolvedValue({
      data: { content: Buffer.from(readmeText, 'utf8').toString('base64') }
    });
    const svc = new IssueWorkflowService(octokit, openai, 'model', config);

    await svc.classifyAndHandleIssue('o', 'r', issue, qualityAnalysis, labelsList);

    const comment = octokit.rest.issues.createComment.mock.calls[0][0].body;
    // 前缀（受控模板）保留，AI 段落净化：script 删除、@提及中和、第三方链接降级
    expect(comment).toContain(config.responses.unclear_answer_prefix.trim());
    expect(comment).not.toContain('<script');
    expect(comment).not.toContain('alert(1)');
    expect(comment).toContain('@ admin');
    expect(comment).not.toContain('](https://evil.example.com/help)');
  });

  test('README 覆盖回答注入：AI 回答带 @user 时发布前被中和（README_COVERED 路径）', async () => {
    const config = buildConfig();
    const readmeText = '# 使用文档\n\n支持 xxx。';
    // handleReadmeRelatedIssue 只调一次 AI：generateReadmeAnswer
    const openai = makeOpenai(['答案在此，详见 @maintainer 的说明']);
    const octokit = makeOctokit();
    octokit.rest.repos.getReadme = jest.fn().mockResolvedValue({
      data: { content: Buffer.from(readmeText, 'utf8').toString('base64') }
    });
    const svc = new IssueWorkflowService(octokit, openai, 'model', config);

    // README_COVERED：handleReadmeRelatedIssue → generateReadmeAnswer → addReadmeAnswer
    const result = await svc.handleReadmeRelatedIssue('o', 'r', issue, readmeText);

    expect(result).toBe(true);
    const comment = octokit.rest.issues.createComment.mock.calls[0][0].body;
    expect(comment).toContain(config.responses.readme_answer_prefix.trim());
    expect(comment).toContain('@ maintainer');
    expect(comment).not.toContain('@maintainer');
  });

  test('无 README（getReadme 404）：fetchReadmeContent 返回 null，回落标准提示', async () => {
    const config = buildConfig();
    const openai = makeOpenai(['BUG', 'UNCLEAR']);
    const octokit = makeOctokit(); // getReadme mockRejectedValue
    const svc = new IssueWorkflowService(octokit, openai, 'model', config);

    const readme = await svc.fetchReadmeContent('o', 'r');
    expect(readme).toBeNull();

    const result = await svc.classifyAndHandleIssue('o', 'r', issue, qualityAnalysis, labelsList);
    expect(result.needsInfo).toBe(true);
    // 回落标准提示（issue_unclear）
    const comment = octokit.rest.issues.createComment.mock.calls[0][0].body;
    expect(comment).toContain('缺少足够信息');
  });
});
