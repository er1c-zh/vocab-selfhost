# Wordwell Self-host：项目背景与开发约定

> 本文件供后续维护者和代码代理快速了解产品约束、系统结构与当前实现。内容依据仓库源码、README 和 Compose 配置整理；新增或修改功能时应以实际代码为准，并同步更新本文及用户文档。
>
> 状态核对日期：2026-10-04。

## 项目目标

Wordwell 是一套可自托管的英语词汇学习系统。核心场景是用户在 Windows 论文或 PDF 阅读器中复制段落，在网页中粘贴、选中目标词，并保存词语、原句、来源和语境释义；之后可在电脑或 iPhone 浏览器中复习同一份词库。

项目目标：

- 用响应式 PWA 支持桌面和手机上的新增、复习、搜索、编辑、删除与数据管理。
- 由自托管 Go API 和 SQLite 保存词卡、复习记录与用户导入的词典。
- 在有词典数据时提供 IPA 和释义；缺少词条时明确提示，不猜造 IPA 或释义。
- 按需生成美式英语发音并缓存音频；提供本地音素/单词级练习评分、ASR 转写反馈和“不确定”结果。
- 释义 AI 是可选服务。基础词库、复习、备份和本地语音不依赖云端大模型 API。
- 交付方式以 Docker 镜像和 Docker Compose 为主，数据写入命名卷。

## 产品范围和边界

已覆盖的主要流程：

1. **收集**：粘贴原句或段落，在文本框中选中目标词；填写来源、语境释义，按需从本地词典补充 IPA 和释义，保存为词卡。
2. **复习**：查看到期词卡，揭示释义和上下文，播放缓存或新生成的语音，选择 Again、Hard、Good、Easy 评分。
3. **管理**：搜索词语、释义或原句；编辑、删除词卡；查看到期状态。
4. **数据管理**：导入 JSON/TSV 词典，导出 JSON 备份，并导入/合并备份。
5. **离线使用**：在已登录并加载过数据的浏览器中读取本地缓存；新增、编辑、删除和评分等操作可排队，联网后重放。

明确不在当前范围内：

- 不搜索、解析或打开 PDF；不提供阅读器、浏览器扩展或自动剪藏。输入内容由用户从现有阅读器复制粘贴。
- 不附带大型商业词典。仓库只嵌入少量示例词条；用户可导入自己的词典。
- 默认本地发音评测使用 Whisper Phoneme CTC 按目标词的 CMU/ARPAbet 音素序列对齐并估算分数，faster-whisper 返回转写；模型不可用或词典缺词时回退到转写比较。可选 Azure Speech 提供准确度、流利度、完整度、韵律及服务响应中的单词/音素结果。两者都只是练习反馈，不是口音诊断或母语水平认证。
- 当前 FSRS 排程以天为单位，最短间隔一天；没有日内学习或重学步长配置。
- 应用使用单一共享 Bearer Token，不含用户账户、角色或多租户隔离。

## 架构概览

~~~mermaid
flowchart LR
  Browser[Desktop or iPhone PWA] -->|HTTPS and same-origin API| Go[Go API and embedded web assets]
  Go --> SQLite[(SQLite in vocab-data)]
  Go --> Audio[(Cached WAV in vocab-data)]
  Go -->|internal HTTP| AI[Python AI service]
  AI --> Models[(ai-models volume)]
  AI -. optional .-> Ollama[Ollama profile]
  AI -. optional .-> Cloud[OpenAI-compatible endpoint]
  Browser -. offline cache and queued operations .-> Local[Browser localStorage and service worker]
~~~

### Web UI

