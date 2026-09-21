const core = require('@actions/core');
const { logMessage } = require('../utils/helpers');
const { callAIStructured } = require('./ai');
const { PR_REVIEW_DECISIONS, GOVERNANCE_DEFAULTS } = require('../utils/constants');
const githubOps = require('./github');

/**
 * PR 历史语境评审服务 —— 「这个 PR 是否该被关掉」的第三层判定。
 *
 * 与上游垃圾检测、canonical 关联治理的关系：
 *   上游垃圾/恶意/trivial 检测仍是最先执行的关闭路径（不变）；
 *   本服务插在既有 PR 治理（提炼 + canonical 关联）之前，回答一个更具体的问题：
 *   「历史上与这个主题相关的 issue 已经得出了什么结论？这个 PR 是满足了那些结论，还是重复了
 *   已被拒绝/已完成/已合并的工作？」
 *
 * 三段式：
 *   1. FETCH（脚本负责）：PR 自身（标题/正文/文件/提交）+ 相关历史 issue（canonical 标签检索 +
 *      文本检索）→ 每条相关 issue 读全文 + 评论 + 时间线，还原「为什么被关」；
 *      压缩成结构化「语境包」（截断规则复用 canonicalBodyTruncate 口径）。
 *   2. AI：对语境包做判定，返回 CLOSE / KEEP / UNCERTAIN + 证据条目（必须引用真实 issue 编号）。
 *   3. ACTION：CLOSE → 生成中文评审评论（🤖 前缀 + 操作日志行）→ 先评论后关闭；
 *      关闭失败容忍（warning 留痕）。不创建任何 canonical issue。
 *
 * 安全阀（宁可漏判、不可误关）：
 *   - 相关历史 issue 为空 → 直接回落旧关联链路，不评审（无历史语境可依）；
 *   - AI 判 UNCERTAIN / 证据不足 / 输出无法解析 → 回落旧关联链路；
 *   - 确定性证据闸门：AI 引用的 issue 编号必须真实存在于语境包（防幻觉引用），否则视为证据不足回落；
 *   - AI 调用抛错 → 向上抛，由 handler 走既有 fail-open 兜底；
 *   - dry-run → 只评论「本应关闭」，不执行关闭。
 */
class PrReviewService {
  /**
   * @param {Object} openai OpenAI 客户端
   * @param {string} aiModel 模型名
   * @param {Object} config 合并后的配置
   * @param {Object} gov 治理参数（prReviewClose / maxRelatedIssues / relatedCommentsPerIssue /
   *   relatedBodyTruncate / dryRun）
   * @param {Object} ops GitHub 操作集合（默认 src/services/github.js，测试时注入 mock）
   */
  constructor(openai, aiModel, config, gov = {}, ops = githubOps) {
    this.openai = openai;
    this.aiModel = aiModel;
    this.config = config;
    this.gov = { ...GOVERNANCE_DEFAULTS, ...gov };
    this.ops = ops;
  }

  /**
   * 主入口：收集语境 → AI 评审 → 依判定执行。
   * 返回值约定：null = 未触发/证据不足（调用方回落旧关联链路）；
   *   其余为 { decision, closed } 等（已处理完毕，调用方不必再走旧链路）。
   * @param {Object} octokit
   * @param {string} owner
   * @param {string} repo
   * @param {Object} pr 结构含 number/title/body/user.login
   * @param {string} fileChanges 已组装的文件变更描述（上游 analyzeFileChanges 产物，直接复用）
   */
  async review(octokit, owner, repo, pr, fileChanges = '') {
    const { number } = pr;
    core.info(logMessage(this.config.logging.pr_review_start, { number }));

    // 1. FETCH：相关历史 issue（canonical 标签 + 文本检索双通道，取并集去重）
    let related = [];
    try {
      related = await this.collectRelatedIssues(octokit, owner, repo, pr);
    } catch (error) {
      // 检索失败视为「无历史语境」，回落旧链路 —— 不让检索故障演变成误关
      core.warning(logMessage(this.config.logging.pr_review_fetch_failed, { number, error: error.message }));
      return null;
    }

    if (related.length === 0) {
      core.info(logMessage(this.config.logging.pr_review_no_related, { number }));
      return null;
    }

    // 2. FETCH：为每条相关 issue 补全全文 + 评论 + 时间线（失败容忍，跳过该条继续）
    const relatedWithDetail = await this.enrichRelatedIssues(octokit, owner, repo, related);
    if (relatedWithDetail.length === 0) {
      core.info(logMessage(this.config.logging.pr_review_no_related, { number }));
      return null;
    }

    // 3. AI 评审（抛错向上传导 → handler fail-open）
    const contextPackage = await this.buildContextPackage(octokit, owner, repo, pr, fileChanges, relatedWithDetail);
    const verdict = await this.judge(contextPackage);

    // 4. 确定性证据闸门：只有 CLOSE 且证据引用了真实 issue 编号才允许关闭
    const verified = this.verifyEvidence(verdict, relatedWithDetail);
    core.info(logMessage(this.config.logging.pr_review_result, {
      number,
      result: verdict.decision + (verified ? '' : '（证据未通过校验，回落）')
    }));

    if (verdict.decision !== PR_REVIEW_DECISIONS.CLOSE || !verified) {
      return null; // KEEP / UNCERTAIN / 证据不足 → 回落旧关联链路
    }

    // 5. ACTION：生成评审评论 → 先评论后关闭；不创建 canonical
    return await this.executeClose(octokit, owner, repo, pr, verdict, relatedWithDetail);
  }

