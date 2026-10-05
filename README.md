# Wordwell Self-host

自托管英语词汇学习 PWA。产品界面优先：桌面端使用侧边导航，手机端使用底部导航和安全区域适配。通过浏览器在电脑和 iPhone 上访问同一份服务器词库。

## 已实现

- 从复制的句子/段落里选中单词，保存原句、阅读来源、普通释义和语境释义；可直接手动录入，也可从本地词典带入 IPA 和释义。
- 文本收词：粘贴整句或段落后自动分词（保留原文偏移量），单击选择单词、拖动选择连续词组，待添加列表可修改和删除，已存在的词会标注；一次批量加入词库，结果按条反馈成功/重复/失败，原句和来源应用到整批词卡。
- 响应式 PWA：新增、复习、编辑、删除、搜索、服务端同步、JSON 备份/恢复；最近的卡片和查词可离线读取，离线新增/修改/删除/评分/批量收词会排队并在联网后重放。
- “浏览”页可逐张翻看词卡；词库中点单词即可从该卡开始浏览，桌面侧栏和手机底栏都有入口。
- 同步状态机（idle/syncing/synced/offline/error）：请求带超时与单飞保护，失败按指数退避重试（有上限），待同步条数与失败原因会显示在左下角，可手动重试。
- Go API、SQLite 持久化、复习事件幂等 ID、FSRS-4.5 默认参数和 90% 目标保留率。首版按天排程，间隔最短为一天，暂不提供日内学习/重学步长设置。批量收词接口以请求 ID 幂等，重复提交返回首次结果。
- 独立 Python AI 服务：Kokoro 美式英语 TTS；新增词卡后后台排队预生成，网页播放请求以最高优先级插入等待队列；WAV 保存在独立持久化目录。本地 faster-whisper 返回识别文本，Whisper Phoneme CTC 按目标词音素序列评测并显示词级/音素级结果；模型不可用时明确回退到转写比较。
- 可选 Azure Speech 发音评价：设置 Azure 区域和订阅 Key 后，浏览器把短录音转成 16 kHz 单声道 PCM WAV，由 Go API 调用 Azure 短音频发音评价接口；界面显示识别文本、服务置信度、整体与单词准确度，并在响应包含时显示音素准确度。
- 可选 OpenAI 兼容文本模型接口，可连接 Ollama 或云端兼容服务。默认禁用；不配置时会显示明确错误，不返回伪造释义。批量收词时，词典没有条目的单词和词组会尝试用 AI 按上下文生成释义，失败不影响词条保存；未配置 AI 时仅保存原句与来源。
- Docker 多阶段构建：构建 React/Vite 静态 PWA、编译嵌入 UI 的 Go API；AI 服务独立构建。Compose 分别持久化 SQLite、WAV 音频和模型文件，并配置健康检查。
- “设置与数据”显示已保存读音的文件数和占用空间，并提供清理按钮；“监控”页显示 App/AI 资源、任务队列、模型状态和应用/AI 近期日志。

## 明确范围

- 本项目不搜索 PDF、不提供 PDF 阅读器。使用时在论文/PDF 阅读器里复制段落，再粘贴到“收集新词”页面。
- 仓库只附 10 条英语词典示例，不包含大型词典数据库。可以从“设置与数据”导入自己的 JSON 或 TSV 词典；TSV 每行依次为 `word<TAB>ipa<TAB>definition<TAB>locale`。没有本地词条时 IPA 不会被猜造，需要手动填写或导入词典。
- 默认本地模式用 Whisper Phoneme CTC 对齐目标词的音素并估算音素/单词分数，使用 faster-whisper 展示识别文本；Kokoro、ASR 和音素评分模型会在 AI 容器启动后依次预加载并常驻内存。模型不可用或词典没有目标词时回退到转写比较。可选 Azure 模式会给出准确度、流利度、完整度、韵律及服务响应中的音素分数。两种自动评分都只用于练习反馈，不是口音诊断或母语水平认证。
- PWA 的离线功能依赖设备浏览器缓存；服务器是跨设备数据源。首次登录和同步需联网。浏览器清理站点数据前请先导出备份。
- 语音模型权重默认缓存在 Compose 的 `ai-models` volume；可用 `AI_MODEL_STORAGE_PATH` 改为宿主机或 NAS 的绝对路径，避免镜像更新或重建时重新下载。AI 服务首次启动需要访问 Hugging Face。设置页显示 ASR、音素模型和 Kokoro 的加载状态，也可手动重试准备音素模型。Whisper Phoneme CTC 模型仓库约 96 MB；模型仓库提供者报告其在 SpeechOcean762 音素测试上的相关系数为 0.606，需要用实际设备录音继续验证。多音词会试算 CMU 词典读音并采用评分较高者。