- React + TypeScript + Vite；主要页面和用户流程位于 **web/src/App.tsx**，HTTP 与浏览器缓存辅助逻辑位于 **web/src/api.ts**，样式位于 **web/src/styles.css**。
- **web/public/manifest.webmanifest** 和 **web/public/sw.js** 提供 PWA 元数据和应用壳缓存。
- 服务端 UI 资源由 Docker 多阶段构建生成，再嵌入 Go 二进制。修改 Web UI 后，生产镜像构建必须经过根目录 **Dockerfile** 中的 Web build 阶段。
- Service worker 缓存同源的静态 GET 请求；明确跳过 /api/ 请求。词卡缓存、词典缓存、登录令牌和待同步操作由 Web 代码存入浏览器 localStorage。
- 最近查过的最多 250 条词典记录可用于离线填表。离线新增和修改等操作需联网后才能同步至服务器；初次登录与首次同步需要联网。

### Go API 和持久化

- **api/main.go** 使用 Go 标准库 HTTP 服务，同时提供嵌入式 Web UI、词卡与词典 API，以及到 AI 服务的代理。当前为单用户自托管形态，没有鉴权中间件。
- **api/types.go** 定义 API 和备份用的数据结构；修改字段时检查 Web TypeScript 类型与导入/导出兼容性。
- **api/store.go** 负责 SQLite schema、初始化词典、词卡和词典查询、复习事件、导入导出。SQLite 驱动为纯 Go 的 modernc.org/sqlite。
- **api/pronunciation.go** 将发音请求路由到本地 AI 服务或 Azure Speech REST；Azure Key 和区域保存在 SQLite 设置中，Key 不返回给浏览器。复习页按住发音按钮开始录音，松开后停止并提交评估；键盘用户可按住空格或回车，切页时清理录音流。浏览器录音转为 16 kHz 单声道 PCM WAV；本地结果含 ASR 转写、音素/单词分数或明确的 ASR 回退说明，Azure 结果由 Go API 归一化。发音模型状态和准备请求经 GET/POST `/api/pronunciation/model` 代理到 AI 服务。
- **api/fsrs.go** 实现 FSRS-4.5 默认参数与排程。目标保留率为 90%，评分范围是 1–4，当前间隔最少一天、最多 36500 天。
- review event 使用唯一事件 ID；重放同一评分请求时按事件 ID 去重，支撑离线操作重放。
- 批量收词请求记录在 batch_requests 表（request_id 主键 + 响应 JSON），重放同一请求返回首次结果；每请求上限 50 条。同一词 + 同一段 contextText 视为重复，不重建词卡；同词不同语境会新建词卡。
- Compose 中应用数据目录为 /data：数据库是 /data/vocab.db，生成的 WAV 缓存在 /data/audio。两者均由 vocab-data 命名卷持久化。
- 当前 schema 通过 CREATE TABLE IF NOT EXISTS 初始化，没有独立版本化迁移框架。修改既有列或数据格式时，应设计兼容迁移并检查旧备份恢复路径。

### Python AI 服务

- **ai/server.py** 是独立的轻量 HTTP 服务；Compose 网络内只由 Go API 调用，不直接发布宿主机端口。
- Kokoro 在 CPU 上按需生成美式英语 WAV，默认 voice 为 af_heart。首次使用时可能下载模型权重。
- 本地 faster-whisper 默认使用 base.en、CPU 和 int8；本地音素评分由 **ai/pronunciation_model.py** 加载固定 revision 的 Whisper Phoneme CTC、CMU 发音词典及其 GOP 回归器，在 0–2 模型尺度上计算音素并映射到 0–100 展示。设置页可以主动下载/加载约 96 MB 文件并显示模型状态；首次评分也会自动准备，文件缓存于 ai-models 卷。模型在每个 AI 进程中单例加载；同一段录音的多个词典读音候选复用一次 Whisper 声学编码，只分别执行音素对齐。单词短录音的 ASR 关闭 VAD 过滤，避免短语音被裁掉；模型或发音字典不可用时回退到 faster-whisper 转写比较并说明原因。AI 日志按请求输出 ASR、音频解码、模型加载、音素评分和总耗时，不记录录音或转写内容。Azure 模式由 Go API 直接调用 `en-US` 短音频发音评价 REST 接口。
- 模型分数是研究模型的练习反馈，不能作为客观正确率或口音诊断；界面必须保留不确定状态，并区分本地音素分数、ASR 文本回退和 Azure 分数。CMU 多读音词会尝试可用读音并选模型分数最高的变体。
- 文本释义默认关闭。可在 Web 设置页（保存于 SQLite settings 表）或通过 AI_TEXT_PROVIDER=openai-compatible 等 env 配置；Web 设置以 textAI 请求字段覆盖 env，UI 停用优先于 env 启用。API Key 只存服务器，GET /api/settings 仅返回掩码提示，不进入 JSON 备份。服务不可用、模型加载失败或响应格式异常时返回明确错误。
- 模型缓存写入 Compose 的 ai-models 卷。AI 容器下载模型时需要能够访问 Hugging Face；`/api/pronunciation/model` 的 GET 查询状态，POST 在后台准备模型。

