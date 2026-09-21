/**
 * AI 产出净化模块（安全加固）。
 *
 * 背景：LLM 基于不可信的 issue/PR 内容生成评论文本、canonical 标题/正文与 PR 评审评论。
 * 现有确定性闸门只校验 #N 引用编号真实性，不限制正文内容。攻击者可通过 prompt 注入
 * 驱动机器人发布任意 markdown（@提及刷通知、钓鱼链接、伪造维护者指令），
 * 注入内容沉淀为 canonical 后还会进入归并语料自放大。本模块在「引用闸门之后、发布之前」
 * 对 AI 生成的段落做统一净化。
 *
 * 关键策略选择（简单可靠优先）：
 *   - HTML：整体剥离标签、保留内部文本（不整体转义、不做白名单渲染）。
 *     危险标签（script/iframe/style/object/embed/form 等）连内容一起删除，
 *     其余标签只删标签本身。理由：GitHub 评论只支持部分 HTML，转义会把合法标记
 *     显示成乱码；剥标签能同时消灭事件属性（onclick=）与 <script> 注入，实现简单可测。
 *   - @提及：代码块外 `@username` → `@ username`，防止机器人评论制造通知骚扰。
 *   - 链接：markdown 链接与裸 URL 中非白名单域降级为纯文本；相对链接保留。
 *   - HTML 注释（<!-- xxx -->）剥掉，防伪造维护者指令/折叠块；行首 `> ` 引用保留
 *     （GitHub 折叠引用无害）。
 *   - `#123` 数字引用不做任何改动（净化发生在引用闸门校验之后，不得破坏闸门已认可的编号）。
 *
 * 输出保证：永远是合法字符串、无异常抛出（try/catch 兜底返回截断后的纯文本）。
 */

// 评论与标题的默认长度上限
const DEFAULT_COMMENT_MAX_LENGTH = 4000;
const DEFAULT_TITLE_MAX_LENGTH = 120;

// 链接白名单：github.com / github.io / raw.githubusercontent.com（允许子域）+ docs.*（一级 docs. 前缀通配）
// 匹配输入：host 恰为白名单项或其子域（*.github.io 等）；docs.foo.com 由 DOCS_PREFIX_RULE 单独放行
const LINK_HOST_ALLOWLIST = [
  'github.com',
  'github.io',
  'raw.githubusercontent.com',
  'docs.github.com'
];

// 危险标签：与其内部内容一起删除（防脚本注入 / 钓鱼表单 / 样式隐藏）
const STRIP_WITH_CONTENT_TAGS = ['script', 'iframe', 'style', 'object', 'embed', 'form'];
// 其余 HTML 标签：只剥标签本身、保留内部文本（含开闭标签与自闭合标签）
const ANY_TAG_PATTERN = /<\/?[a-zA-Z][^>]*>/g;
// HTML 注释整体剥掉：防伪造「维护者指令」与隐藏内容
const HTML_COMMENT_PATTERN = /<!--[\s\S]*?-->/g;

/**
 * 按 host 判断是否在链接白名单内。
 * - github.com / github.io / raw.githubusercontent.com：host 恰为白名单项或其子域；
 * - docs.*：docs. 前缀通配（docs.foo.com 放行，docs.github.com 亦在其中）。
 * @param {string} host 已转小写的 URL host（可能带端口）
 * @returns {boolean}
 */
function isAllowedLinkHost(host) {
  if (!host) {
    return false;
  }
  const bare = host.replace(/:\d+$/, '');
  if (/^docs\.[A-Za-z0-9.-]+$/.test(bare)) {
    return true; // docs.* 通配
  }
  return LINK_HOST_ALLOWLIST.some(allowed =>
    bare === allowed || bare.endsWith(`.${allowed}`)
  );
}

/**
 * 截断到 maxLength，截断处补省略号「…」。
 * 非字符串输入先转字符串；非法/空输入返回空串。
 */
function truncate(text, maxLength) {
  const s = String(text ?? '');
  const limit = Number.isFinite(maxLength) && maxLength > 0 ? maxLength : Infinity;
  if (s.length <= limit) {
    return s;
  }
  // 至少保留 1 个字符 + 省略号，保证总长不超过 maxLength
  return `${s.slice(0, Math.max(1, limit - 1))}…`;
}