  /**
   * 双通道检索相关历史 issue：
   *   a) canonical 标签过滤（label:"canonical"，含已关闭的 —— 历史结论大多在关闭的 canonical 里）；
   *   b) 标题关键词文本检索已关闭 issue（覆盖没打 canonical 但有结论的场景，如 wontfix）。
   * 并集去重后截取 maxRelatedIssues 条（canonical 优先 —— 它们是治理体系的一手结论）。
   * @returns {Promise<Array>} [{ number, title, body, state, state_reason, closed_at }]
   */
  async collectRelatedIssues(octokit, owner, repo, pr) {
    const candidates = new Map(); // number → item

    // 通道 a：canonical 标签（复用 listCanonicalIssues 的检索口径）
    try {
      core.info(logMessage(this.config.logging.pr_review_fetch_canonical, { label: this.gov.canonicalLabel }));
      const canonical = await this.ops.listCanonicalIssues(
        octokit,
        owner,
        repo,
        this.gov.canonicalLabel,
        this.gov.maxCanonicalIndex,
        this.gov.relatedBodyTruncate,
        true
      );
      canonical.forEach(item => {
        if (item.state !== 'closed') {
          return; // 只关心已得出结论的历史 issue
        }
        candidates.set(item.number, {
          number: item.number,
          title: item.title,
          body: item.body || '',
          state: 'closed',
          state_reason: null,
          closed_at: null,
          source: 'canonical'
        });
      });
    } catch (error) {
      core.warning(logMessage(this.config.logging.pr_review_canonical_failed, { error: error.message }));
    }

    // 通道 b：标题关键词文本检索已关闭 issue
    try {
      const keywords = this.extractKeywords(pr);
      if (keywords.length > 0) {
        const textHits = await this.ops.searchRelatedClosedIssues(
          octokit,
          owner,
          repo,
          keywords,
          this.gov.maxRelatedIssues * 2
        );
        textHits.forEach(item => {
          if (item.is_pr || candidates.has(item.number)) {
            return;
          }
          candidates.set(item.number, {
            number: item.number,
            title: item.title,
            state: item.state,
            state_reason: item.state_reason,
            closed_at: item.closed_at,
            source: 'search'
          });
        });
      }
    } catch (error) {
      core.warning(logMessage(this.config.logging.pr_review_search_failed, { error: error.message }));
    }

    // canonical 优先排序后截断
    const merged = [...candidates.values()];
    merged.sort((a, b) => (a.source === 'canonical' ? 0 : 1) - (b.source === 'canonical' ? 0 : 1));
    return merged.slice(0, this.gov.maxRelatedIssues);
  }

  /**
   * 从 PR 标题/正文提取检索关键词：标题按分隔符切词 + 正文取前几个有意义的词组。
   * 纯启发式，目标是让 search API 命中同主题的历史 issue，不追求精确。
   */
  extractKeywords(pr) {
    const words = new Set();
    const title = String(pr.title || '');
    // Conventional Commits 前缀对检索无意义，剥离
    const bareTitle = title.replace(/^\w+(\(.+?\))?:\s*/, '');
    bareTitle.split(/[\s,，。:：/|()（）\-_]+/).forEach(w => {
      const t = w.trim();
      if (t.length > 1) {
        words.add(t);
      }
    });
    return [...words].slice(0, 4);
  }

