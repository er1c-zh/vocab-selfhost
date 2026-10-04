# Wordwell Self-host

自托管英语词汇学习 PWA。产品界面优先：桌面端使用侧边导航，手机端使用底部导航和安全区域适配。通过浏览器在电脑和 iPhone 上访问同一份服务器词库。

## 已实现

- 从复制的句子/段落里选中单词，保存原句、阅读来源、普通释义和语境释义；可直接手动录入，也可从本地词典带入 IPA 和释义。
- 文本收词：粘贴整句或段落后自动分词（保留原文偏移量），单击选择单词、拖动选择连续词组，待添加列表可修改和删除，已存在的词会标注；一次批量加入词库，结果按条反馈成功/重复/失败，原句和来源应用到整批词卡。
- 响应式 PWA：新增、复习、编辑、删除、搜索、服务端同步、JSON 备份/恢复；最近的卡片和查词可离线读取，离线新增/修改/删除/评分/批量收词会排队并在联网后重放。
- 同步状态机（idle/syncing/synced/offline/error）：请求带超时与单飞保护，失败按指数退避重试（有上限），待同步条数与失败原因会显示在左下角，可手动重试。
- Go API、SQLite 持久化、复习事件幂等 ID、FSRS-4.5 默认参数和 90% 目标保留率。首版按天排程，间隔最短为一天，暂不提供日内学习/重学步长设置。批量收词接口以请求 ID 幂等，重复提交返回首次结果。
- 独立 Python AI 服务：按需使用 Kokoro 美式英语 TTS，并把 WAV 缓存在 `/data/audio`；本地 faster-whisper 返回识别文本，Whisper Phoneme CTC 按目标词音素序列评测并显示词级/音素级结果；模型不可用时明确回退到转写比较。
- 可选 Azure Speech 发音评价：设置 Azure 区域和订阅 Key 后，浏览器把短录音转成 16 kHz 单声道 PCM WAV，由 Go API 调用 Azure 短音频发音评价接口；界面显示识别文本、服务置信度、整体与单词准确度，并在响应包含时显示音素准确度。
- 可选 OpenAI 兼容文本模型接口，可连接 Ollama 或云端兼容服务。默认禁用；不配置时会显示明确错误，不返回伪造释义。批量收词时，词典没有条目的单词和词组会尝试用 AI 按上下文生成释义，失败不影响词条保存；未配置 AI 时仅保存原句与来源。
- Docker 多阶段构建：构建 React/Vite 静态 PWA、编译嵌入 UI 的 Go API；AI 服务独立构建。Compose 持久化 SQLite/音频和模型缓存，并配置健康检查。

## 明确范围

- 本项目不搜索 PDF、不提供 PDF 阅读器。使用时在论文/PDF 阅读器里复制段落，再粘贴到“收集新词”页面。
- 仓库只附 10 条英语词典示例，不包含大型词典数据库。可以从“设置与数据”导入自己的 JSON 或 TSV 词典；TSV 每行依次为 `word<TAB>ipa<TAB>definition<TAB>locale`。没有本地词条时 IPA 不会被猜造，需要手动填写或导入词典。
- 默认本地模式用 Whisper Phoneme CTC 对齐目标词的音素并估算音素/单词分数，使用 faster-whisper 展示识别文本；模型不可用或词典没有目标词时回退到转写比较。可选 Azure 模式会给出准确度、流利度、完整度、韵律及服务响应中的音素分数。两种自动评分都只用于练习反馈，不是口音诊断或母语水平认证。
- PWA 的离线功能依赖设备浏览器缓存；服务器是跨设备数据源。首次登录和同步需联网。浏览器清理站点数据前请先导出备份。
- 语音模型权重缓存在 Compose 的 `ai-models` volume；这需要 Docker 主机能访问 Hugging Face。发音模型可在「设置与数据 → 发音评估」点击“下载本地模型”提前下载约 96 MB，并查看下载/加载状态；也会在第一次评分时自动准备。模型仓库提供者报告其在 SpeechOcean762 音素测试上的相关系数为 0.606；需要用实际设备录音继续验证。多音词会试算 CMU 词典读音并采用评分较高者。

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

