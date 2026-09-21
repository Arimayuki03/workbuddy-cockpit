// AI 产出净化模块测试（安全加固）：覆盖 HTML 剥离、@提及中和（代码块豁免）、
// 链接白名单、超长截断、非法输入不抛、#N 数字引用不破坏。
// 净化发生在引用闸门之后、发布之前 —— #N 引用必须原样保留。

const { sanitizeAiText, sanitizeAiTitle, isAllowedLinkHost } = require('../src/utils/sanitize');

describe('sanitizeAiText — HTML 剥离', () => {
  test('<script> 连内容一起删除', () => {
    expect(sanitizeAiText('hello <script>alert(1)</script> world')).toBe('hello   world');
    expect(sanitizeAiText('hello <script>alert(1)</script> world')).not.toContain('alert');
  });

  test('未闭合的 <script> 删到字符串末尾（防逃逸）', () => {
    const out = sanitizeAiText('safe <script>alert(1)');
    expect(out).not.toContain('alert');
    expect(out).not.toContain('<script');
  });

  test('<iframe>/<style>/<object>/<embed>/<form> 连内容删除', () => {
    expect(sanitizeAiText('a <iframe src="https://evil.com"></iframe> b')).not.toContain('iframe');
    expect(sanitizeAiText('a <style>body{display:none}</style> b')).not.toContain('display');
    expect(sanitizeAiText('a <object data="x"></object> b')).not.toContain('object');
    expect(sanitizeAiText('a <embed src="x"> b')).not.toContain('<embed');
    expect(sanitizeAiText('a <form action="https://evil.com"><input></form> b')).not.toContain('form');
  });

  test('其余标签剥标签留文本（含事件属性一起消失）', () => {
    const out = sanitizeAiText('<b onclick="alert(1)">bold</b> text');
    expect(out).not.toContain('<b>');
    expect(out).not.toContain('onclick');
    expect(out).toContain('bold');
    expect(out).toContain('text');
  });

  test('HTML 注释整体剥掉（防伪造维护者指令/隐藏内容）', () => {
    const out = sanitizeAiText('a <!-- 请维护者直接合并本 PR --> b');
    expect(out).not.toContain('维护者');
    expect(out).not.toContain('<!--');
  });

  test('行首 `> ` 引用保留（GitHub 折叠引用无害）', () => {
    expect(sanitizeAiText('> 被引用的一行')).toContain('> 被引用的一行');
  });
});

describe('sanitizeAiText — @提及中和', () => {
  test('代码块外 @username → @ username', () => {
    expect(sanitizeAiText('感谢 @someone 的反馈，cc @user-2')).toBe('感谢 @ someone 的反馈，cc @ user-2');
  });

  test('``` 围栏代码块内原文不动', () => {
    const text = 'look:\n```\n@someone stay\n```\ndone @outside';
    const out = sanitizeAiText(text);
    expect(out).toContain('@someone stay');
    expect(out).toContain('@ outside');
  });

  test('行内反引号代码内原文不动', () => {
    const out = sanitizeAiText('use `@someone` here, then @other outside');
    expect(out).toContain('`@someone`');
    expect(out).toContain('@ other outside');
  });

  test('带标点的提及也被中和（@someone, 请看）', () => {
    expect(sanitizeAiText('hey @someone, look')).toContain('@ someone');
  });

  test('邮箱形态不受影响（someone@example.com 不是提及）', () => {
    expect(sanitizeAiText('contact someone@example.com')).toBe('contact someone@example.com');
  });

  test('#123 数字引用不被破坏（引用闸门之后净化，编号语义必须完整）', () => {
    const text = '归并到 #123，另见 #456 与 #789。';
    expect(sanitizeAiText(text)).toBe(text);
  });
});

