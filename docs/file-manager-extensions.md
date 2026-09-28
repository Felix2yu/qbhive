# 文件管理功能扩展设计

> 单文件自动归档、文件名自动清理（内置规则 + 自定义正则）、AI 格式化（OpenAI 兼容多通道）、
> 审计日志与回退（「文件」页）、**批量重命名（模板）** 已实现（见 README「文件管理」小节）。
> 本文 ③ 为实现记录，①② 仍未实现。

## 总原则（与已实现能力保持一致）

- 所有触及做种中文件的操作必须通过 qB API（`renameFile` / `setLocation` 等），
  禁止裸 `os.Rename` 移动做种文件
- **任何情况下绝不覆盖、不删除已有文件**：目标存在一律加 `_hash8` 兜底或跳过
- 异步 API 提交后不在当轮做依赖新路径的操作，统一「下轮扫描确认」
- 每次自动改动写入审计日志（`data/filemgr_audit.jsonl`），可回退
- 状态持久化沿用 tmp+rename 原子写，放 `data/` 下

## ① 按类型 / 日期自动分类（优先级：高）

- **触发条件**：某任务的 clean pass 与单文件归档均已完成（`cleanState[hash].Done`
  且归档 done）的下一轮扫描；需开启 `classifyEnabled` 开关
- **处理逻辑**：
  1. 按模式选目标目录：
     - `type` 模式：按主文件扩展名映射类型目录（video/audio/image/archive/doc，
       映射表内置 + 可配 `typeDirs`）
     - `date` 模式：按完成时间 `CompletedOn` 格式化为 `2006-01` 月份目录
  2. 目标目录 = 当前 save_path 的父目录 + 类型/月份目录（目录不存在时由 qB 创建）
  3. 调用 qB `/torrents/setLocation` 移动任务（qB 自己移动文件并保持做种与元数据一致，
     **禁止裸 mv**）
  4. setLocation 异步：下轮以「GetTorrents 返回的 save_path 已变为目标路径」确认，
     确认前不重复提交；连续 3 轮未生效则告警放弃
- **配置项**：`classifyEnabled bool`、`classifyMode "type"|"date"`、
  `typeDirs map[string]string`（扩展名 → 子目录）
- **风险点**：
  - 与 renameFile 状态互斥：同一任务同轮不做两件事（clean 未 Done 前不进入分类）
  - 目标目录已存在同名文件：setLocation 前用 `os.Stat` 预检，命中「绝不覆盖」策略跳过
  - RSS 规则可能已指定 savePath：分类仅对 save_path 位于受管根目录下的任务生效，
    避免与其他分类工具互相打架

## ② 重复文件检测（优先级：中）

- **触发条件**：低频定时（建议每日一次，`dedupeHours` 可配）或「文件」页手动触发；
  `dedupeEnabled` 开关
- **处理逻辑**：
  1. 按 `SavePath` 分组扫描磁盘文件（并发 1，避免打爆盘）
  2. 粗筛：`(size, 归一化文件名)` 相同归入候选组
  3. 精筛：候选组内计算内容指纹——大文件只读首 4MB + 尾 4MB + size 做近似 hash，
     近似命中再全量 SHA-1 复核（可配只做近似）
  4. **只报告不自动删除**：分组结果写入 `data/duplicates.json`，「文件」页新增
     重复分组展示（组内列出路径 / 大小 / 所属任务）
  5. 用户手动决定处理：已做种文件的删除必须走 qB API 取消该文件的勾选
     （`filePriority=0`），禁止直接删磁盘文件；未做种文件才允许删磁盘
- **配置项**：`dedupeEnabled bool`、`dedupeHours int`、`dedupePathFilter []string`
  （只扫描这些根目录）
- **风险点**：
  - 硬链接（qB 常见）会误报重复：按 inode 去重，同 inode 视为同一文件
  - 正在下载 / 校验的文件跳过
  - IO 开销：单并发 + 每文件限速读取；任务数量大时分批跨轮执行

## ③ 批量重命名规则（模板）（✅ 已实现）

- **触发条件**：纯手动——「文件」页「批量重命名」卡片：选任务 → 输入模板 → 预览 → 执行。
  不做自动触发；上次模板存 localStorage 便于复用
- **实现要点**：
  - 模板变量：`{date}`（完成日期 2006-01-02，缺失时回退添加时间）、`{type}`（扩展名分类
    video/audio/image/archive/doc/other）、`{index}`（任务文件列表序号，从 1 起）、
    `{orig}`（原 base 名去扩展）、`{title}`（任务名）
  - 模板校验（后端 `validateBatchTemplate` + 前端同步报错提示）：非空、≤256 字符、
    占位符必须属于支持集合、至少含 `{index}` 或 `{orig}` 之一（防全员重名）
  - 仅重命名文件 base 段，目录段不动；扩展名保留；产出经 `sanitizeCleaned` 护栏
  - 后端 `POST /api/filemgr/batch/preview`（dry-run 预览）与 `POST /api/filemgr/batch/apply`；
    apply 时按当前文件列表重新计算计划，预览过期不影响正确性
  - 提交复用 clean pass 的 `applyRenames` 管线（Stop → 批量 renameFile → Start），
    审计 `via="manual"`，可回退；pending 挂到 cleanState 核销（AISettled 置位防 AI 重格式化）
  - 冲突策略与自动清理一致：计划内目标去重 + 磁盘预检，绝不覆盖已有文件

## 实施建议顺序

~~3（批量重命名，已实现）~~ → 1（分类，与 clean pass 天然衔接、复用 done 状态）→
2（去重，独立只读功能风险低）。