## 启动

需要 Docker Desktop / Docker Engine 与 Docker Compose v2。Compose 默认拉取 `ghcr.io/er1c-zh/vocab-selfhost:0.1.0` 与 `ghcr.io/er1c-zh/vocab-selfhost-ai:0.1.0` 两个 GHCR 镜像；首次拉取私有包需要用具有 `read:packages` 权限的 GitHub PAT 登录（`docker login ghcr.io -u er1c-zh`）。默认仅绑定本机回环地址。Windows PowerShell：

```powershell
Copy-Item .env.example .env
docker login ghcr.io -u er1c-zh
docker compose pull
docker compose up -d
docker compose ps
```

需要本地从源码构建时，可用 `docker compose up -d --build` 替代拉取命令。然后打开 <http://localhost:8080> 即可使用。

如需让 iPhone 通过家庭局域网访问，把 `.env` 的 `APP_BIND` 改为 `0.0.0.0`，并让手机和电脑访问同一个 HTTPS 域名。PWA 离线缓存和麦克风在局域网 IP 的普通 HTTP 页面上不可用。仓库提供可选 Caddy 服务：将 `.env` 的 `WORDWELL_DOMAIN` 设为指向这台主机的域名后启动；公网证书要求 DNS 和路由器的 80/443 端口可达。仅家庭局域网使用时，可把 `deploy/Caddyfile` 改为 Caddy 内部 TLS，并将 Caddy 的本地根证书安装到手机。本项目未启用鉴权，请勿把服务端口直接暴露到公网或不受信任的网络。

```powershell
# .env 中设置真实域名后
docker compose --profile https up -d
```

停止服务：

```powershell
docker compose down
```

`docker compose down` 会保留命名卷。若要彻底删除数据库、音频和模型，需要另行删除 `vocab-data`、`vocab-audio`、`ai-models`、`ollama-models` 卷。

### 读音文件存储与 NAS

默认情况下，生成的 WAV 文件放在 Docker 命名卷 `vocab-audio`，和数据库分开持久化；升级时旧版 `/data/audio` 中的文件会迁移到新目录 `/audio`。需要查看空间时，可在网页「设置与数据 → 读音存储」查看文件数与总大小，或执行：

```sh
docker compose exec app du -sh /audio
```

如果 NAS 已挂载到 Debian/WSL 主机，把 `.env` 中的 `AUDIO_STORAGE_PATH` 改为挂载路径。例如：

```sh
vim .env
# 设置 AUDIO_STORAGE_PATH=/mnt/nas/wordwell-audio
docker compose up -d
```

宿主机目录需允许容器 UID `10001` 写入；新建目录时可按你的 NAS 权限策略设置属主或 ACL。Compose 会将该路径挂载到容器 `/audio`。读音管理中的“清理已保存读音”会删除这个目录中的 WAV，并取消等待中的生成任务；词卡不会被删除，之后播放时会重新生成。JSON 备份不包含 WAV，NAS 目录应单独备份。

### AI 模型目录与 NAS

AI 模型默认保存在 Compose 命名卷 `ai-models`，不是镜像临时层中的文件。要把模型放到已挂载的 NAS 路径，可在部署目录编辑 `.env`：

```sh
vim .env
# 设置 AI_MODEL_STORAGE_PATH=/mnt/nas/wordwell-models
docker compose up -d
```

目录会挂载到 AI 容器 `/models`。NAS 路径必须允许容器 UID `10002` 读写。保留 `AI_MODEL_STORAGE_PATH=ai-models` 时继续使用 Docker 命名卷；执行 `docker compose down` 不会删除命名卷，`docker compose down -v` 会删除它。镜像只包含程序，不包含下载的模型权重。

### 监控和网页日志

网页侧栏“监控”每 10 秒刷新应用与 AI 容器的内存、AI CPU、读音占用、TTS 队列和发音模型任务状态。页面显示应用与 AI 服务各自最近保留的日志；日志缓冲仅在进程内，容器重启后清空，需要完整历史时仍使用：

