const { handleNewPR, isCollaborator } = require('../src/handlers/prHandler');
const { applyLocale } = require('../src/utils/config');
const baseConfig = require('../config.json');

jest.mock('../src/services/prWorkflowService');
jest.mock('../src/services/prGovernanceService');
jest.mock('../src/services/prReviewService');

const PrWorkflowService = require('../src/services/prWorkflowService');
const PrGovernanceService = require('../src/services/prGovernanceService');
const PrReviewService = require('../src/services/prReviewService');

function buildConfig() {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  return config;
}

const govDefaults = { dryRun: false, maintainerExempt: true };

function makePR(overrides = {}) {
  return {
    number: 42,
    title: 'feat: add caching',
    body: 'body',
    user: { login: 'someone' },
    ...overrides
  };
}

function makeContext(pr) {
  return {
    payload: { pull_request: pr, action: 'opened' },
    eventName: 'pull_request_target'
  };
}

function makeOctokit({ isCollaborator = false } = {}) {
  const checkCollaborator = jest.fn();
  if (isCollaborator) {
    checkCollaborator.mockResolvedValue({ status: 204 });
  } else {
    checkCollaborator.mockRejectedValue(new Error('not a collaborator'));
  }
  return {
    rest: {
      repos: { checkCollaborator },
      pulls: {
        listFiles: jest.fn().mockResolvedValue({ data: [] }),
        update: jest.fn().mockResolvedValue({})
      },
      issues: { createComment: jest.fn().mockResolvedValue({}) }
    }
  };
}

