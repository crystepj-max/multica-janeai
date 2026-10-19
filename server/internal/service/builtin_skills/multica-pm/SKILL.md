---
name: multica-pm
description: "Use when planning or supervising Multica work: decomposing goals into issues, dependencies, priority, dates, progress, risk, recovering a stuck project. 中文：拆任务、排优先级、跟进进度、里程碑/依赖/风险、项目卡住。Not for platform mechanics (see multica-platform)."
user-invocable: false
allowed-tools: Bash(multica *)
---

# Plan and supervise work

Mika's durable instructions still apply. This skill adds the working
playbooks: how to shape a plan, what a good snapshot looks like, and what to do
when a project stops moving.

Read the one section the moment needs. Do not read the whole file to "be safe"
— that is the cost this skill exists to avoid.

## Plan a goal

You need three things before a plan is worth writing: the outcome, the real
date, and who can carry the work. If any of them is missing, that is the first
question — not a reason to guess.

1. **State the outcome in one sentence, and how you will know it is done.**
   "Search returns results in under 300ms at p95" is a plan input. "Improve
   search" is not.
2. **Find what already exists.** List the project's issues, and the open issues
   that mention the same outcome. If a record already carries it, extend that
   record instead of opening a second one.
3. **Split into stages**, not into a flat list. A stage is a barrier: its work
   can run in parallel inside itself, and nothing later starts until it closes.
4. **Create the parent first**, then its children.
5. **Verify what you created** — `multica issue children` shows the stages, and
   only the first stage should be `todo`.

A worksheet before you type a command:

| Stage | Deliverable | Owner | Depends on | Done when |
|---|---|---|---|---|
| 1 | the thing that unblocks everything else | | — | observable check |
| 2 | the next barrier | | stage 1 | observable check |

Sizing rule: one child is one deliverable one assignee can finish in a single
run. If a child needs two unrelated decisions, two different skills, or a
review cycle plus an implementation, it is two children.

```bash
# parent: the outcome, the date, the context
multica issue create --title "Search latency under 300ms" \
  --description-file ./parent.md --project <project-id> \
  --priority high --due-date 2026-11-01 --assignee <owner>

# stage 1 runs now; later stages park until promoted
multica issue create --title "Profile the query path" --parent <parent-id> \
  --stage 1 --status todo --assignee <agent> --description-file ./stage1.md
multica issue create --title "Add the missing index" --parent <parent-id> \
  --stage 2 --status backlog --assignee <agent> --description-file ./stage2.md
multica issue create --title "Verify p95 in staging" --parent <parent-id> \
  --stage 3 --status backlog --assignee <agent> --description-file ./stage3.md
```

Write description files inside your working directory, never a shared or
temporary path. Every child description carries: the goal, the scope boundary,
the acceptance check, and the references a run would otherwise have to guess.

## Stage patterns

| Shape | Layout | Use when |
|---|---|---|
| Parallel | all children in stage 1, all `todo` | independent pieces, same barrier |
| Serial | stage N `backlog`, promoted one at a time | each step needs the previous result |
| Fan-in | stage 1 several pieces, stage 2 one integration child | integration needs all inputs |
| Spike then build | stage 1 investigation, stage 2 implementation | the approach is still unknown |
| External gate | child plus a wakeup condition on the outside issue | the trigger is another team's work |

Promotion is manual on purpose. The platform tells you a barrier closed; it
does not decide that the next step is safe. Re-read each parked child, confirm
its stated dependency actually holds, then promote it:

```bash
multica issue children <parent-id>
multica issue status <child-id> todo
```

For a dependency that lives outside the family, do not poll. Register the
condition and end the turn:

```bash
multica issue wakeup create <issue> --until-issue <other-issue-id> \
  --until-issue-state done --expires-in 168h --instruction-file ./check.md
```

## Priority pass

Run one when a member asks, when a commitment changes, or when the board no
longer matches what people believe.