  /**
   * 为每条相关 issue 补全：全文 + 全部评论 + 时间线事件。
   * 单条补全失败容忍（跳过该条），全失败则返回空（调用方回落）。
   */
  async enrichRelatedIssues(octokit, owner, repo, related) {
    const enriched = [];
    for (const item of related) {
      try {
        const [comments, timeline] = await Promise.all([
          this.ops.listIssueComments(octokit, owner, repo, item.number, this.gov.relatedCommentsPerIssue),
          this.ops.listIssueTimeline(octokit, owner, repo, item.number, 30)
        ]);
        enriched.push({
          ...item,
          comments,
          timeline
        });
      } catch (error) {
        core.warning(logMessage(this.config.logging.pr_review_enrich_failed, { number: item.number, error: error.message }));
      }
    }
    return enriched;
  }

  /**
   * 组装结构化「语境包」喂给 AI（脚本负责取数，AI 只做判断 —— 它无网络、无仓库浏览能力）。
   * 截断规则：正文复用 relatedBodyTruncate 口径；评论每条截断；时间线只保留事件摘要。
   */
  async buildContextPackage(octokit, owner, repo, pr, fileChanges, related) {
    let commits = [];
    try {
      commits = await this.ops.listPRCommits(octokit, owner, repo, pr.number, 20);
    } catch (error) {
      core.warning(logMessage(this.config.logging.pr_review_commits_failed, { error: error.message }));
    }

    const truncate = (text, limit) => {
      const s = String(text || '');
      return s.length > limit ? `${s.slice(0, limit)}…(截断)` : s;
    };

    return {
      pr: {
        number: pr.number,
        title: pr.title,
        body: truncate(pr.body, this.gov.relatedBodyTruncate),
        author: pr.user ? pr.user.login : 'unknown',
        files_changed: truncate(fileChanges, this.gov.relatedBodyTruncate),
        commits: commits.map(c => truncate(c.message, 200))
      },
      related_issues: related.map(item => ({
        number: item.number,
        title: item.title,
        state: item.state,
        state_reason: item.state_reason,
        body: truncate(item.body, this.gov.relatedBodyTruncate),
        comments: (item.comments || []).map(c => ({
          author: c.author,
          body: truncate(c.body, 500)
        })),
        timeline: (item.timeline || [])
          .filter(e => ['closed', 'reopened', 'merged', 'referenced', 'cross-referenced', 'locked', 'unlocked'].includes(e.event))
          .map(e => ({
            event: e.event,
            actor: e.actor,
            commit_id: e.commit_id
          }))
      }))
    };
  }

  /**
   * AI 评审：判定 PR 是否与历史结论冲突/重复/已被覆盖。
   * 返回 { decision, reasons, evidence }；输出不可解析时返回 UNCERTAIN（回落）。
   */
  async judge(contextPackage) {
    const request = {
      instructions: this.config.prompts.pr_historical_review,
      input: JSON.stringify(contextPackage)
    };
    const result = await callAIStructured(
      this.openai,
      this.aiModel,
      request,
      this.config,
      'PR 历史语境评审',
      { decision: 'UNCERTAIN', reasons: [], evidence: [] }
    );
    if (!result) {
      return { decision: PR_REVIEW_DECISIONS.UNCERTAIN, reasons: [], evidence: [] };
    }
    const decision = String(result.decision || '').trim().toUpperCase();
    return {
      decision: [PR_REVIEW_DECISIONS.CLOSE, PR_REVIEW_DECISIONS.KEEP, PR_REVIEW_DECISIONS.UNCERTAIN].includes(decision)
        ? decision
        : PR_REVIEW_DECISIONS.UNCERTAIN,
      reasons: Array.isArray(result.reasons) ? result.reasons.map(String) : [],
      evidence: Array.isArray(result.evidence) ? result.evidence.map(String) : []
    };
  }

