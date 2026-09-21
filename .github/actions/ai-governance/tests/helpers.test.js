const { logMessage, sanitizeLogText, handleApiCall } = require('../src/utils/helpers');
const { analyzeFileChanges, getLastPageNumber } = require('../src/handlers/prHandler');
const { applyLocale } = require('../src/utils/config');
const baseConfig = require('../config.json');

function buildConfig(overrides = {}) {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  config.ai_settings.analyze_file_changes = true;
  config.ai_settings.max_files_to_analyze = 5;
  config.ai_settings.max_patch_lines_per_file = 5;
  return { ...config, ...overrides };
}

describe('helpers.sanitizeLogText（GitHub Actions 日志注入防护）', () => {
  test('中和 workflow-command 序列 ::（防伪造 ::error:: / ::warning::）', () => {
    expect(sanitizeLogText('::error::injected')).toBe('：:error：:injected');
  });

  test('去除 \r\n 换行，防止伪造多条日志行', () => {
    expect(sanitizeLogText('line1\n::error::fake\r\nline3')).not.toMatch(/[\r\n]/);
    expect(sanitizeLogText('line1\n::error::fake\r\nline3')).toBe('line1 ：:error：:fake line3');
  });

  test('正常标题原样保留', () => {
    expect(sanitizeLogText('feat: add caching layer')).toBe('feat: add caching layer');
  });

  test('非字符串输入（null/undefined）返回空串，不抛错', () => {
    expect(sanitizeLogText(null)).toBe('');
    expect(sanitizeLogText(undefined)).toBe('');
  });
});

describe('helpers.logMessage（函数替换，防 $ 模式注入）', () => {
  test('替换值中的 $& 不再被展开为整个匹配', () => {
    expect(logMessage('检查Issue: {title}', { title: '$&$`$\'$0abc' }))
      .toBe('检查Issue: $&$`$\'$0abc');
  });

  test('替换值中的 $1（数字引用）原样保留', () => {
    expect(logMessage('错误: {error}', { error: '$1 invalid capture' }))
      .toBe('错误: $1 invalid capture');
  });

  test('普通插值行为不变', () => {
    expect(logMessage('Issue #{number} 已关闭', { number: 42 }))
      .toBe('Issue #42 已关闭');
  });

  test('同一占位符出现多次全部替换（g 标志保持）', () => {
    expect(logMessage('{a}和{a}', { a: '$&' })).toBe('$&和$&');
  });
});

describe('prHandler.analyzeFileChanges（per_page 与真实总数）', () => {
  function makePr() {
    return { number: 42, title: 'feat: x', body: '', user: { login: 'someone' } };
  }

  function makeFile(i) {
    return { filename: `src/file${i}.js`, status: 'modified', additions: 1, deletions: 0, patch: '+line' };
  }

  test('listFiles 显式携带 per_page: 100（修复默认 30 截断）', async () => {
    const listFiles = jest.fn().mockResolvedValue({
      data: [makeFile(1)],
      headers: {}
    });
    const octokit = { rest: { pulls: { listFiles } } };

    await analyzeFileChanges(octokit, 'o', 'r', makePr(), buildConfig());

    expect(listFiles).toHaveBeenCalledWith(
      expect.objectContaining({ pull_number: 42, per_page: 100 })
    );
  });

  test('无 Link 头时总数即当前页条数（不虚构）', async () => {
    const listFiles = jest.fn().mockResolvedValue({
      data: [makeFile(1), makeFile(2), makeFile(3)],
      headers: {}
    });
    const octokit = { rest: { pulls: { listFiles } } };

    const result = await analyzeFileChanges(octokit, 'o', 'r', makePr(), buildConfig());

    expect(result).not.toContain('共');
    expect(result).not.toContain('truncat');
  });

  test('Link 头 rel="last" 存在：首页满页时取末页一次，截断提示按真实总数计（而非 100）', async () => {
    // 250 个文件的 PR：首页满 100 条，末页 3 → 总数 = 2*100 + 50
    const firstPage = Array.from({ length: 100 }, (_, i) => makeFile(i));
    const lastPage = Array.from({ length: 50 }, (_, i) => makeFile(i));
    const listFiles = jest.fn()
      .mockResolvedValueOnce({
        data: firstPage,
        headers: {
          link: '<https://api.github.com/repos/o/r/pulls/42/files?page=2&per_page=100>; rel="next", ' +
            '<https://api.github.com/repos/o/r/pulls/42/files?page=3&per_page=100>; rel="last"'
        }
      })
      .mockResolvedValueOnce({ data: lastPage, headers: {} });
    const octokit = { rest: { pulls: { listFiles } } };

    const result = await analyzeFileChanges(octokit, 'o', 'r', makePr(), buildConfig());

    expect(result).toContain('共250个文件，仅显示前5个');
    // 第二次调用带末页页码
    expect(listFiles).toHaveBeenCalledTimes(2);
    expect(listFiles.mock.calls[1][0]).toEqual(expect.objectContaining({ page: 3, per_page: 100 }));
  });

  test('≤100 文件的 PR（无分页 Link 头）：不发生第二次调用，提示按 data.length 计', async () => {
    const files = Array.from({ length: 50 }, (_, i) => makeFile(i));
    const listFiles = jest.fn().mockResolvedValue({ data: files, headers: {} });
    const octokit = { rest: { pulls: { listFiles } } };

    const result = await analyzeFileChanges(octokit, 'o', 'r', makePr(), buildConfig());

    // 50 个文件 > 显示的 5 个：提示按真实总数 50 计（修复前 data.length 已是该值，行为一致）
    expect(result).toContain('共50个文件，仅显示前5个');
    expect(listFiles).toHaveBeenCalledTimes(1);
  });

  test('首页满页但取末页失败：容忍回落，截断提示按已知值计，不抛错', async () => {
    const firstPage = Array.from({ length: 100 }, (_, i) => makeFile(i));
    const listFiles = jest.fn()
      .mockResolvedValueOnce({
        data: firstPage,
        headers: { link: '<https://api.github.com/x?page=2&per_page=100>; rel="next", <https://api.github.com/x?page=2&per_page=100>; rel="last"' }
      })
      .mockRejectedValueOnce(new Error('last page boom'));
    const octokit = { rest: { pulls: { listFiles } } };

    const result = await analyzeFileChanges(octokit, 'o', 'r', makePr(), buildConfig());

    // 至少已知 100 个（不是被截断的默认 30），不虚构
    expect(result).toContain('共100个文件，仅显示前5个');
  });

  test('getTotalFileCount 语义由 getLastPageNumber 承载：Link 头参数顺序无关', () => {
    expect(getLastPageNumber({
      link: '<https://api.github.com/x?page=2&per_page=100>; rel="next", <https://api.github.com/x?page=3&per_page=100>; rel="last"'
    })).toBe(3);
    expect(getLastPageNumber({ link: '<https://api.github.com/x?page=2>; rel="prev"' })).toBe(1);
    expect(getLastPageNumber({})).toBe(1);
    expect(getLastPageNumber(undefined)).toBe(1);
  });

  test('listFiles 失败：返回「无法获取」占位（fail-soft 不变）', async () => {
    const listFiles = jest.fn().mockRejectedValue(new Error('boom'));
    const octokit = { rest: { pulls: { listFiles } } };

    const result = await analyzeFileChanges(octokit, 'o', 'r', makePr(), buildConfig());

    expect(result).toBe(baseConfig.logging.file_changes_unavailable);
  });
});