```bash
multica issue list --status todo --output json \
  --fields identifier,title,status,priority,due_date,assignee_id,project_id
multica issue list --status blocked --output json --fields identifier,title,priority,assignee_id
```

Produce a short ordered list, with the reason attached to each item that moved:

- first, what blocks work already in flight or holds a stage barrier
- then, what puts a committed date at risk
- then, recorded priority, then downstream unblock count, then cheap decisions

Every `urgent` you keep needs either a date or a blocker behind it. If it has
neither, say so and propose the demotion rather than quietly leaving it there.

Do not renumber or reorder the board cosmetically. Positions are a human's
working memory; change order only when the reason would survive being read out
loud.

## Progress snapshot

Read before you write; report numbers you actually read.

```bash
multica issue children <parent-id> --output json
multica issue runs <parent-id> --active --siblings --output json
multica issue list --project <project-id> --output json \
  --fields identifier,title,status,status_category,priority,due_date,assignee_id
multica issue wakeup list <parent-id>
```

Answer in this order, and stop when the reader has what they need:

1. **Done** — `x/y` children terminal, per stage when stages exist.
2. **Running** — active runs and who owns them.
3. **Blocked** — each item, what it waits on, who owns the unblock.
4. **At risk** — the date, the slip you expect, the reason.
5. **Next** — the one action you recommend, and where it lives.

A run being active is not progress. A finished child is not the parent's goal
met — the parent's own acceptance criteria decide that, and a human decides
`done`.

## Risk review

Run one before a milestone date, at each stage barrier, and whenever a member
asks "where are we".

Scan for these, because they are the failures that stay invisible on a board:

- an open issue with no assignee
- an overdue issue still in `todo` or `backlog`
- a `blocked` issue whose comment does not name what unblocks it
- an `in_progress` issue with no active run and no recent activity
- every child terminal but the parent untouched
- two issues describing one outcome
- an assignee holding more open work than it can run

Escalate in one message: what is at risk, the evidence, what you need, and the
default if nobody answers. Name a person, not a role, when you can.

## Recovery playbooks

**A stuck parent.** Read the children, then the parent's own acceptance
criteria. If the criteria hold, propose `in_review` with the evidence. If they
do not, the gap is the next child — create it, do not reopen finished work.

**A blocked issue.** Classify before acting, because the responses differ:

| Kind | Test | Move |
|---|---|---|
| Missing information | the ask is ambiguous | ask one precise question, name the default |
| Missing access | credentials, permission, repo | escalate to the owner, state what access |
| Broken dependency | another issue or team has not delivered | wakeup condition, name the owner |
| Failed run | a run ended in failure | read the failure, retry once if transient |
| Conflicting instructions | two sources disagree | stop, present both, ask which wins |

**A failed run.** Do not retry blind and do not treat it as the goal being
impossible. Read what failed. Retry only when the cause is transient and the
action is safe to repeat — once. Otherwise report the blocker with the failure
quoted.

**An overloaded assignee.** List that assignee's open issues, keep the
highest-priority and the ones already in flight, and move the rest — with a
reason the receiving owner can act on. If nothing can take the work, say which
date moves rather than queuing it silently.

**A slipping milestone.** Recompute the critical path from the board, then offer
three options with their cost: cut scope, move the date, or add capacity. Do not
present a slip as a status update — it is a decision someone has to make.

## Reporting templates

**Plan proposal** (before creating anything):

> Outcome: <one sentence>. Done when: <observable check>.
> Stages: 1 <deliverable> → 2 <deliverable> → 3 <verify>. Owners: <who>.
> Date: <date or "needs a date">. Create these <n> issues?

**Status update** (after reading):

> Done <x/y> (stage 2 of 3). Running: <n>, <owner>. Blocked: <item> on
> <dependency>. At risk: <date> by <slip>. Next: <one action>.

**Risk escalation**:

> <Risk> will miss <date> because <evidence>. I need <decision/access> from
> <person> by <date>. Without it, <default consequence>.

## Working in a non-English workspace

Your instructions are English; the work rarely is. Translate the conversation,
never the data. Map the team's vocabulary onto the objects the platform actually
has, and say which object you used.

### Term map (Chinese workspaces)

| 团队说法 | 平台里真实的承载物 | 怎么落 |
|---|---|---|
| 里程碑 | 没有独立对象。二选一：`project`（有状态、负责人、起止日期、优先级、关联 issue），或 `parent issue` + 分阶段子 issue | 选定后一句话说明用的是哪个 |
| 阶段 / 里程碑阶段 | `--stage N` 的子 issue，配 `backlog` → `todo` 提升 | 后序阶段建为 `backlog`，逐个提升 |
| 依赖 / 前置条件 | stage 屏障、`backlog` 提升、wakeup 条件 | 不要只写在评论里 |
| 验收标准 / 完成定义 | issue 正文（没有独立字段，命令和 JSON 都不返回） | 写成可观测结果，不写"做好一点" |
| 优先级 P0 / P1 / P2 | `--priority`：`urgent` / `high` / `medium` / `low` / `none` | 按 P0→`urgent`、P1→`high`、P2→`medium` 映射，映射前跟团队确认，不默默改 |
| 阻塞 / 卡住了 | `--status blocked` 加一条写明"谁解除、何时"的评论 | 只改状态不算处理 |
| 负责人 / 主办 | `--assignee`（成员或 agent） | 换人要给出对方能行动的理由 |
| 排期 / 死线 | `--start-date` / `--due-date`（`YYYY-MM-DD`）；项目级同样有这两个参数 | 没有真实日期就不填 |
| 迭代 / Sprint | 没有 sprint 对象：用 `project` 或 `parent` + stage 表达 | 别假装有这个层级 |

Two rules sit above the table: a field value is never translated (`in_review`
stays `in_review` in a Chinese sentence), and a term the team already uses wins
over the platform's name — answer in their word, write with the platform's.

### Report templates (Chinese workspaces)

Same shapes as above, in the language of the thread. Use them as structure, not
as fixed wording; a `ja` / `ko` / `fr` workspace keeps the ASCII values and
translates the prose.

**计划提案**

> 目标：<一句话>。完成判定：<可观测结果>。
> 阶段：1 <交付物> → 2 <交付物> → 3 <验收>。负责人：<谁>。
> 日期：<日期，或"待定，需要你定">。要我建这 <n> 个 issue 吗？

**进度快照**

> 已完成 <x/y>（第 2/3 阶段）。进行中：<n> 个，<谁>。
> 阻塞：<事项>，等 <依赖>，<谁> 负责解除。风险：<日期> 可能推迟 <天数>。
> 下一步：<一个动作>，落在 <issue-id>。

**风险升级**

> <风险> 会让 <日期> 无法达成，证据是 <证据>。需要 <谁> 在 <日期> 前给出
> <决定 / 权限>。若无回应，默认 <后果>。

### 中文工作区的三个坑

- 把"待验收"当状态值提交 —— 写入会被拒，还会白烧一轮；状态永远是英文键。
- 把"里程碑"当成一个存在的字段或按钮 —— 平台没有，承诺了就是编造状态。
- 同一术语两套写法（"阻塞"和 `blocked` 交替出现）—— 读者要二次翻译，选一种用到底。

## Pitfalls

- Creating every serial step as `todo` — the whole chain fires at once.
- Assuming a closed barrier promotes the next stage — you promote it.
- Reporting a status you did not read, or a count you estimated.
- Inventing a date to make a plan look complete.
- Leaving a dependency in a comment instead of in stages or a wakeup.
- Opening a second issue for an outcome that already has one.
- Setting `done` yourself, or cancelling work a human has not agreed to cancel.
- Posting progress comments on an issue run instead of one result comment.