```sh
docker compose logs -f --since=15m --tail=100 ai app
```

## 可选 Ollama / OpenAI 兼容释义

AI 文本释义为中英对照输出（「英文释义 ｜ 中文释义」，语境释义为中文），可以直接在网页「设置与数据 → AI 文本释义」中配置：启用开关、Base URL、模型名和 API Key，保存后可用「保存并测试」立即验证。配置保存在服务器的 SQLite `settings` 表中，优先于环境变量；API Key 只保存在服务器（GET 接口只返回掩码提示），也不会随 JSON 备份导出。

Ollama 作为可选 Compose profile，不会随默认启动下载模型，先启动并拉取模型：

```powershell
docker compose --profile ollama up -d
docker compose exec ollama ollama pull qwen2.5:3b
```

然后在设置页填入 `http://ollama:11434/v1` 和 `qwen2.5:3b` 即可（本地 Ollama 无需 API Key）。也可以在 `.env` 里以环境变量方式配置（两处都不配置时，AI 释义会明确报错而不是猜造）：

```dotenv
AI_TEXT_PROVIDER=openai-compatible
AI_TEXT_BASE_URL=http://ollama:11434/v1
AI_TEXT_MODEL=qwen2.5:3b
AI_TEXT_API_KEY=
```

接入兼容云端服务时，把 Base URL、模型名和 API Key 改为对应值。云服务费用由其提供方决定；默认的本地词典、FSRS、SQLite 和本地语音路径不需要第三方 API Key。

## 可选 Azure 发音评价

打开「设置与数据 → 发音评估」，选择 Azure，填写 Speech 资源所在区域（例如 `eastasia`）和订阅 Key，然后保存。Key 存在服务器 SQLite 设置中，接口只返回是否已配置和掩码提示，不会发给浏览器，也不会进入 JSON 备份。录音经 Go API 发给 Azure Speech 处理，费用和音频数据处理遵循 Azure 账户与服务条款；未选择 Azure 时使用下方的本地音素评测。

发音按钮录制短音频后会在浏览器转换为 16 kHz 单声道 PCM WAV。Azure 模式按 `en-US` 目标词做 scripted pronunciation assessment；评估结果含识别文本、置信度和评分，具体音素明细以服务响应为准。

## 本地音素评测

