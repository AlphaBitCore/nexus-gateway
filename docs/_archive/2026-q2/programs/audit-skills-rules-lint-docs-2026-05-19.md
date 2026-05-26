# 审计决策日志：skills / cursor rules / lint / 架构文档与代码库路径一致性

- **日期**：2026-05-19
- **执行人**：Claude Opus 4.7（4 路并行 Explore agent 调研 + 2 路 Sonnet agent 批量修复 + 主会话决策）
- **范围**：`.claude/skills/`、`.cursor/rules/`、`scripts/check-*.sh` + `.coverage-allowlist`、`docs/dev/architecture/`、`docs/dev/architecture-doc-triggers.md`、`docs/dev/workflow/`、`docs/dev/README.md`、`CLAUDE.md` 自身、以及在 sweep 中浮出来的 10 处 Go 源码注释。
- **触发**：用户要求按"最新架构、文件路径"复核现有规范工件。

## 1. 审计方法

### 1.1 4 路并行 Explore agent 广度扫描

| Agent | 范围 | 关键约束传递 |
|---|---|---|
| skills | 全部 `.claude/skills/**/{SKILL.md,*.sh,*.py}` | 必须 `ls` 真实路径，禁止臆测 |
| cursor rules | 全部 `.cursor/rules/*.mdc` | 校验 frontmatter globs + 与 CLAUDE.md 绑定规则的冲突 |
| lint/scripts | `scripts/check-*.sh`、`.coverage-allowlist`、`package.json#scripts`、`.golangci.yml`、`.githooks/` | 校验每个 hard-coded 路径是否仍存在 |
| arch docs | `docs/dev/architecture/`、`-triggers`、`README.md`、`workflow/` | 排除 `docs/dev/_internal/` 历史快照、`docs/sdd/` 故事快照 |

### 1.2 深度复核（主会话）

agent 报告全部经过主会话二次确认，**驳回 1 处误报**：
- agent #1 称 `test-compliance-proxy` 行 47/376 含 `shared/compliance` —— 二次 grep 证实路径确实存在但行号有偏；以实际 grep 结果为准。

### 1.3 关键发现来源

- 在 agent #1 报告完成前，主会话独立做了"基础事实校验"`ls packages/shared/`，**意外发现 CLAUDE.md 自身错误**（声明 "7 buckets" 实际列出 9 个名字，其中 `compliance/` 不存在）。这成为后续所有修复的"真理之锚"。
- 在最终 sweep 阶段又发现 **10 处 Go 源码注释**含 `shared/security/`、`shared/runtime/` —— agent #4 只扫了 `docs/`，遗漏了源码注释。补救派遣第 6 路 sonnet agent 修复。

## 2. 关键发现汇总

### 2.1 真理之锚：`packages/shared/` 实际结构

经 `ls packages/shared/` 核实：**8 个桶**，子目录如下：

| Bucket | 实际子目录 |
|---|---|
| `audit/` | — |
| `core/` | `bootenv/` `diag/`（含 `runtimeintrospect/`）`logging/` `metrics/` `telemetry/` |
| `identity/` | `iam/` `pkce/` `rstokenauth/` |
| `policy/` | `decision/` `device/` `domain/` `hooks/` `payloadcapture/` `pipeline/`（前 `shared/compliance/` 决策引擎落地处）`rulepack/` |
| `schemas/` | `configkey/` `configtypes/`（含 enums/identity/interception/observability/policy 子树）`credstate/` `domain/` `thingtype/` |
| `storage/` | `cacheconfig/` `configcache/` `configstore/` `redisfactory/` `spillstore/` `spillupload/` |
| `traffic/` | — |
| `transport/` | `bufconn/` `configloader/` `http/` `mq/` `normalize/` `responseio/` `streaming/` `thingclient/` `tlsbump/` `wirerewrite/` |

### 2.2 三类历史重构遗留

P9 重构后**仓库内大量文件仍引用旧路径**：