## API 摘要

当前没有鉴权：/api/ 下的数据接口不要求令牌，依赖部署侧（默认仅绑定回环地址/反向代理）保护。

| 路径 | 用途 |
| --- | --- |
| GET /api/healthz | 应用健康检查，不要求令牌 |
| GET、POST /api/cards | 列出词卡、创建词卡；列表支持到期范围和关键词查询，服务端最多返回 2000 条 |
| POST /api/cards/batch | 批量收词：一次创建多条词卡并附加同一段原句和来源；按 requestId 幂等，逐条返回 created/duplicate/error |
| GET、PUT、DELETE /api/cards/{id} | 读取、编辑、删除词卡 |
| POST /api/cards/{id}/review | 提交评分和唯一复习事件 ID |
| GET /api/dictionary?q=... | 搜索本地词典 |
| POST /api/dictionary/import | 导入词典条目 |
| GET /api/export | 导出包含词卡、复习记录和词典的 JSON 备份 |
| GET、POST /api/sync | GET 返回服务端同步快照；POST 导入/合并快照 |
| POST /api/import | 导入/合并 JSON 备份 |
| POST /api/tts | 请求 Kokoro 生成语音；Go API 按 voice 与文本的哈希缓存 WAV |
| GET /api/audio/{hash}.wav | 读取缓存的 WAV |
| POST /api/pronunciation | 请求本地音素评测（失败时回退 ASR 转写）或 Azure Speech 发音评分 |
| GET、POST /api/pronunciation/model | 查询本地音素模型下载状态；后台下载并加载模型 |
| GET、PUT /api/settings | 读取/保存 AI 文本释义配置（Key 只存服务器，返回掩码） |
| POST /api/gloss | 请求可选文本 AI 释义 |

## 备份、同步和数据语义

- 备份版本当前为 1，包含 cards、reviews 和 dictionary。
- 导入按词卡 ID 合并/更新，复习记录按事件 ID 去重，词典按单词覆盖；导入不应被描述为清空数据库后完整替换。
- 删除词卡时关联复习记录受 SQLite 外键级联删除影响。导出后应妥善保存备份。
- TTS 音频和模型文件是可再生成的缓存，不在 JSON 备份中。部署备份还应保存 .env 中的 AI 配置。
- 浏览器 localStorage 是单设备缓存；服务器 SQLite 是跨设备同步数据源。清除浏览器站点数据之前，应先导出备份并确认服务器同步完成。

## Docker 与部署现状