默认本地模式除了 faster-whisper 转写外，还使用固定版本的 [Whisper Phoneme CTC](https://huggingface.co/vb223/whisper-base-en-phoneme-ctc)，将录音对齐到 CMU 发音词典中的目标音素，显示模型估算的单词和音素分数。无需 Azure Key，也无需 GPU。AI 容器启动后会依次下载/加载 Kokoro、faster-whisper 和音素评分模型，成功加载的模型在进程运行期间常驻内存。设置页会分别显示三者加载状态，也保留了手动准备音素模型的入口；约 96 MB 的音素模型文件保存在 `/models` 持久化挂载中。模型代码按 MIT 授权随 AI 服务构建，模型权重和回归器按固定 revision 下载，不从模型仓库执行 Python 源码。

练习时按住发音按钮开始采集，说完松开按钮结束并提交评估；手机页面会显示录音浮层，向上滑动可取消当前录音，取消时不会发送评测请求。评测结果不会自动显示释义。键盘用户可按住空格或回车录音。麦克风流在应用页面存活期间复用，松开或取消后禁用音轨，切换复习卡/页面不需要再次调用麦克风；关闭或刷新页面时由浏览器回收流。浏览器是否记住跨页面/重启的权限仍由浏览器和系统权限设置决定。单词录音关闭 VAD 语音端点过滤，避免短词被误判为静音；如果音素模型有评分但 ASR 没有转写，界面会分别说明，不会把空转写写成“未识别到语音”。

如果一次评估仍耗时较长，可实时查看分阶段耗时。在部署目录运行下面的命令，然后在页面评估一个词；停止查看时按 `Ctrl+C`。日志不包含录音或转写内容：

```sh
docker compose logs -f --since=15m --tail=100 ai app
```

AI 会输出以 `Pronunciation timing:` 开头的阶段日志：`asr_load_s` 和 `phoneme_model_load_s` 是模型准备时间；`asr_inference_s` 和 `phoneme_score_s` 是计算时间；`total_s` 是 AI 端总耗时；`candidates` 是比较的词典读音数量。Go API 同时输出 `Pronunciation assessment provider=... duration=...`，表示实际走本地还是 Azure，以及 API 请求总时长。本地长驻的 AI 进程会复用已加载的模型；模型准备字段只有首次加载或进程重启后的首个请求应明显偏高。

AI 容器启动后会在后台依次下载并加载 Kokoro、faster-whisper 和 Whisper Phoneme CTC，逐项输出 `Startup model preload ready` 或失败日志；健康检查在预加载期间仍可响应。成功加载的模型在 AI 进程存活期间常驻内存，容器重启后从 `/models` 持久化挂载读取。`/healthz` 与设置页提供 TTS、ASR、音素模型各自的状态。Go API 使用单 worker 的 TTS 优先队列：新词、批量收词和导入卡片进入后台队列；复习、浏览和设置页试听发起的请求优先于所有等待任务，同一文本/音色只生成一次。正在运行的 Kokoro 推理不可中断，播放请求会在当前推理结束后先于其余排队任务执行。每次生成的 WAV 会保留在 `/audio` 挂载目录，直到用户在设置页手动清理。AI 日志 `TTS timing:` 分别给出 `model_load_s`（模型加载或等待）、`inference_queue_s`（AI 内部推理锁等待）、`inference_s`（语音生成）和 `encode_s`（WAV 编码）；Go 日志记录队列优先级、AI 耗时、文件大小和传输耗时，不记录单词或句子内容。如果朗读按钮正在等待，会显示忙碌状态并阻止重复提交。浏览器请求最长等待 7 分钟，Go API 对 AI 的上游超时为 6 分钟。

如果日志出现 `open() got an unexpected keyword argument 'metadata_errors'`，说明 faster-whisper 与 PyAV 版本不兼容，ASR 转写会失败但音素评分仍可返回。AI 镜像将 PyAV 限制在 19 以下以避免此错误；更新后再看日志，确认 `asr_inference_s` 大于 0 且不再出现该异常。

该模型是在 SpeechOcean762 英语学习者数据集上训练的研究模型。发布者报告的音素级测试相关系数为 0.606；这个数值不是“正确率”，项目也没有足够的本地学习者样本来校准分数。它可能受录音质量、词典读音和个人口音影响；分数只用于日常练习参考。如果模型首次下载或本地音素对齐失败，接口会显示说明并回退到 ASR 转写比较。Azure 模式仍可在设置中切换。

## 词典格式

JSON 可以是条目数组，也可以是 `{ "entries": [...] }`。字段为 `word`、`ipa`、`definition`、可选 `locale`。TSV 示例：

```text
evidence	ˈɛvɪdəns	information supporting a conclusion	en-US
context	ˈkɑːntɛkst	the circumstances around an event	en-US
```

导入在“设置与数据 → 本地词典”。词典存在 SQLite 中，词条精确匹配时自动填充释义和 IPA；浏览器会缓存最近查过的 250 个词条以便离线填表。

## 备份与恢复

在“设置与数据”导出 JSON，会包含卡片、原句、来源、FSRS 状态、复习事件和已导入词典；同一页面选择 JSON 可恢复/合并。JSON 不含 TTS WAV；这些文件是持久化数据，单独保存在 `vocab-audio` volume 或配置的 NAS 目录中，需单独备份。另请单独保存 `.env` 中的 AI 配置。

## 开发与验证

```powershell
cd api
go test ./...
cd ..\web
npm ci
npm test
npm run build
New-Item -ItemType Directory -Force ..\api\web\dist | Out-Null
Remove-Item -Path ..\api\web\dist\* -Recurse -Force
Copy-Item -Recurse -Force dist\* ..\api\web\dist\
cd ..
python -m unittest discover -s ai -p 'test_*.py'
```

Go 测试覆盖批量收词 API、发音配置隔离、Azure REST 请求参数/评分响应映射和失败处理，以及本地发音模型状态/下载代理。Python 测试覆盖本地 ASR 回退、短词录音不启用 VAD、音素词典映射、多读音共用一次声学编码、多音词选择和分数映射。Web 的 vitest 覆盖英文分词（缩写、连字符、Unicode、PDF 断行、偏移量映射）和同步状态机（成功、超时、服务器错误、离线、丢弃永久失败操作、同步中再次变更）。修改 Web 代码后需要重新构建并把 `web/dist` 复制到 `api/web/dist`，Go 二进制嵌入的是后者的内容。

2026-10-04 本地音素模型集成验证：Python 单元测试 15 项、Go API 测试、Web 测试 29 项和 Web 生产构建均通过。当前环境未安装 Docker，因此没有验证容器镜像构建。未在此工作环境下载或执行模型权重；Debian 部署首次评分需要联网拉取约 96 MB 权重，并应使用真实设备录音验证。

2026-10-04 模型状态/手动准备入口、短词 ASR 设置、结果重试和播放图标改动验证：Python 单元测试 16 项、Go API 测试、Web 测试 29 项及 Web 生产构建通过。没有在本次验证中联网下载模型，也未使用真实麦克风录音；部署时可先在设置页准备模型，再用实际设备录音确认识别质量。

2026-10-04 发音耗时排查：确认 ASR 与音素模型在同一 AI 进程内使用单例缓存和锁，不会正常地每次请求重新初始化；修复多读音候选重复执行声学编码、缓存 CMU 词典，并增加各阶段耗时日志。Python 17 项和 Go API 测试通过。真实 Debian CPU 上的耗时仍需从新镜像日志确认；录音按住开始、松开结束，时长由用户控制。
2026-10-04 ASR 兼容修复：faster-whisper 1.2.1 与 PyAV 19 因 `metadata_errors` 参数变更而不兼容；约束 `av>=11,<19`，WSL 镜像构建解析到 av 18.1.0 并推送 AI 镜像。Debian 运行时仍需在更新后通过 `asr_inference_s` 和转写结果确认。
2026-10-04 朗读延迟排查：发现 Kokoro 冷启动/模型下载发生在首个 TTS 请求中，浏览器原 30 秒超时短于 Go 的 6 分钟上游超时，且朗读可重复点击造成排队。延长首次请求等待时限、显示按钮忙碌状态、合并同一文本/音色的并发缓存未命中请求，并增加 AI 模型加载/排队/推理及 Go 缓存/传输耗时日志。Go、Web、Python 编译均通过；WSL 构建并推送 app（`sha256:a23c2cd0a3c46471053c7949eb02ccfa6d777b070621b015ddd441896e8a9d7c`）和 AI（`sha256:be2b7f65bae2b2ba817e0c0c897530b91f94497951da8c8ecfb7612e25c9bf66`）镜像。Debian 实际瓶颈仍需从运行后的 `TTS timing` 和 `TTS request` 日志确认。
2026-10-05 Kokoro 启动预加载：AI 服务启动时后台加载 Kokoro pipeline，向 `/healthz` 返回模型状态，并输出预加载完成或失败与耗时；健康检查仍可在预加载期间响应，避免模型下载阻塞 AI 服务启动。Python 语法检查通过，WSL AI 镜像构建并推送至 `ghcr.io/er1c-zh/vocab-selfhost-ai:0.1.0`，摘要 `sha256:e8ef6eaf8ff0aff41d3b976672fcb8f6fa878d49758397c448ba792f3e7ba0c9`；Debian 容器启动日志尚待确认。
2026-10-05 持久化语音和运维入口：新增 TTS 后台预生成队列与 UI 优先级、独立 `/audio` 挂载目录和旧 WAV 迁移、读音统计/清理、卡片浏览页，以及进程内近期日志和应用/AI 资源与任务监控。Go、Web、Python 和 Compose 配置验证通过；镜像发布与 Debian/NAS 实际挂载、权限和资源读数待部署确认。
2026-10-05 发音模型预加载与录音交互：启动时按序预加载 Kokoro、faster-whisper ASR 和 Whisper Phoneme CTC，状态进入 `/healthz`、metrics 和设置页；新增 `AI_MODEL_STORAGE_PATH` 持久化挂载。手机录音支持上滑取消及全屏状态浮层，结果和释义分离，复习页面内复用麦克风流。Python 17 项、Go 测试、Web 生产构建、WSL Compose 配置检查和 app/AI 镜像本地构建通过；AI 镜像确认声明 `/models` volume。未在构建时下载模型权重，也未使用真实设备录音；新镜像未推送，Debian 上的模型常驻内存、NAS 写入权限和麦克风体验仍需部署验证。

本机开发环境可以分别运行 Go API 和 Vite 开发服务器；生产容器由 Compose 启动。AI 推理和 Docker 镜像体积较大，建议通过 Docker 构建验证完整部署。

