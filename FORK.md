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

## 版本号规则（必须遵守）

宿主从 **文件名** 里解析版本（`<插件ID>-v<版本>.so`），再和插件自报的
`pluginVersion` 做**等值比对**；不一致时执行器直接不可用。

所以：改了 `pluginVersion`，文件名必须同时改。

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