- Compose 默认包含 **app** 和 **ai**；Compose 文件还定义可选 **ollama** 与 **caddy** 服务。
- 当前 Compose 镜像名为 ghcr.io/er1c-zh/vocab-selfhost:0.1.0 和 ghcr.io/er1c-zh/vocab-selfhost-ai:0.1.0。GHCR 包当前为私有包；拉取需有 read:packages 权限的 PAT。标签和仓库权限在后续发布时可能变化。
- app 默认绑定 127.0.0.1:8080；面向 iPhone 使用时应通过已有反向代理提供 HTTPS。普通 HTTP 的局域网 IP 页面无法满足 PWA 离线能力和浏览器麦克风的安全上下文要求。
- 启用 https profile 的 Caddy 会占用宿主机 80、443 TCP 和 443 UDP。部署到已有反向代理主机前先确认端口安排；不要在未检查现有服务的情况下启用该 profile。
- Ollama profile 使用独立 ollama-models 卷。默认文本 AI 关闭，因此启用 Ollama 服务本身不会自动配置释义模型。
- 命名卷：vocab-data 持久化 SQLite 和音频；ai-models 保存 Hugging Face/ASR 权重；ollama-models 保存 Ollama 模型；caddy-data 和 caddy-config 保存可选 Caddy 状态。
- .env 被 .gitignore 排除。不要读取、打印、记录或提交真实 AI API Key；文档示例只使用占位值。
- 可本机构建镜像，或登录 GHCR 后拉取 Compose 指定镜像。详细操作、词典文件格式和用户备份流程以 **README.md** 为用户文档入口，**.env.example** 为配置项示例。

## 当前实现状态

| 能力 | 当前状态 |
| --- | --- |
| 桌面/手机响应式 PWA，新增、复习、管理词卡 | 已实现 |
| 文本收词：粘贴段落自动分词、点选单词、拖选词组、批量添加并逐条反馈 | 已实现；批量接口按 requestId 幂等；词典没有的单词和词组会尝试用可选 AI 按语境生成释义，AI 失败不阻塞词条保存 |
| 同步状态机 idle/syncing/synced/offline/error，带超时、单飞与退避重试 | 已实现；左下角显示状态与待同步数量 |
| 原句、来源、语境释义保存 | 已实现 |
| 本地 SQLite、跨设备服务端同步和备份导入导出 | 已实现 |
| FSRS-4.5 天级排程，四级评分 | 已实现；无日内步骤配置 |
| 本地词典和 IPA | 已实现基础功能；内置词条很少，需自行导入完整词库 |
| Kokoro 美式英语语音及 WAV 缓存 | 已实现按需调用；首次使用需下载模型 |
| 录音识别和评分 | 默认本地 Whisper Phoneme CTC 返回词级/音素级分数并由 faster-whisper 转写；失败时回退文本匹配；设置页可预下载并查看模型状态；可选 Azure；都不是专业发音诊断 |
| 可选 Ollama 或兼容服务的 AI 释义 | 已实现可选接口；默认关闭，费用由用户配置的外部服务决定 |
| PDF 搜索、PDF 阅读和自动提取 | 未实现，且不属于当前产品范围 |
| 多用户、角色权限和用户隔离 | 未实现 |
| 批量收词词形还原（lemma） | 未实现；保存用户选中的原始词形，不擅自改写专业术语 |

仓库包含 Go、Web 与 Python 测试。Whisper Phoneme CTC 的真实权重下载和 Debian 设备录音尚需在部署环境中验证；单元测试不等同于真实发音效果校准。

## 后续修改约定

- 先确认改动符合上面的产品范围；任何“已实现”说明都要能在代码和实际流程中找到依据。
- 同步修改 Go DTO、Web TypeScript 类型、导入导出结构和 README，尤其是数据结构或 API 改动。
- 保留明确失败语义：词典无结果时不伪造词条；AI、网络、模型或语音解析失败时显示错误或不确定状态。
- 保持 AI 只通过 Go API 的受保护同源端点提供给 Web；不要把 Python 服务端口发布到公网。
- 新增 SQLite schema 时考虑已有 vocab.db、备份版本和跨设备同步兼容性。
- 更改离线操作时同时检查 service worker 缓存策略、localStorage 队列、操作幂等性和重连重放。
- 保持手机安全区域与窄屏布局可用；新增依赖前评估 Docker 镜像体积和首次启动成本。
- Debian/Linux 手工编辑配置文件的操作说明使用 vim；一组可连续执行的部署命令尽量集中在同一个代码块。
- 修改完成后按改动范围选择并执行 Go、Web、Python 或 Compose 验证；更新文档中的现状和验证记录。


