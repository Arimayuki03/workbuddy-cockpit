const baseConfig = require('../config.json');
const { applyLocale, normalizeLanguage, parseInputs } = require('../src/utils/config');

function cloneConfig() {
  return JSON.parse(JSON.stringify(baseConfig));
}

describe('configuration', () => {
  test('uses chat completions by default', () => {
    expect(baseConfig.defaults.ai_api_type).toBe('chat-completions');
  });

  test('defaults unknown languages to English', () => {
    expect(normalizeLanguage('fr')).toBe('en');
    expect(applyLocale(cloneConfig(), 'fr')).toBe('en');
  });

  test('loads English responses by default', () => {
    const config = cloneConfig();
    applyLocale(config, 'en');

    expect(config.responses.issue_spam).toContain('This issue');
    expect(config.locale.answer_language).toBe('English');
  });

  test('loads Simplified Chinese when requested', () => {
    const config = cloneConfig();
    applyLocale(config, 'zh-CN');

    expect(config.responses.issue_spam).toContain('此Issue');
    expect(config.locale.answer_language).toBe('Simplified Chinese');
  });

  describe('parseInputs 数值防 NaN（C17/FIX-E）', () => {
    const core = require('@actions/core');

    beforeEach(() => {
      jest.resetModules();
      // 清空所有 INPUT_* 环境变量，避免宿主环境污染
      Object.keys(process.env).filter(k => k.startsWith('INPUT_')).forEach(k => delete process.env[k]);
    });

    afterEach(() => {
      Object.keys(process.env).filter(k => k.startsWith('INPUT_')).forEach(k => delete process.env[k]);
    });

    test('非数值输入回落默认值，而不是 NaN（NaN 会令 slice(0,NaN) 静默清空 related 列表）', () => {
      process.env.INPUT_MAX_RELATED_ISSUES = 'not-a-number';
      process.env.INPUT_RELATED_COMMENTS_PER_ISSUE = 'abc';
      process.env.INPUT_RELATED_BODY_TRUNCATE = '';
      process.env.INPUT_MAX_CANONICAL_INDEX = 'x50';
      process.env.INPUT_CANONICAL_BODY_TRUNCATE = '1500px';

      const result = parseInputs(cloneConfig());

      // Number.isFinite 守卫：解析失败回落 config.defaults
      expect(result.maxRelatedIssues).toBe(baseConfig.defaults.max_related_issues);
      expect(result.relatedCommentsPerIssue).toBe(baseConfig.defaults.related_comments_per_issue);
      expect(result.relatedBodyTruncate).toBe(baseConfig.defaults.related_body_truncate);
      expect(result.maxCanonicalIndex).toBe(baseConfig.defaults.max_canonical_index);
      expect(result.canonicalBodyTruncate).toBe(baseConfig.defaults.canonical_body_truncate);
    });

    test('正常数值输入照常解析', () => {
      process.env.INPUT_MAX_RELATED_ISSUES = '7';
      process.env.INPUT_MAX_CANONICAL_INDEX = '30';

      const result = parseInputs(cloneConfig());

      expect(result.maxRelatedIssues).toBe(7);
      expect(result.maxCanonicalIndex).toBe(30);
    });
  });
});