1. `shared/security/` → `shared/identity/`：14 处文档 + 8 处 Go 注释
2. `shared/runtime/` → `shared/core/`：15 处文档 + 2 处 Go 注释
3. `shared/compliance/` → `shared/policy/pipeline/`：1 处文档 + 1 处 skill（含 brace 展开）
4. CP `internal/handler/<domain>/` → 分布式 domain 子包（`identity/`、`ai/`、`governance/`、`observability/`、`traffic/`）：17 处 `.coverage-allowlist` 条目失效
5. `tools/db-migrate/_archive/`、`docs/dev/_archive/` 已删：但 cursor rule + check 脚本注释 + `docs/dev/README.md` 仍引用
6. 工作区目录已从 `nexus-gateway/` 改名到 `nexus-gateway-refactor/`：skills 中 2 处硬编码绝对路径未跟进
7. 测试 env 文件 `tests/.env.test` → `tests/.env.{local,dev,prod}`：1 个 skill 的 run.sh 未跟进
8. `canonicalbridge` / `canonicalext` 实际在 `packages/ai-gateway/internal/{execution,providers}/`，不在 `packages/shared/`：2 处 cursor rules globs 错误

### 2.3 CLAUDE.md 自身错误

| 行 | 旧文本 | 修正 |
|---|---|---|
| 87 | "grouped into 7 buckets: `compliance/`, `audit/`, `traffic/`, `core/` (bootenv/logging/diag/telemetry/metrics/opsmetrics), `transport/` (...wirerewrite), `storage/` (...configstore), `identity/`..., `policy/`..., `schemas/`..." | "grouped into 8 buckets: `audit/`, `core/` (bootenv/logging/diag/metrics/telemetry; `diag/` includes `runtimeintrospect/`), `identity/` ..., `policy/` (decision/device/domain/hooks/payloadcapture/pipeline/rulepack; pipeline holds the former `shared/compliance/` decision engine), `schemas/` (configkey/configtypes/credstate/domain/thingtype), `storage/` (...redisfactory/spillstore/spillupload), `traffic/`, `transport/` (bufconn/configloader/http/...wirerewrite)" |

修正点：
- 桶数从 7 改为 8（实数）
- 去掉不存在的 `compliance/` 桶
- 去掉 `core/` 中不存在的 `opsmetrics`（实际位于 service-internal `observability/opsmetrics`）
- 补充 `transport/` 缺失的 `configloader`
- 补充 `storage/` 缺失的 `redisfactory`
- 补充 `schemas/` 缺失的 `configkey`

## 3. Brainstorm：关键决策点

### 决策 1：`shared/compliance/` 在新结构里如何引用？

**取舍**：
- A. 完全淡化（让历史路径成为遗忘）
- B. 在 CLAUDE.md 桶列表里加 "(former `shared/compliance/`)" 注释 / 在 `architecture-doc-triggers.md` 加 "(was X)" 标注

**选择 B**：保留 `(was ...)` / `(former ...)` 标注。
- **理由**：仓库尚未 GA，大量 SDD 文档 + git history 仍以 `shared/compliance/` 命名引用决策引擎；新人 grep 时如果完全找不到映射会卡住。已有 trigger doc 用 `(was shared/security/)` 模式，沿用一致。
- **不冲突 CLAUDE.md "no backward compatibility"**：这是文档中的"翻译表"，不是代码里的兼容 shim。

### 决策 2：`scripts/.coverage-allowlist` 中 17 个 stale handler/<domain> 条目—— 删除还是改路径？

**取舍**：
- A. 全部删除（脚本对不存在路径静默跳过，删除无功能影响）
- B. 改成新路径（如 `handler/cache` → `ai/cache`）
- C. 删除并加 `# removed YYYY-MM-DD — moved to <new path>` 注释

**选择 C（合并写法）**：删除 14 个 stale path 行，合并到一条说明性注释，记录原条目意图与新位置映射。
- **理由**：CLAUDE.md 明确"long-term goal is an empty allowlist"+ "no defer / no @deprecated kept alive"。每条原条目都是 "temporarily allowlisted; coverage push pending" 占位，按规则属于待清理对象。新位置（`ai/cache` 等）若仍 <95% 会在 CI 显式失败，那时再针对性补条目比维护一堆失效条目更健康。
- **保留**：2 处 idptest / authserver/store/storetest 改成 `identity/idptest` / `identity/authserver/store/storetest`（test helper B 类，新路径仍是真 test helper，需保留）。

### 决策 3：是否更新 `docs/dev/_internal/` 下的历史 plan 文档？