  /**
   * 确定性证据闸门：AI 给出的 evidence 必须引用语境包里真实存在的 issue 编号。
   * 防止模型幻觉编造「历史 issue #999 说过…」式证据骗过关闭动作。
   * KEEP / UNCERTAIN 不需要过闸门（它们本来就不关 PR）。
   */
  verifyEvidence(verdict, related) {
    if (verdict.decision !== PR_REVIEW_DECISIONS.CLOSE) {
      return true;
    }
    const realNumbers = new Set(related.map(item => item.number));
    const cited = verdict.evidence
      .map(e => (String(e).match(/#(\d+)/g) || [])
        .map(m => parseInt(m.slice(1), 10)))
      .flat();
    const valid = cited.filter(n => realNumbers.has(n));
    if (valid.length === 0) {
      core.warning(this.config.logging.pr_review_evidence_rejected);
      return false;
    }
    return true;
  }

  /**
   * 执行关闭：生成中文评审评论 → 先评论后关闭（与 issueGovernanceService 的安全写序一致）。
   * 关闭失败容忍（warning 留痕，不重试不回滚）。不创建 canonical、不打 duplicate 标签。
   */
  async executeClose(octokit, owner, repo, pr, verdict, related) {
    const { number } = pr;

    let comment;
    try {
      comment = await this.draftReviewComment(pr, verdict, related);
    } catch (error) {
      // 评论草稿失败 → 视为证据链不完整，宁可漏判不可误关，回落旧链路
      core.warning(logMessage(this.config.logging.pr_review_comment_failed, { number, error: error.message }));
      return null;
    }

    if (this.gov.dryRun) {
      const intro = this.config.responses.governance_dry_run;
      const body = `${intro}\n\n**本应执行**：关闭 PR（历史语境评审判定 CLOSE，证据已校验）\n\n---\n\n${comment}`;
      await this.safeComment(octokit, owner, repo, number, body);
      return { decision: PR_REVIEW_DECISIONS.CLOSE, dryRun: true };
    }

    // 先评论，后关闭
    await this.safeComment(octokit, owner, repo, number, comment);

    try {
      await this.ops.updatePullRequest(octokit, owner, repo, number, { state: 'closed' });
      core.info(logMessage(this.config.logging.pr_review_closed, { number }));
    } catch (error) {
      // 关闭失败容忍：评论已发出，留 warning，不重试不回滚
      core.warning(logMessage(this.config.logging.pr_review_close_failed, { number, error: error.message }));
    }

    return { decision: PR_REVIEW_DECISIONS.CLOSE, closed: true };
  }

  /**
   * 生成中文评审评论（🤖 前缀 + 尾部操作日志行，与 issueGovernanceService 同约定）。
   * 输入全部是不可信数据，AI 只做「重述 + 评价」，引用的 issue 编号已在 verifyEvidence 校验过真实性。
   * 评论草稿是多段结构化中文，逐字用默认 max_tokens（1000）容易截断 JSON 导致解析失败，
   * 这里局部放宽到至少 2000 —— 只影响本次调用，不动全局配置。
   */
  async draftReviewComment(pr, verdict, related) {
    const request = {
      instructions: this.config.prompts.pr_review_comment,
      input: JSON.stringify({
        pr_number: pr.number,
        author: pr.user ? pr.user.login : 'unknown',
        decision: verdict.decision,
        reasons: verdict.reasons,
        evidence: verdict.evidence,
        related_issues: related.map(item => ({ number: item.number, title: item.title, state_reason: item.state_reason }))
      })
    };
    const configForDraft = {
      ...this.config,
      ai_settings: {
        ...this.config.ai_settings,
        max_tokens: Math.max(this.config.ai_settings.max_tokens || 0, 2000)
      }
    };
    const raw = await callAIStructured(
      this.openai,
      this.aiModel,
      request,
      configForDraft,
      'PR 评审评论起草',
      { comment: '' }
    );
    const text = String((raw && raw.comment) || '').trim();
    if (!text) {
      throw new Error('评审评论草稿为空');
    }
    // 机器人身份标记由服务端确定性补上（不依赖模型自觉），尾部附操作日志行
    const prefixed = text.startsWith('🤖') ? text : `🤖 ${text}`;
    return `${prefixed}\n\n${logMessage(this.config.responses.governance_log_prefix, { action: 'pr_historical_review_close' })}`;
  }

  async safeComment(octokit, owner, repo, number, body) {
    try {
      await this.ops.addComment(octokit, owner, repo, number, body, this.config.logging.governance_comment_failed);
    } catch (error) {
      core.warning(logMessage(this.config.logging.governance_comment_failed, { error: error.message }));
    }
  }
}

module.exports = PrReviewService;
module.exports.PR_REVIEW_DECISIONS = PR_REVIEW_DECISIONS;
