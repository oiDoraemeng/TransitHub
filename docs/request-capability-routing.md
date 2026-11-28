# 按实际请求能力分流

TransitHub 负责协议转换、Schema 校验和调度。PDF 解析、搜索与 Python 执行交给上游，不在中转服务器启动 PDF 渲染器或执行容器。新规则不匹配检测站提示词，不生成或重放 thinking 签名，也不替换知识截止时间答案。

## 能力条件

成员策略提供 `disabled`、`skip_on_match` 和 `require_match`。条件包括 `pdf`、`code_execution`、`json_schema`、`web_search`、`native_claude`，配置独立保存到每个智能分组成员。

`skip_on_match` 在任一所选条件命中时排除成员。`require_match` 要求至少命中一个条件，并覆盖请求的全部已识别能力，避免 Schema 专用线路接走 Schema + PDF 请求。命中的专用成员不受关键词条件限制，仍遵守模型支持、启用状态、暂停、并发、优先级、限速、流式和输入限制。重试只在筛选后的成员内进行。Compact 保持原有维护请求策略。

识别实际 PDF 内容块、JSON Schema 配置、保留的服务端工具类型及原生 thinking/工具历史。提示词提及 PDF/Python、知识截止日期、signature，或普通客户端函数恰好叫 `web_search` / `code_execution`，均不触发能力条件。历史附件参与判断；工具结果中的示例文档不被当作新附件。

显式 `thinking.enabled/adaptive`、历史 thinking/redacted_thinking、原生服务端工具结果、文件 ID、容器/MCP/上下文状态和需要动态过滤的新版搜索工具默认进入原生能力路径。“模拟 thinking 格式”开关允许普通 thinking 请求及可见续聊走 GPT 映射，thinking 使用 GPT 实际推理摘要，signature 从 30 个固定预留标签中随机选择；其余不透明状态仍排除 GPT。详见 [thinking 格式兼容](thinking-format-compat.md)。

## 此前部署策略（2026-11-28，历史记录）

以下是此前部署时的「kiro」组配置，不代表后续人工调整后的实时状态：

| 成员 | 能力策略 | 优先级 | 用途 |
| --- | --- | --- | --- |
| kiro 普通 | 五种条件命中跳过 | 1 | 常规请求，保留已有其他限制 |
| kiro 映射 | 只接收完整覆盖的 Schema / 基础搜索请求 | 2 | Claude Messages → gpt-6-luna Responses |
| kiro 企业 | 能力与关键词策略关闭 | 0 | PDF、Python、原生 thinking，以及其他成员失败后的备用 |

策略关闭的成员仍可被调度，因此企业成员能处理多个能力组合。它也可能在普通号不可用或已有关键词规则排除普通号时承接常规请求。未添加新的检测题关键词。

## GPT 映射协议

模型映射后的目标为 GPT 时，TransitHub 自动把 `/v1/messages` 转为 `/v1/responses`，保留客户端模型别名。协议转换不再依赖 `requestMatchMode=require_match` 或是否命中特定能力；能力策略只负责选成员，不能让已映射为 GPT 的请求继续使用 Messages 上游接口。直接使用线路 key、请求模型为 `gpt-*` 时，同样支持 Messages 转换；直接线路不会继承其他智能分组成员的 Claude 别名映射。

响应头 `X-TransitHub-Upstream-Model` 和响应扩展 `transithub.upstream_model` 披露实际发送的上游型号；扩展中的 `tool_records`、`citations` 保留真实搜索证据。这是功能适配，不表示返回内容来自请求别名对应的 Claude 型号。

支持文本、PDF、图像、普通函数及其结果、基础 `web_search_20250305` 和代码工具转换。PDF 文件或 URL 原样交给上游；代码工具转换为 `code_interpreter`。基础搜索转换域名白名单、地理位置、调用上限和工具选择，并保留真实调用记录与来源链接。地理位置补齐 Responses 要求的 `type=approximate`；OpenAI 不支持的非空域名黑名单明确拒绝转换，不会静默丢弃限制。新版动态搜索和原生搜索历史走企业线路。跨供应商 file_id、签名、容器状态等不能转移。

Schema 转换到 Responses strict JSON Schema，同时在 instructions 中附上同一份格式契约，兼容接受但不转发格式参数的上游。最终答案返回前进行本地完整校验；中间客户端函数调用以 `tool_use` 正常返回，等待客户端提供工具结果后再生成最终 JSON。大整数工具参数保持精度。不符合契约的最终响应交由现有失败处理，不当作成功。Schema 上限 64 KiB，禁止读取外部 URL / 本地文件引用。格式适配不额外请求模型，也没有新增无限修复循环。

上游完整结束后，适配器返回 Messages JSON 或完整 SSE，映射路径不实时逐 token 转发。搜索记录使用扩展字段和实际引用，不制造 Anthropic encrypted_content/encrypted_index。原生 Claude 搜索结果要求的加密续聊协议应使用原生线路。

代码工具必须返回真实、完成的执行调用与日志；文字声称执行不算成功。Responses 缺少 Claude 原生结果要求的退出码与分离 stderr，因此不会伪造这些字段。依赖原生执行结果、文件或容器续接的客户端应使用支持对应能力的真实服务。

## 资源限制

缓冲映射适配器全进程最多同时 8 个请求，单请求体 16 MiB、上游响应流 8 MiB。超额返回失败，由已筛选的备用成员处理；普通转发保持原有并发策略。占用名额会在成功、错误和取消时释放。

这些限制控制输入与响应体上界；JSON 解析、序列化和运行时另有额外内存开销，因此不能将 8 × 8 MiB 当作进程总内存上限。应用现有 1.5 GiB 容器限制保持不变，没有运行生产并发压力测试。

## 验证与剩余限制

完整 `model_proxy` PostgreSQL/Redis 集成测试、迁移测试、前端类型检查与构建通过。独立真实映射测试中，Schema 和搜索均通过。上线后从实际智能分组入口测试：

| 项目 | 实际结果 |
| --- | --- |
| Schema | 映射返回符合 Schema 的 JSON，实际模型 gpt-6-luna |
| 基础搜索 | 映射组账号 43793 返回 400，企业号备用完成真实搜索；独立映射测试另一次成功，表明组内账号能力不一致 |
| 混合 PDF | 企业号正确读出随机编号、金额、蓝色圆形 |
| thinking | 企业号返回 thinking 和 1244 字符签名；仅确认原生字段返回，未作独立密码学校验 |
| Python | 企业号 code_execution_20250825 和 20250522 均返回工具额度错误，未实际执行；当前 GPT 映射上游也已复现 400 |

Python 功能仍受外部服务限制，没有可宣称成功的执行记录。解决它需要恢复企业号的工具额度，或接入经过真实执行验证的外部后端。训练知识和 Claude 签名不能通过 JSON 协议转换生成；需要对应模型的真实上游。本次没有重跑 CCTest 网站，也没有把签名存在或模型自报日期当作官方身份认证。

发布镜像 `transithub:functional-compat-20261128`。代码发布前保存数据库和配置备份，分流配置单独事务更新并验证原设置未被并行修改。测试证据、部署结果、回滚信息见工作区 `reports/cctest-kiro-20261128/通用能力兼容执行记录.md`。