**选择"不动"**。
- **理由**：CLAUDE.md 明文："archaeology lives in git log"。R6/R8 reorg plan 是当时的运行手册，记录了 reorg 在哪个时间点决定的什么。这些文档是"快照"性质而不是"现行规范"，更新会篡改历史叙事。
- 仍发现的引用如 `_internal/r8-directory-size-decomp-plan.md` 含 `shared/runtime/opsmetrics`、`test-coverage-program-handoff.md` 含 `shared/security/pkce` —— 留作历史。

### 决策 4：cursor rule `completion-time-self-audit.mdc` 中 `tools/db-migrate/_archive/` 引用如何替换？

**初次修复错了**：我先写成 `tools/db-migrate/prisma/generated/` 和 `packages/db/` —— 二次 ls 证实**两条路径都不存在**！
- **复核后选择**：改成 `packages/shared/schemas/configtypes/`（实际由 `tools/db-migrate/codegen-go.mjs` 生成的目录，每个文件含 `Code generated by … DO NOT EDIT.` 头）。
- **副作用**：意外发现 `tools/db-migrate/codegen-go.mjs:19` 的 `outDir` 也是过期的（`packages/shared/configtypes/` 而非 `packages/shared/schemas/configtypes/`）。已加 task #12 跟踪，本次审计**不修**（属 codegen 脚本 bug，超出审计范围）。

### 决策 5：`smoke-gateway/SKILL.md` 硬编码绝对路径如何处理？

**取舍**：
- A. 改成相对路径或 `$(git rev-parse --show-toplevel)`
- B. 仅把 `nexus-gateway/` 替换为 `nexus-gateway-refactor/`

**选择 B（最小改动）**：仅做工作区目录名替换。
- **理由**：skill 设计是给单个开发者本机用的，绝对路径有"我现在在哪"的辅助语义。改成动态 git-root 路径会让 SKILL 文档可读性下降。等下次更大范围重写 smoke-gateway 时再讨论 portability。

## 4. 执行清单

### 4.1 已修复（共 30 个文件）

#### CLAUDE.md（1）
- `CLAUDE.md` —— shared/ 桶清单修正

#### Cursor rules（3）
- `.cursor/rules/ai-gateway-smoke-mandatory.mdc` —— globs + body 中 canonicalbridge / canonicalext 路径
- `.cursor/rules/provider-adapter-canonical-openai.mdc` —— globs 中 canonicalbridge 路径
- `.cursor/rules/completion-time-self-audit.mdc` —— `_archive/` 引用替换为 `packages/shared/schemas/configtypes/`

#### Skills（4）
- `.claude/skills/smoke-gateway/SKILL.md` —— 2 处工作区路径 `nexus-gateway/` → `nexus-gateway-refactor/`
- `.claude/skills/test-compliance-proxy/SKILL.md` —— 2 处 `shared/{compliance,...}` brace 展开重写
- `.claude/skills/test-openai-responses/run.sh` —— 4 处 `.env.test` → `.env.local`
- `.claude/skills/iam-impact-review/skill.md` —— `shared/security/iam` ×2 + handler 路径表述 + arch doc 路径

#### 架构文档（16，由 sonnet agent 批量修）
- `docs/dev/architecture/iam-identity-architecture.md`（6 处）
- `docs/dev/architecture/oauth-pkce-admin-auth-architecture.md`（5 处）
- `docs/dev/architecture/idp-sso-architecture.md`（2 处）
- `docs/dev/architecture/jwt-verifier-architecture.md`（1 处）
- `docs/dev/architecture/prometheus-naming-architecture.md`（3 处）
- `docs/dev/architecture/otel-pipeline-architecture.md`（3 处）
- `docs/dev/architecture/diag-event-triage-architecture.md`（4 处）
- `docs/dev/architecture/trace-id-propagation-architecture.md`（1 处）
- `docs/dev/architecture/pii-redaction-policy-architecture.md`（1 处）
- `docs/dev/architecture/otel-span-attributes-architecture.md`（1 处）
- `docs/dev/architecture/service-bootstrap-config-architecture.md`（1 处）
- `docs/dev/architecture/runtime-introspection-architecture.md`（1 处）
- `docs/dev/architecture/normalization-architecture.md`（1 处：`shared/compliance/pipeline.go` → `shared/policy/pipeline/`）
- `docs/dev/architecture-doc-triggers.md`（3 处 opsmetrics 修正）
- `docs/dev/workflow/local-dev-debugging.md`（1 处）
- `docs/dev/README.md`（删除已退役的 `docs/dev/_archive/` 条目）