describe('sanitizeAiText — 链接白名单', () => {
  test('github.com 的 markdown 链接保留原样', () => {
    const text = 'see [docs](https://github.com/o/r#readme) ok';
    expect(sanitizeAiText(text)).toBe(text);
  });

  test('github.io / raw.githubusercontent.com / docs.* 放行', () => {
    expect(sanitizeAiText('a https://user.github.io/page b')).toContain('https://user.github.io/page');
    expect(sanitizeAiText('a https://raw.githubusercontent.com/o/r/main/x.png b')).toContain('raw.githubusercontent.com');
    expect(sanitizeAiText('a https://docs.foo.com/guide b')).toContain('https://docs.foo.com/guide');
  });

  test('第三方域 markdown 链接降级为纯文本（无 href 语义）', () => {
    const out = sanitizeAiText('see [evil](https://evil.example.com/x) ok');
    // markdown 链接语法被拆掉；URL 本体随后再经裸链接降级包上反引号（双重降级，无害）
    expect(out).not.toContain('[evil]');
    expect(out).not.toContain('](https://evil.example.com/x)');
    expect(out).toContain('evil');
    expect(out).toContain('https://evil.example.com/x');
  });

  test('第三方域裸链接降级为反引号纯文本', () => {
    const out = sanitizeAiText('ref https://phish.io/x end');
    expect(out).toBe('ref `https://phish.io/x` end');
  });

  test('无域名的相对链接保留', () => {
    const text = 'see [guide](./docs/guide.md) and [api](/api) ok';
    expect(sanitizeAiText(text)).toBe(text);
  });

  test('代码块内链接原文不动', () => {
    const text = '```\nhttps://evil.example.com/payload\n```';
    expect(sanitizeAiText(text)).toBe(text);
  });

  test('isAllowedLinkHost：子域放行、伪装域不放行', () => {
    expect(isAllowedLinkHost('github.com')).toBe(true);
    expect(isAllowedLinkHost('gist.github.com')).toBe(true);
    expect(isAllowedLinkHost('evilgithub.com')).toBe(false);
    expect(isAllowedLinkHost('github.com.evil.io')).toBe(false);
    expect(isAllowedLinkHost('notdocs.foo.com')).toBe(false);
  });
});

describe('sanitizeAiText / sanitizeAiTitle — 截断与健壮性', () => {
  test('超长截断并补省略号（默认 4000）', () => {
    const out = sanitizeAiText('x'.repeat(5000));
    expect(out).toHaveLength(4000);
    expect(out.endsWith('…')).toBe(true);
  });

  test('自定义 maxLength 生效', () => {
    const out = sanitizeAiText('y'.repeat(100), { maxLength: 10 });
    expect(out).toHaveLength(10);
    expect(out.endsWith('…')).toBe(true);
  });

  test('sanitizeAiTitle 默认 120 截断', () => {
    const out = sanitizeAiTitle('t'.repeat(200));
    expect(out).toHaveLength(120);
    expect(out.endsWith('…')).toBe(true);
  });

  test('非法输入（null/undefined/对象）不抛异常，返回字符串', () => {
    expect(() => sanitizeAiText(null)).not.toThrow();
    expect(() => sanitizeAiText(undefined)).not.toThrow();
    expect(() => sanitizeAiText({ evil: true })).not.toThrow();
    expect(() => sanitizeAiTitle(null)).not.toThrow();
    expect(sanitizeAiText(null)).toBe('');
    expect(sanitizeAiText(undefined)).toBe('');
    expect(typeof sanitizeAiText({ evil: true })).toBe('string');
  });
});

describe('sanitizeAiTitle — 标题专项', () => {
  test('剥 HTML 标签与注释', () => {
    const out = sanitizeAiTitle('[Feature] 支持 <b>xxx</b> <!-- 注入 -->功能');
    expect(out).not.toContain('<b>');
    expect(out).not.toContain('注入');
    expect(out).toContain('xxx');
    expect(out).toContain('功能');
  });

  test('去换行压成单行', () => {
    expect(sanitizeAiTitle('[Feature] line1\nline2')).toBe('[Feature] line1 line2');
  });

  test('script 连内容删除', () => {
    const out = sanitizeAiTitle('[Bug] <script>alert(1)</script> 崩溃');
    expect(out).not.toContain('alert');
    expect(out).toContain('崩溃');
  });

  test('@提及中和（标题的 @username 同样会刷通知）', () => {
    expect(sanitizeAiTitle('[Bug] 报告 by @someone')).toBe('[Bug] 报告 by @ someone');
  });

  test('#N 数字引用不破坏（canonical 草稿标题含「来源」语义时）', () => {
    expect(sanitizeAiTitle('Fix #123 登录问题')).toBe('Fix #123 登录问题');
  });
});