`docker compose down` 会保留命名卷。若要彻底删除数据库和模型，需要另行删除 `vocab-data`、`ai-models`、`ollama-models` 卷。

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

默认本地模式除了 faster-whisper 转写外，还使用固定版本的 [Whisper Phoneme CTC](https://huggingface.co/vb223/whisper-base-en-phoneme-ctc)，将录音对齐到 CMU 发音词典中的目标音素，显示模型估算的单词和音素分数。无需 Azure Key，也无需 GPU。打开「设置与数据 → 发音评估」可点击“下载本地模型”，页面会显示模型未下载、下载/加载中、已下载、已就绪或失败状态；约 96 MB 的文件保存在 Compose `ai-models` volume。也可以不预下载，首次评分时自动准备。模型代码按 MIT 授权随 AI 服务构建，模型权重和回归器按固定 revision 下载，不从模型仓库执行 Python 源码。

练习时按住发音按钮开始采集，说完松开按钮结束并提交评估；键盘用户可按住空格或回车录音。松开或录音被系统取消时会结束采集，离开页面也会清理麦克风。单词录音关闭 VAD 语音端点过滤，避免短词被误判为静音；如果音素模型有评分但 ASR 没有转写，界面会分别说明，不会把空转写写成“未识别到语音”。

如果一次评估仍耗时较长，可查看分阶段耗时。AI 日志会显示 ASR 初始化/推理、音频解码、音素模型初始化、音素评分和候选读音数量；Go API 日志会注明请求实际走本地还是 Azure。日志不包含录音或转写内容：

```sh
docker compose logs --since=15m ai app
```

本地长驻的 AI 进程会复用已加载的模型；`phoneme_model_load_s` 只有首次加载或进程重启后的首个请求应明显偏高。`candidates` 显示同一词实际比较的词典读音数量。

该模型是在 SpeechOcean762 英语学习者数据集上训练的研究模型。发布者报告的音素级测试相关系数为 0.606；这个数值不是“正确率”，项目也没有足够的本地学习者样本来校准分数。它可能受录音质量、词典读音和个人口音影响；分数只用于日常练习参考。如果模型首次下载或本地音素对齐失败，接口会显示说明并回退到 ASR 转写比较。Azure 模式仍可在设置中切换。

## 词典格式

JSON 可以是条目数组，也可以是 `{ "entries": [...] }`。字段为 `word`、`ipa`、`definition`、可选 `locale`。TSV 示例：

```text
evidence	ˈɛvɪdəns	information supporting a conclusion	en-US
context	ˈkɑːntɛkst	the circumstances around an event	en-US
```

导入在“设置与数据 → 本地词典”。词典存在 SQLite 中，词条精确匹配时自动填充释义和 IPA；浏览器会缓存最近查过的 250 个词条以便离线填表。

## 备份与恢复

在“设置与数据”导出 JSON，会包含卡片、原句、来源、FSRS 状态、复习事件和已导入词典；同一页面选择 JSON 可恢复/合并。TTS WAV 是派生缓存，可以在需要时重新生成，数据库和它们都在 `vocab-data` volume。另请单独保存 `.env` 中的 AI 配置。

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

2026-10-04 发音耗时排查：确认 ASR 与音素模型在同一 AI 进程内使用单例缓存和锁，不会正常地每次请求重新初始化；修复多读音候选重复执行声学编码、缓存 CMU 词典，并增加各阶段耗时日志。Python 17 项和 Go API 测试通过。真实 Debian CPU 上的耗时仍需从新镜像日志确认；录音界面的固定采集时间约 5 秒。

本机开发环境可以分别运行 Go API 和 Vite 开发服务器；生产容器由 Compose 启动。AI 推理和 Docker 镜像体积较大，建议通过 Docker 构建验证完整部署。