#### Scripts（2）
- `scripts/.coverage-allowlist` —— 2 处 idptest/storetest 改路径；17 处 stale handler/<domain> 合并删除（保留映射注释）
- `scripts/check-arch-doc-triggers.mjs` —— 注释中 `_archive/` 引用替换为 `runbooks/`

#### Go 源码注释（10，由第 6 路 sonnet agent 批量修）
- `packages/nexus-hub/internal/identity/agentca/{token,ca,agentca_test}.go`
- `packages/agent/internal/identity/{enrollment/sso_pkce,attestation/signer}.go`
- `packages/agent/internal/network/tls/engine.go`
- `packages/agent/internal/observability/{diag/slog_sink,localrollup/localrollup}.go`
- `packages/control-plane/internal/identity/iam/catalog_consistency_test.go`
- `packages/control-plane/internal/platform/audit/helpers.go`

### 4.2 待跟进（未在本次修复）

| ID | 描述 | 原因 |
|---|---|---|
| TASK-12 | `tools/db-migrate/codegen-go.mjs:19` `outDir` 是过期路径（`packages/shared/configtypes/` → `packages/shared/schemas/configtypes/`） | 属 codegen 脚本 bug；下次 `npx prisma generate` 会写到错误目录；超出本次审计范围 |
| backward-compat | `packages/agent/internal/observability/diag/slog_sink.go:3` 含 "re-exported here for backwards-compatibility" 字样 | CLAUDE.md "Development-phase policy: no backwards compatibility" 与之冲突；本次仅修复路径，shim 本身需独立评估 |
| ne-relative-path | `smoke-gateway/SKILL.md` 仍是硬编码绝对路径 | 见决策 5；建议下次重写 smoke 时统一改成 git-root 相对 |

### 4.3 验证

- `node scripts/check-arch-doc-triggers.mjs` —— `OK -- 82 architecture doc(s) referenced` ✓
- 全仓 `grep -rnE "shared/security|shared/runtime|shared/compliance/[a-z]|packages/agent/core/|tests/\.env\.test\b|tools/db-migrate/_archive|docs/dev/_archive|docs/dev/_internal/epic-status"` —— 仅剩 `_internal/` 内历史快照 + "was X / former / formerly" 历史注释 + CLAUDE.md 中 2026-05-16 迁移叙事文字 ✓
- 全仓 `grep -rnE "/nexus-gateway[^-]"` —— 仅剩 prod systemd `/etc/nexus-gateway/env`（合法）+ Go module path `github.com/ai-nexus-platform/nexus-gateway/`（合法，模块名未随 workspace 改名）✓
- `git status --short` —— 工作树干净时启动；修复期间无 `.git/index.lock` 冲突，符合并行会话安全规则 ✓

## 4.4 实际审计共经历的 7 轮（首次写本文档时仅记录到 Round 3）

| Round | 触发原因 | 新发现 |
|---|---|---|
| 1 | 主自审 4 问题 | `bridge_test.go:14` 仍含 `shared/compliance/audit_emitter_test.go` 引用 |
| 2 | Round 1 修复后再扫 | `bridge_test.go:16` 还有 `agent/core/observability/audit/queue_test.go` 引用（路径已迁移到 `agent/internal/observability/audit/queue/`） |
| 3 | Round 2 修复后扫 Go 注释 | `nexus-hub/internal/config/config.go:109`、`agent/internal/observability/localrollup/localrollup.go:8` 两处 `agent/core/observability/...` |
| 4 | Round 3 扫所有 mardown 含 `agent/core` 模式 | **3 个 Tier 1/2/3 架构文档**有 `agent/core/` ：`agent-enrollment-architecture.md`、`agent-forwarder-architecture.md`、`agent-policy-eval-architecture.md` |
| 5 | Round 4 扩展 grep 模式 | **再 8 个架构文档** + 3 处 Go 注释 + 4 处 .env.test 引用 |
| 6 | Round 5 修完后扫 Go 注释 | **9 处 Go 注释残留**（sso_flow.go、enroll.go、sso_seam_test.go、reconciler_integration_linux_test.go、windivert_integration_windows_test.go） |
| 7 | Round 6 后绝对终扫 | `roadmap.md:376` 1 处 + `control-plane-ui/src/constants/hooks.ts:126` 1 处 TS 注释 |

