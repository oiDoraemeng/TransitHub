# 输入量范围矫正

规则：`矫正上限 = ceil(最高估算值 × (1 + 超出比例 / 100))`。

例如范围为 **150,000–170,000**，超出比例 **40%**，上限就是 **238,000**。上游上报 238,000 时保留；上报 238,001 或 1,695,788 时，输入总量都压到 238,000。不降到最低值，也不减去 40%。缓存优先缩减，各项合计保持一致，输出 Token 保留。

设置仍在智能分组中，增加 40% 快捷选项和公式提示。新分组默认比例 40%、开关关闭；已有分组保留原设置。

## 范围怎么算

- 提取完整可见输入：系统提示、历史消息、工具定义、调用参数和工具结果。忽略 JSON 排版、转义方式和非输入 metadata。
- 文本不超过 16 KiB 时完整分词；更长时取 64 个分散片段，含边界上下文总工作量不超过 20 KiB。
- 记有效文本字节数为 B，各样本 Token/字节密度为 d。以合计密度 μ 和样本密度标准差 s 计算 `B × (μ ± 3s/√n)`，再应用离线校准包络系数，上界另加消息和工具结构余量。
- 这是经验估算区间，**不是保证覆盖所有内容的数学上下界，也不是注入量证明**。抽样为固定分散位置，不能把 3s/√n 解读为经过证明的统计置信度。
- 按当前成员映射后的型号选择分词器；同请求重试复用相同分词器的结果。运行时没有官方计数网络请求，也不拿可疑上游 usage 自动校准。

| 型号 | 本地方法 | 当前限制 |
|---|---|---|
| 已识别 GPT 型号 | o200k_base / cl100k_base 抽样分词 | 完整请求结构仍为估算 |
| Google 注册表支持的 Gemini 型号 | Gemma 3 SentencePiece 抽样分词 | 不将新分词器型号自动套入旧表 |
| **Claude、Luna 和未识别型号** | **保守兼容范围：1 到有效 UTF-8 字节数＋结构余量** | **缺少当前 Claude 官方计数样本；范围较宽，尚未获得紧凑且已校准的 Claude 区间** |

Claude 兼容范围用于识别极端放大，不能当作 Claude 精确估算。图片、文件、隐藏历史、远程缓存引用和托管工具上下文无法从本地完整计数，继续跳过矫正。

## 本地验证

- 校准语料 460 份，留出文本 116 份；3 个分词器共 **348/348** 留出对照落入经验范围。仅验证本地文本分词，未调用三家官方 API，不代表线上所有完整请求的准确率。
- 47 个短文本/样本片段 × 3 个分词器：Go 与 Python 参考计数 **141/141** 一致。
- 40% 的边界、历史 1,695,788 输入、缓存拆分、输出保持、JSON/SSE、成员映射、原生 Gemini、分组隔离、PostgreSQL 配置持久化均通过。
- 长文本估算耗时（词表预热后，排除请求读取和 JSON 解析）：GPT 约 2.3 ms，Gemini 约 3.5–3.8 ms；测试请求从 1 MiB 到 16 MiB，采样工作量不随其增长。
- `go test ./...`、前端 `npm run build`、后端本地构建通过。数据库测试使用临时本地 PostgreSQL，已停止。当前 Windows 未配置 CGO，无法运行 Go race detector；并发结果一致性测试已通过。

一个本地中文长文本例子（508,004 个有效字节，不是用户历史请求）：

| 模型 | 最低 | 最高 | 40% 矫正上限 |
|---|---:|---:|---:|
| GPT-4o | 87,427 | 104,929 | 146,901 |
| Gemini 2.5 Pro | 97,910 | 105,050 | 147,070 |
| Claude 兼容估算（未校准） | 1 | 508,044 | 711,262 |

响应头提供 `X-TransitHub-Input-Token-Min/Max/Limit` 与 `X-TransitHub-Input-Estimate-Method`。触发矫正时，日志保留范围、算法、有效字节数、抽样字节数、实际目标模型和修改前后用量。

复现脚本：`scripts/token-range/calibrate.py`；原始留出结果：`scripts/token-range/validation.json`。

依据：[OpenAI tiktoken](https://github.com/openai/tiktoken)、[Google 官方分词器型号注册表](https://github.com/googleapis/python-genai/blob/main/google/genai/_local_tokenizer_loader.py)、[Claude 官方计数](https://platform.claude.com/docs/en/build-with-claude/token-counting)、[旧 Claude 分词器适用限制](https://github.com/anthropics/anthropic-tokenizer-typescript)。