describe('prHandler', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    PrWorkflowService.mockImplementation(() => ({
      performLayeredDetection: jest.fn().mockResolvedValue({ decision: 'KEEP' }),
      classifyAndLabelPR: jest.fn().mockResolvedValue('enhancement'),
      config: buildConfig()
    }));
    PrGovernanceService.mockImplementation(() => ({
      govern: jest.fn().mockResolvedValue({})
    }));
    PrReviewService.mockImplementation(() => ({
      review: jest.fn().mockResolvedValue(null)
    }));
  });

  test('维护者豁免：协作者 PR 做垃圾检测 + 分类，但不进入治理', async () => {
    const config = buildConfig();
    const pr = makePR();
    const octokit = makeOctokit({ isCollaborator: true });
    const gov = { ...govDefaults };

    await handleNewPR(octokit, {}, makeContext(pr), 'o', 'r', 'model', config, ['enhancement'], [], gov);

    // 垃圾检测仍执行
    expect(PrWorkflowService).toHaveBeenCalled();
    // 不调用治理
    expect(PrGovernanceService).not.toHaveBeenCalled();
  });

  test('跳过名单（bot 自环）：直接返回，不做任何检测与治理', async () => {
    const config = buildConfig();
    const pr = makePR({ user: { login: 'github-actions[bot]' } });
    const octokit = makeOctokit();
    const gov = { ...govDefaults };

    await handleNewPR(octokit, {}, makeContext(pr), 'o', 'r', 'model', config, ['enhancement'], [], gov);

    expect(PrWorkflowService).not.toHaveBeenCalled();
    expect(PrGovernanceService).not.toHaveBeenCalled();
  });

  test('AI 失败放行：治理抛错时只评论放行，不关闭 PR', async () => {
    const config = buildConfig();
    const pr = makePR();
    const octokit = makeOctokit();
    const gov = { ...govDefaults };

    // 治理服务 govern 抛错
    PrGovernanceService.mockImplementation(() => ({
      govern: jest.fn().mockRejectedValue(new Error('AI boom'))
    }));

    await handleNewPR(octokit, {}, makeContext(pr), 'o', 'r', 'model', config, ['enhancement'], [], gov);

    // 不调用关闭（closePR 会走 octokit.rest.pulls.update({state:'closed'})）
    expect(octokit.rest.pulls.update).not.toHaveBeenCalled();
  });

  test('isCollaborator：查询失败按非协作者处理（宁可多治理）', async () => {
    const octokit = makeOctokit({ isCollaborator: false });
    await expect(isCollaborator(octokit, 'o', 'r', 'someone')).resolves.toBe(false);
  });

  // ---- PR 历史语境评审（pr-review-close）接线 ----

  test('pr-review-close 开启：评审服务被调用，且在上游垃圾检测之后、旧治理之前', async () => {
    const config = buildConfig();
    const pr = makePR();
    const octokit = makeOctokit();
    const gov = { ...govDefaults, prReviewClose: true };

    await handleNewPR(octokit, {}, makeContext(pr), 'o', 'r', 'model', config, ['enhancement'], [], gov);

    expect(PrReviewService).toHaveBeenCalled();
    // 旧治理也在（评审返回 null 回落）
    expect(PrGovernanceService).toHaveBeenCalled();
  });

  test('pr-review-close 开启且评审已处理：不再走旧 canonical 关联治理', async () => {
    const config = buildConfig();
    const pr = makePR();
    const octokit = makeOctokit();
    const gov = { ...govDefaults, prReviewClose: true };

    PrReviewService.mockImplementation(() => ({
      review: jest.fn().mockResolvedValue({ decision: 'CLOSE', closed: true })
    }));

    await handleNewPR(octokit, {}, makeContext(pr), 'o', 'r', 'model', config, ['enhancement'], [], gov);

    expect(PrReviewService).toHaveBeenCalled();
    expect(PrGovernanceService).not.toHaveBeenCalled();
  });

  test('pr-review-close 关闭（默认）：不实例化评审服务，行为与原先一致', async () => {
    const config = buildConfig();
    const pr = makePR();
    const octokit = makeOctokit();
    const gov = { ...govDefaults }; // 未设置 prReviewClose

    await handleNewPR(octokit, {}, makeContext(pr), 'o', 'r', 'model', config, ['enhancement'], [], gov);

    expect(PrReviewService).not.toHaveBeenCalled();
    expect(PrGovernanceService).toHaveBeenCalled();
  });

  test('评审插在维护者豁免之后：协作者 PR 不进入评审', async () => {
    const config = buildConfig();
    const pr = makePR();
    const octokit = makeOctokit({ isCollaborator: true });
    const gov = { ...govDefaults, prReviewClose: true };

    await handleNewPR(octokit, {}, makeContext(pr), 'o', 'r', 'model', config, ['enhancement'], [], gov);

    expect(PrReviewService).not.toHaveBeenCalled();
  });

  test('评审服务抛错：走既有 fail-open 兜底（评论放行、不关闭 PR）', async () => {
    const config = buildConfig();
    const pr = makePR();
    const octokit = makeOctokit();
    const gov = { ...govDefaults, prReviewClose: true };

    PrReviewService.mockImplementation(() => ({
      review: jest.fn().mockRejectedValue(new Error('review boom'))
    }));

    await handleNewPR(octokit, {}, makeContext(pr), 'o', 'r', 'model', config, ['enhancement'], [], gov);

    // 不关闭（fail-open），且发出兜底评论
    expect(octokit.rest.pulls.update).not.toHaveBeenCalled();
    expect(octokit.rest.issues.createComment).toHaveBeenCalled();
  });

  test('评审返回 null（证据不足）：回落旧 canonical 关联治理', async () => {
    const config = buildConfig();
    const pr = makePR();
    const octokit = makeOctokit();
    const gov = { ...govDefaults, prReviewClose: true };

    PrReviewService.mockImplementation(() => ({
      review: jest.fn().mockResolvedValue(null)
    }));

    await handleNewPR(octokit, {}, makeContext(pr), 'o', 'r', 'model', config, ['enhancement'], [], gov);

    expect(PrReviewService).toHaveBeenCalled();
    expect(PrGovernanceService).toHaveBeenCalled();
  });
});