/**
 * 链接白名单过滤：非白名单域的链接去掉链接语义、降级为纯文本。
 * - `[text](url)` → text 原样保留；白名单/相对链接保留原 markdown；
 *   非白名单时降级为 `text (url)` 纯文本（不产生 <a>）
 * - `https?://...` 裸链接 → 白名单保留原样；否则降级为反引号包裹的纯文本（GitHub 不渲染链接）
 */
function neutralizeLinks(text) {
  let out = text;

  // 1) markdown 链接 [text](url)
  out = out.replace(/\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)/g, (match, label, url) => {
    // 相对链接（无协议、无合法 host）保留；白名单域保留原 markdown
    if (!hasHost(url) || isAllowedLinkHost(parseUrl(url).host)) {
      return match;
    }
    return `${label} (${url})`;
  });

  // 2) 裸 URL（https?:// 开头，非空白且非右括号字符）
  out = out.replace(/https?:\/\/[^\s<>)\]]+/g, (match) => {
    if (isAllowedLinkHost(parseUrl(match).host)) {
      return match; // 白名单域保留链接语义
    }
    return `\`${match}\``; // 非白名单域：反引号包裹，GitHub 不渲染 <a>
  });

  return out;
}

/**
 * 判断 URL 字符串是否带 host（http(s):// 或协议相对 //host）。
 * 相对路径（./x、/x、x.md）没有 host，返回 false。
 */
function hasHost(url) {
  return /^https?:\/\//i.test(String(url || '')) || /^\/\//.test(String(url || ''));
}

/**
 * 安全解析 URL：非法输入返回 null（不抛异常）。
 */
function parseUrl(url) {
  try {
    // 补协议以便解析 //host 与相对链接的形态判断
    return new URL(String(url), 'https://github.invalid');
  } catch (_error) {
    return null;
  }
}

/**
 * @提及中和：代码块外 `@username` → `@ username`（空格隔开）。
 * GitHub 只在 @ 紧贴用户名时触发通知，加空格即失效。
 * @username 合法字符集与 GitHub 一致：字母数字与连字符（1-39 位），不再吃后续标点。
 */
