# 我们的 fork（ProxyHub）

上游：<https://github.com/massiveits/opencode-go-cliproxyapi>
我们的仓库：<https://github.com/FongKunm/opencode-go-cliproxyapi>

这个 fork 由我们自己维护：上游有什么问题、我们自己发现了什么坑，都在这里补上，
不依赖上游的发版节奏。上游更新照常可以合并进来（见「同步上游」）。

## 我们改了什么

| 版本 | 改动 | 说明 |
|---|---|---|
| `0.2.2` | 上游原版 | 包含 tool_result 图片修复（PR #16） |
| `0.2.2-ph1` | **实现 `executor.count_tokens`** | 官方实现是直接回 `unsupported`，Claude Code / Claude Desktop 每轮的 `/v1/messages/count_tokens` 全部 400。详见下 |
| `0.2.2.2` | **历史内容预处理、思考回传与档位、保活、后缀名** | 见下「0.2.2.2」。从这一版起版本号用纯数字四段 |

### count_tokens（`internal/plugin/counttokens.go`）

- 官方插件没有 token 计数；客户端拿不到计数时，自动压缩的「执行」阶段会失败
  （分组拿不到数据 → 摘要算出来应用不上 → 上下文一路涨到上游上限）。
- 实现是**本地估算**，格式无关（递归遍历 JSON 字符串值），Anthropic Messages /
  OpenAI Chat / Responses 三种入站都能用：
  - 只数会进模型的内容：文本、思考、工具参数、工具结果、工具定义；
  - 图片（base64 或 `data:image/` URL）按块固定 1000，不按体积折；
  - ASCII 4 字节/token，非 ASCII 每字 1 token；返回值 ≥ 1。
- 返回格式按客户端协议：`claude` / `openai` → `{"input_tokens":N}`；
  `openai-response` → `{"response":{"usage":{...}}}`。
- **不要改成 `len(body)/4`**：JSON 骨架和 base64 图片会被整体高估。

### 0.2.2.2（1020）

- **历史里 DeepSeek 不认的内容不再 400**（`internal/adapter/sanitize`、`internal/pdftext`）：从真 Claude / GPT
  中途切到 DeepSeek，或 Claude Code 直接发 PDF 时，以前每一轮都 400、只能清空对话。现在转换前先预处理：
  PDF 抽正文（内置加固过的 ledongthuc/pdf，见 `internal/thirdparty/pdf`）、搜索 / 抓取 / 代码执行结果转成文字、
  托管工具（web_search_* 等）从 tools 里去掉；认得的内容原样不动。Codex 的压缩记录、web_search_call 等同理。
- **思考内容回传给客户端**：reasoning_content 转成 Claude 的 thinking 块 / Responses 的 reasoning 项（流式、非流式）；
  历史里的思考作为 reasoning_content 回传给 DeepSeek。
- **思考档生效**：Claude 的 `output_config.effort`、`thinking.type=disabled`、enabled+budget 都换算成
  reasoning_effort，DeepSeek 按完整 7 档（none～max）夹紧。
- **保活**：下游超过 15 秒没输出就补一帧（Claude `event: ping`，Responses SSE 注释，Chat 入站不发）。
- **模型名带思考档后缀**（如 `deepseek-v4-pro(high)`）：剥掉后缀查目录，后缀换成 reasoning_effort；
  目录里真查不到回 400（以前回 404，会让 CPA 把模型冷却 12 小时）。
- 跑模糊测试一律加 `-parallel 4`、多包测试加 `-p 4`（开发机 24 核，默认并发会占满整机）。

## 版本号规则（必须遵守）

宿主选用哪个 .so **只看文件名里的版本**（`<插件ID>-v<版本>.so`）；插件自报的
`pluginVersion` 只在文件名不带版本时兜底，另外用于日志和管理页显示。两者保持一致，日志好对照。
（`0.2.2-ph1` 时这里曾写成"等值比对、不一致执行器不可用"，1020 读宿主源码确认不对，已更正。）

真正的坑：

1. **用纯数字版本号**：同一插件目录里有多个 .so 时宿主按版本挑一个，遇到非数字段会退回字符串比较，
   `0.2.2-ph10` 会排在 `0.2.2-ph9` 前面。从 `0.2.2.2` 起改成「上游版本 + 第四段序号」：
   上游 0.2.2 上的第 N 个补丁版叫 `0.2.2.N`；合并上游 0.2.3 之后从 `0.2.3.1` 重新数。
2. **旧版一定先改名 `.bak` 再重启**：宿主启动时会**直接删掉**同一插件里没被选中的 .so。
3. **不要在 CPA 管理面板的插件商店里"更新"这个插件**：商店地址（`githubRepoURL`）已改指我们的 fork，
   但面板装的是 Release 产物，和我们手动编译上线的流程不同步，容易装回旧版或上游原版。

## 构建

CI 在 `.github/workflows/build.yml`：推 `v*` 标签会构建全平台并产出 zip
（workflow_dispatch 的版本号是 `0.0.0-dev`，只用来验证能编过）。

服务器上没有 Go/gcc，用 Docker 编（我们平时就用这条）：

```bash
git clone --depth=1 --branch main https://github.com/FongKunm/opencode-go-cliproxyapi.git /root/opencode-go-build
cd /root/opencode-go-build
docker run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=1 -e GOFLAGS=-trimpath \
  golang:1.26-bookworm \
  go build -buildmode=c-shared -ldflags "-s -w" \
  -o /src/out/opencode-go-cliproxyapi-v<版本>.so .
rm -f /src/out/opencode-go-cliproxyapi-v<版本>.h
```

## 上线

```bash
P=/root/cli-proxy-api/plugins/linux/amd64
cp /root/opencode-go-build/out/opencode-go-cliproxyapi-v<版本>.so "$P"/
# 旧版改名成 .bak（宿主只认 .so，不会加载 .bak）
mv "$P"/<旧版>.so "$P"/<旧版>.so.bak
chmod 644 "$P"/opencode-go-cliproxyapi-v<版本>.so
docker restart cli-proxy-api
```

验收：`docker logs --tail 100 cli-proxy-api | grep opencode` 要看到
`plugin registered ... version=<版本>`，且版本与文件名一致。

## 同步上游

```bash
git remote -v          # origin = 我们的 fork，upstream = massiveits
git fetch upstream
git checkout main
git merge upstream/main   # 有冲突就按我们的改动优先解决
git push origin main
```

合并后：跑 `go test ./...`，重新构建、重新上线（版本号跟着改）。