**经验**：每一轮修复都会让下一轮 grep 的"信号"更纯，从而暴露上一轮 grep 模式没覆盖的边缘情况。终态需要 **5–7 轮** sweep 才稳定，平均每轮抓出 ~5–10 处新发现 —— 单次 audit agent 报告不够，必须主会话有耐心反复扫。

## 4.5 用户中途追加修正 (Task #14)

执行至 Round 6 时用户追加：CLAUDE.md "In-flight epics & queued work" 段引用 `docs/dev/_internal/epic-status.md` 应改成 `docs/dev/roadmap.md`。
- **背景**：用户在 MEMORY.md 同步声明，`epic-status.md` 已迁移成 `roadmap.md`（roadmap.md 此前被审计误判为"其它会话偶然涉及"，实际是用户主动迁移产物）。
- **修正**：CLAUDE.md 单行替换；同时把审计期间发现的 `roadmap.md:376` 一处 stale `agent/core/` 也一并修了（与用户迁移工作一致）。
- **决策日志措辞修正**：前面"另一个会话的工作 —— 必须 exclude"叙述已不准确；现在 `roadmap.md` 是用户的合法编辑产物，正常 commit 即可。

### 4.6 完整文件清单（最终态）

71 个文件被本次审计修改：

- **配置/规范类**：`CLAUDE.md`（2 处独立修：桶清单 + epic-status→roadmap）、`docs/dev/README.md`、`docs/dev/architecture-doc-triggers.md`
- **Cursor rules**：3 个 `.mdc`
- **Skills**：5 个 `SKILL.md` + `run.sh`
- **架构文档**：24 个 `docs/dev/architecture/*-architecture.md`（含 2 个文档标题字段也改了）
- **Workflow 文档**：2 个 `docs/dev/workflow/*.md`
- **Roadmap**：1 处（用户已 staging 的 roadmap.md 中追加 1 处 path fix）
- **Scripts**：2 个 (`.coverage-allowlist`、`check-arch-doc-triggers.mjs`)
- **Go 源码注释（非 production code 逻辑）**：23 个 `.go` 文件，全部为 doc comments
- **TS 注释**：1 个 `hooks.ts` —— 单行 `matchesIngress` 引用
- **决策日志（新建）**：本文件

## 5. 经验

### 5.1 agent 调度模式（用户原话："合理使用多 agent 和 sonet"）

- **广度调研：4 路 Explore 并行**：单次大并发覆盖 4 个独立领域，互不重叠。Explore 只读特性正好契合"先盘清问题再决策"的需求。
- **批量机械修复：sonnet agent 后台跑**：16 个架构文档 + 10 个 Go 注释 = 26 个文件机械路径替换，主会话同期处理 cursor rules + skills + scripts（不重叠的文件集），实现真并行。
- **决策 + 复核留在主会话 opus**：每个 agent 的报告都经主会话独立 `grep` / `ls` 验证，已驳回 1 处误报、补充 1 处遗漏（agent #4 漏了 Go 源码注释，主会话最终 sweep 才发现并补救）。

### 5.2 不变 vs. 易变路径

```
[NEVER CHANGE]      Go module name: github.com/ai-nexus-platform/nexus-gateway/...
[NEVER CHANGE]      Prod systemd EnvironmentFile: /etc/nexus-gateway/env
[LOCAL WORKSPACE]   nexus-gateway-refactor/  (was: nexus-gateway/)
[REORG]             packages/shared/{security,runtime,compliance}/ 已废弃
[REORG]             packages/control-plane/internal/handler/<domain>/ 已分布化
[ENV RENAME]        tests/.env.test → tests/.env.{local,dev,prod}
```

下次工作区/重构时应继续维护此映射表，避免修复时再把"合法"路径误改成新的"错"路径（本次决策 4 第一次就误改了，二次复核才纠正）。

### 5.3 自查 vs. 委托的边界

`CLAUDE.md` 自身有错误这件事 —— 一个 audit 工件被审计但本身不准 —— 是这类工作最容易遗漏的盲区。**修复策略**：审计前先 `ls` 关键目录确认"真理基线"，再用基线核对每一个工件。这次正是在 agent 启动前的 5 秒 ls 中发现 9 桶 / 8 桶差异，从而锁定了后续所有修复的方向。