function neutralizeMentions(text) {
  return text.replace(/(^|[^\w`])@([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9]|[A-Za-z0-9])?)\b/g,
    (match, prefix, username) => `${prefix}@ ${username}`);
}

/**
 * 主净化入口（评论正文）。流程：
 *   1) 剥 HTML 注释 → 2) 剥危险标签连内容 → 3) 剥其余标签留文本
 *   → 4) 分离代码块，仅对代码块外文本做 @提及中和与链接白名单
 *   → 5) 截断。
 * 任何一步异常都由外层 try/catch 兜底，返回截断后的纯文本，绝不抛出。
 * @param {string} text AI 生成的评论文本
 * @param {{maxLength?: number}} options maxLength 默认 4000
 * @returns {string}
 */
function sanitizeAiText(text, options = {}) {
  try {
    const maxLength = options.maxLength === undefined ? DEFAULT_COMMENT_MAX_LENGTH : options.maxLength;
    const raw = String(text ?? '');

    // 1) HTML 注释整体剥掉
    let working = raw.replace(HTML_COMMENT_PATTERN, ' ');

    // 2) 危险标签连内容一起删（script/iframe/style/object/embed/form）
    for (const tag of STRIP_WITH_CONTENT_TAGS) {
      working = working.replace(
        new RegExp(`<${tag}(\\s[^>]*)?>[\\s\\S]*?<\\/${tag}\\s*>`, 'gi'),
        ' '
      );
      // 未闭合的危险标签：删到字符串末尾（防 <script> 未闭合逃逸）
      working = working.replace(new RegExp(`<${tag}(\\s[^>]*)?>[\\s\\S]*$`, 'i'), ' ');
    }

    // 3) 其余 HTML 标签：剥标签、留文本
    working = working.replace(ANY_TAG_PATTERN, ' ');

    // 4) 分离代码块（``` 围栏与行内 `），代码块内原文不动
    const segments = splitCodeSegments(working);
    const sanitized = segments
      .map(segment => segment.isCode
        ? segment.text
        : neutralizeMentions(neutralizeLinks(segment.text)))
      .join('');

    // 5) 截断（含省略号）
    return truncate(sanitized, maxLength);
  } catch (_error) {
    // 兜底：任何意外异常都退化为「转字符串 + 截断」，绝不抛出
    return truncate(String(text ?? ''), options.maxLength === undefined ? DEFAULT_COMMENT_MAX_LENGTH : options.maxLength);
  }
}

/**
 * 标题净化入口：AI 起草的 canonical / PR 标题。
 * 标题是单行纯文本（GitHub title 字段不渲染 markdown/HTML），但 @提及 仍会触发通知，
 * 因此做：剥 HTML 标签与注释、去换行（标题不允许换行）、@提及中和、截断。
 * 链接白名单不做（剥标签后标题里的 URL 已无 href 语义）。
 * @param {string} text AI 生成的标题
 * @param {{maxLength?: number}} options maxLength 默认 120
 * @returns {string}
 */
function sanitizeAiTitle(text, options = {}) {
  try {
    const maxLength = options.maxLength === undefined ? DEFAULT_TITLE_MAX_LENGTH : options.maxLength;
    const raw = String(text ?? '');

    // 剥 HTML 注释与标签（标题不区分危险标签，一律剥标签留文本）
    let working = raw.replace(HTML_COMMENT_PATTERN, ' ');
    for (const tag of STRIP_WITH_CONTENT_TAGS) {
      working = working.replace(new RegExp(`<${tag}(\\s[^>]*)?>[\\s\\S]*?<\\/${tag}\\s*>`, 'gi'), ' ');
      working = working.replace(new RegExp(`<${tag}(\\s[^>]*)?>[\\s\\S]*$`, 'i'), ' ');
    }
    working = working.replace(ANY_TAG_PATTERN, ' ');

    // 标题不允许换行：压成单行空白
    working = working.replace(/\s*[\r\n]+\s*/g, ' ').trim();

    // @提及中和（标题的 @username 同样会刷通知）
    return truncate(neutralizeMentions(working), maxLength);
  } catch (_error) {
    return truncate(String(text ?? ''), options.maxLength === undefined ? DEFAULT_TITLE_MAX_LENGTH : options.maxLength);
  }
}

/**
 * 把文本切成「代码段 / 普通段」序列。
 * ``` 围栏块（含围栏行本身）与成对行内 ` 反引号内容为代码段，原文不动；
 * 未闭合的行内反引号按普通文本处理（成对才视为代码）。
 * @returns {Array<{isCode: boolean, text: string}>}
 */
function splitCodeSegments(text) {
  const segments = [];
  // 围栏块：``` 开头到下一个 ```（或字符串末尾——未闭合围栏视为代码到末尾，避免泄漏内容）
  const fencePattern = /```[\s\S]*?(?:```|$)/g;
  let lastIndex = 0;
  let match;
  while ((match = fencePattern.exec(text)) !== null) {
    if (match.index > lastIndex) {
      segments.push({ isCode: false, text: text.slice(lastIndex, match.index) });
    }
    segments.push({ isCode: true, text: match[0] });
    lastIndex = match.index + match[0].length;
  }
  if (lastIndex < text.length) {
    segments.push({ isCode: false, text: text.slice(lastIndex) });
  }

  // 行内反引号：把普通段中成对的 `...` 再切出来
  const result = [];
  for (const segment of segments) {
    if (segment.isCode) {
      result.push(segment);
      continue;
    }
    let rest = segment.text;
    while (rest) {
      const open = rest.indexOf('`');
      if (open === -1) {
        result.push({ isCode: false, text: rest });
        break;
      }
      const close = rest.indexOf('`', open + 1);
      if (close === -1) {
        // 未闭合：整体按普通文本处理
        result.push({ isCode: false, text: rest });
        break;
      }
      if (open > 0) {
        result.push({ isCode: false, text: rest.slice(0, open) });
      }
      result.push({ isCode: true, text: rest.slice(open, close + 1) });
      rest = rest.slice(close + 1);
    }
  }
  return result;
}

module.exports = {
  sanitizeAiText,
  sanitizeAiTitle,
  // 导出供测试与未来扩展使用
  truncate,
  isAllowedLinkHost
};
