// index.js 启动安全红线测试：配置了第三方 AI 端点（ai-base-url）却漏配 ai-api-key 时，
// 绝不把 GITHUB_TOKEN 静默当作 LLM key 发给第三方服务 —— 启动必须抛明确配置错误。
// 未配置 customBaseUrl（走 GitHub Models）时保留 token 回落（models:read 合法用途）。
//
// @actions/github 是纯 ESM 包且经动态 import 加载（jest CJS 环境不支持拦截），
// 通过 run() 的 deps.loadGithub 注入 mock；其余依赖走常规 jest.mock。

jest.mock('@actions/core', () => ({
  info: jest.fn(),
  warning: jest.fn(),
  error: jest.fn(),
  setFailed: jest.fn()
}));

jest.mock('openai', () => {
  return jest.fn().mockImplementation(() => ({ chat: {}, responses: {} }));
});

jest.mock('../src/utils/config', () => ({
  loadConfig: jest.fn(() => require('../config.json')),
  parseInputs: jest.fn()
}));

jest.mock('../src/handlers/issueHandler', () => ({ handleNewIssue: jest.fn().mockResolvedValue({}) }));
jest.mock('../src/handlers/prHandler', () => ({ handleNewPR: jest.fn().mockResolvedValue({}) }));

const core = require('@actions/core');
const OpenAI = require('openai');
const { parseInputs } = require('../src/utils/config');
const { handleNewIssue } = require('../src/handlers/issueHandler');
const { run } = require('../src/index');

function makeGithubModule() {
  return {
    getOctokit: jest.fn(() => ({ rest: {} })),
    context: {
      eventName: 'issues',
      payload: { action: 'opened' },
      repo: { owner: 'o', repo: 'r' }
    }
  };
}

function baseInputs(overrides = {}) {
  return {
    token: 'gh-token-abc',
    aiModel: 'openai/gpt-4o',
    labelsList: ['bug', 'enhancement'],
    blacklistUsers: [],
    analyzeFileChanges: true,
    analysisDepth: 'normal',
    maxFilesToAnalyze: 5,
    maxPatchLinesPerFile: 5,
    customBaseUrl: '',
    customApiKey: '',
    governanceToken: '',
    skipUsers: [],
    dryRun: true,
    maintainerExempt: true,
    enablePrGovernance: false,
    maxCanonicalIndex: 50,
    canonicalBodyTruncate: 1500,
    prReviewClose: false,
    maxRelatedIssues: 3,
    relatedCommentsPerIssue: 10,
    relatedBodyTruncate: 1500,
    canonicalLabel: 'canonical',
    duplicateLabel: 'duplicate',
    maxHistoryIndex: 100,
    enableTwoStage: false,
    maxScreenedCandidates: 5,
    screeningModel: '',
    config: {
      defaults: { api_base_url: 'https://models.github.ai/inference' },
      logging: {
        governance_identity_enabled: 'governance identity enabled',
        using_custom_api: 'using custom api',
        using_github_models: 'using github models',
        using_ai_model: 'model: {model}',
        config_info: 'config info',
        analysis_depth_info: 'analyze: {analyze_changes}',
        analysis_depth_details: 'depth {depth} (files: {files}, lines: {lines})',
        event_no_match: 'no match'
      }
    },
    ...overrides
  };
}

async function runWithInputs(inputs) {
  parseInputs.mockReturnValue(inputs);
  const githubModule = makeGithubModule();
  await run({ loadGithub: async () => githubModule });
  return githubModule;
}

describe('index.js AI 鉴权安全红线（第三方端点绝不回落 GITHUB_TOKEN）', () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  test('配置 ai-base-url 但漏配 ai-api-key：抛配置错误，绝不把 token 发给第三方', async () => {
    await runWithInputs(baseInputs({
      customBaseUrl: 'https://api.third-party.example/v1',
      customApiKey: ''
    }));

    // setFailed 携带明确配置错误信息（而非静默 fallback 到 token）
    expect(core.setFailed).toHaveBeenCalledTimes(1);
    const msg = core.setFailed.mock.calls[0][0];
    expect(msg).toContain('ai-api-key');
    expect(msg).toContain('GITHUB_TOKEN');
    // OpenAI 客户端从未被实例化（没有把 token 发出去的机会）
    expect(OpenAI).not.toHaveBeenCalled();
    // 事件处理从未执行
    expect(handleNewIssue).not.toHaveBeenCalled();
  });

  test('未配置 ai-base-url（走 GitHub Models）：保留 token 回落（models:read 合法用途）', async () => {
    const githubModule = await runWithInputs(baseInputs({
      customBaseUrl: '',
      customApiKey: ''
    }));

    expect(core.setFailed).not.toHaveBeenCalled();
    expect(OpenAI).toHaveBeenCalledTimes(1);
    expect(OpenAI.mock.calls[0][0]).toEqual({
      baseURL: 'https://models.github.ai/inference',
      apiKey: 'gh-token-abc'
    });
    expect(handleNewIssue).toHaveBeenCalledTimes(1);
    // GitHub 客户端用 token 构建
    expect(githubModule.getOctokit).toHaveBeenCalledWith('gh-token-abc');
  });

  test('配置 ai-base-url 且配了 ai-api-key：正常用自定义 key', async () => {
    await runWithInputs(baseInputs({
      customBaseUrl: 'https://api.third-party.example/v1',
      customApiKey: 'sk-third-party'
    }));

    expect(core.setFailed).not.toHaveBeenCalled();
    expect(OpenAI.mock.calls[0][0]).toEqual({
      baseURL: 'https://api.third-party.example/v1',
      apiKey: 'sk-third-party'
    });
    expect(handleNewIssue).toHaveBeenCalledTimes(1);
  });

  test('红线触发早于治理令牌客户端构建：漏配 key 时 getOctokit 至多一次（github-token 自身）', async () => {
    const githubModule = await runWithInputs(baseInputs({
      customBaseUrl: 'https://api.third-party.example/v1',
      customApiKey: '',
      governanceToken: 'claude-app-token'
    }));

    // governanceOctokit 在红线检查之前构建（令牌身份本身合法），但不会继续到 OpenAI
    expect(githubModule.getOctokit).toHaveBeenCalledTimes(2);
    expect(OpenAI).not.toHaveBeenCalled();
  });
});
