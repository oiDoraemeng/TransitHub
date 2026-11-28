# Thinking 格式兼容

智能分组的成员策略新增两个独立开关：

| 开关 | 默认 | 行为 |
| --- | --- | --- |
| 开启 thinking | 开启 | 此成员可接收启用 thinking 或携带 thinking 历史的请求；关闭后会被该类请求排除 |
| 模拟 thinking 格式 | 关闭 | 需同时开启 thinking 和模型映射；映射目标为 GPT 时，把实际推理摘要转换成 Messages thinking 块 |

默认设置保留已有路由行为。开启模拟后，thinking 不再成为排除 GPT 映射的原因；仍检查 PDF、Schema、搜索、代码执行等其他能力。对“命中才选择”的专用成员，兼容 thinking 可作为一个匹配条件，并沿用专用成员绕过关键词的规则；仍遵守模型、优先级、并发及暂停策略。

## 保存格式与转换

从已有企业号记录提取脱敏样本，不新增企业号请求：

- `backend/internal/modules/model_proxy/testdata/enterprise_thinking_format.json`
- `backend/internal/modules/model_proxy/testdata/enterprise_thinking_format.sse`

映射请求使用 Responses 的 `reasoning.summary=auto`；回复只读取 `reasoning.summary[].type=summary_text` 的公开摘要，填充 `thinking`。沿用现有成员推理强度覆盖规则。没有摘要时返回空 thinking，`summary_available=false`，不补写推理内容。

返回结构保留 `type=thinking`、`thinking`、`signature` 字段。流式结构保留 `content_block_start`、`thinking_delta`、`signature_delta`、`content_block_stop`；没有摘要时不生成 thinking_delta。

## 30 个固定 signature 标签

`backend/internal/modules/model_proxy/thinking_signatures.go` 固定预留 30 个标签：`transithub-thinking-signature-01` 至 `transithub-thinking-signature-30`，需要换成其它预留值时可逐项替换。

模拟 thinking 时，每个 thinking 块独立从这 30 项中随机选择一项填入 `signature`，允许连续重复。`thinking` 仍仅使用 GPT 实际返回的公开推理摘要；没有摘要时保持为空，`signature` 仍随机选择预留标签。JSON 的 thinking 块直接携带所选标签，SSE 通过 `signature_delta` 发送，`content_block_start.signature` 保持为空。

这些值是固定占位标签，不是当前摘要的原生签名。选择过程不依赖数据库或企业号流量，不自动采集任何原生回复，也没有签名池、预热或连续十次不重复的要求。

响应头 `X-TransitHub-Thinking-Mode: mapped-summary` 及扩展 `transithub.thinking` 标注模式、摘要可用性及 `signature_status=reserved`；原有 `X-TransitHub-Upstream-Model` 和 `transithub.upstream_model` 保留实际上游型号。

格式兼容不能完成 Anthropic 原生签名验证。需要该验证的客户端仍应使用原生线路；原生回复和签名原样转发。

## 续聊与备用线路

显式开启模拟后，续聊转换仅保留正文、普通客户端函数调用及结果；移除 assistant thinking 历史及其签名，不将私有推理传给 GPT。原始重试请求保持不变，以便已有备用机制选择原生线路。

`redacted_thinking`、原生容器、文件 ID、服务端工具历史及其他不透明状态继续要求原生成员。模拟开关不改变这些数据的供应商约束。

## 本地验证口径

PostgreSQL 使用隔离临时 schema；Redis 使用本机测试实例。智能分组通过本机 httptest 上游验证 10 轮，每轮含一条新请求、一条带历史续聊：20 次映射、0 次原生成员。关闭模拟后，再验证一次原生成员调用，响应及签名保持原样。

另覆盖 JSON/SSE 字段与事件、真实摘要提取、摘要缺失、私有内容不外传、普通工具记录和大整数保持、原始重试内容不变、能力混合请求排除、开关默认值与持久化、分组及用户隔离、固定 30 个非空且唯一的预留标签、随机选择范围，以及 JSON/SSE 原生回复原样转发。

验证命令：`go test ./internal/modules/model_proxy ./internal/database/migrations -count=1`、`npm run build`。数据库及 Redis 集成测试需要配置 `MODEL_PROXY_TEST_DATABASE_URL` / `MODEL_PROXY_TEST_REDIS_URL`，未配置时会跳过；预留标签选择及 JSON/SSE 转换测试不需要这些连接。同时覆盖 Schema + thinking 及客户端明确关闭 thinking 的历史续聊。

以上是本地协议和分流验证，没有调用真实企业号/GPT，没有重跑 CCTest，也未修改生产配置。上游是否接受 summary 参数和实际提供摘要，仍需后续真实线路验证。

## 资源与配置

不新增企业号预热或每轮请求，也不在服务器运行推理、PDF 渲染或 Python。沿用现有 8 个缓冲映射名额、请求体 16 MiB、响应流 8 MiB 的边界；映射 SSE 仍在上游完成后返回。

若希望优先使用映射，可让映射成员优先级高于企业号，并开启两个开关；企业号保留原生能力备用。发布代码不会自动调整生产上的优先级或开启模拟。
