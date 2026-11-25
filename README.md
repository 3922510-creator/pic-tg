# Pic TG

基于 Telegram 私有频道与 Go 单文件构建的自托管个人图床。内置纯原生 Web 工作台、SQLite 本地索引、短链代理转发、上传前本地打码与质量压缩，不落盘缓存图片字节。

> **免责声明**：本项目全程由 AI 开发，为个人实验性项目，**请勿在生产环境中使用**。本项目按现状提供，不提供安全更新保证。如发现安全漏洞，请通过私信告知；本人会尽力修复，但不承诺修复时限。

## 特性

- 单二进制部署：Go 后端 + `go:embed` 内嵌静态资源 + 纯 Go SQLite（`modernc.org/sqlite`，无需 CGO）。
- Telegram 频道存储：以 `sendDocument` 原图存储至自有 Telegram 频道/群组，支持多 Bot/频道热切换并保留历史图片回源能力。
- 代理直出与并发回源合并：图片通过 `/i/{token}.{ext}` 短链提供稳定访问，支持 `ETag` / `If-None-Match` 304 协商缓存与并发回源请求合并（避免热图穿透），服务端不落盘保存图片与缩略图。
- 无损与按需压缩：前端支持上传前画质滑块（默认 95% / 原画质 / WebP）与本地矩形/马赛克打码；服务端在检测到 `jpegtran` / `cwebp` 时自动执行无损编码优化（未安装时静默保留原图），保留动图（GIF / APNG / 动画 WebP）与 EXIF/色彩配置。
- 安全边界清晰：
  - 首次启动自动生成随机登录密码与独立 API Key（配置文件权限 `0600`），避免默认免密暴露。
  - 登录密码与 API Key 权限隔离：API Key 仅用于上传/查询/下载/删除图片，无法读取或修改后台配置与 Telegram Bot Token。
  - 按客户端 IP 的认证失败速率限制与锁定时窗；反向代理真实 IP 头仅在显式配置 `trust_proxy_header` 且来自回环代理时生效。
  - 严格 CORS 白名单与静态资源哈希版本缓存。

## 快速开始

### 1. 一键交互式安装（推荐，Linux + systemd）

将域名 A/AAAA 记录解析到服务器 IP 后，运行：

```bash
curl -fsSL https://raw.githubusercontent.com/3922510-creator/pic-tg/main/install.sh | sudo bash
```

脚本会交互式引导完成以下配置（支持 `linux/amd64` 与 `linux/arm64`，优先下载带 SHA256 校验的 Release 二进制，无预编译包时自动回退源码编译）：

- 输入图床域名并进行 DNS 预检
- 选择是否经过 Cloudflare 代理（自动配置 `trust_proxy_header`）
- 设置本地监听端口、安装目录与初始登录密码（留空自动生成随机强密码与独立 API Key）
- 可选安装无损图片优化工具（`jpegtran` / `cwebp`）
- 自动安装并配置 Caddy（独立写入 `/etc/caddy/conf.d/pic-tg.caddy`，不覆盖已有站点配置，自动签发并续期 HTTPS 证书）
- 创建非 root 系统用户 `pictg` 与加固的 `pic-tg.service`，启动并完成本地健康检查

无损升级与卸载：

```bash
# 无损升级二进制（保留 config.json 与图库数据，失败自动回滚）
curl -fsSL https://raw.githubusercontent.com/3922510-creator/pic-tg/main/install.sh | sudo bash -s -- --upgrade

# 卸载服务（可选择是否保留数据目录）
curl -fsSL https://raw.githubusercontent.com/3922510-creator/pic-tg/main/install.sh | sudo bash -s -- --uninstall
```

如需全非交互自动化部署，可传入参数，例如：

```bash
curl -fsSL https://raw.githubusercontent.com/3922510-creator/pic-tg/main/install.sh | sudo bash -s -- \
  --domain pic.example.com --port 8798 --cloudflare no --caddy yes --optimizers yes --yes
```

安装完成后访问 `https://你的域名`，使用输出面板中的初始密码登录，前往「设置 -> Telegram 存储」填入 Bot Token 与目标频道/群组 Chat ID 即可开始使用。

### 2. 源码构建

需要 Go 1.26+（可选安装 `jpegtran` 与 `cwebp` 以启用服务端无损优化）：

```bash
git clone https://github.com/3922510-creator/pic-tg.git
cd pic-tg
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o pic-tg .
./pic-tg -config ./config.json
```

首次启动且 `config.json` 不存在时，程序会自动初始化 `./config.json`（权限 `0600`）并生成随机 `auth_token`（Web 登录密码）与 `api_key`（独立 API Key），请查看该文件获取初始凭据。

### 3. Docker 运行

```bash
docker build -t pic-tg .
docker run -d \
  --name pic-tg \
  -p 127.0.0.1:8798:8798 \
  -v "$(pwd)/data:/data" \
  pic-tg
```

容器中默认监听 `127.0.0.1:8798`，若需从宿主机直接映射端口，请先在挂载目录创建 `config.json` 并将 `"host"` 设为 `"0.0.0.0"`。

## 配置说明

参考 `config.example.json`：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `host` | `"127.0.0.1"` | 监听地址。建议配合反向代理只监听回环地址。 |
| `port` | `8798` | 监听端口（1-65535）。 |
| `public_origin` | `"http://127.0.0.1:8798"` | 对外访问根地址（如 `https://pic.example.com`），用于生成图片外链与默认同源 CORS。 |
| `allowed_origins` | `[]` | 额外允许跨域调用 API 的 Origin 白名单（例如 `["https://editor.example.com"]`）。 |
| `trust_proxy_header` | `""` | 反向代理真实客户端 IP 头，仅在直连方为回环地址时生效。可选 `""`（不信任任何代理头）、`"CF-Connecting-IP"`、`"X-Real-IP"`、`"X-Forwarded-For"`。 |
| `state_dir` | `"./data"` | SQLite 索引目录（存储 `objects.sqlite3`）。 |
| `auth_token` | 首次随机生成 | Web 管理台登录密码。显式设为 `""` 可进入免密模式（仅建议用于受信任内网）。 |
| `api_key` | 首次随机生成 | 独立 API Key，不能与 `auth_token` 相同。 |
| `default_connection_id` | `""` | 当前默认上传使用的 Telegram 连接 ID（推荐在 Web 设置页管理）。 |
| `telegram_connections` | `[]` | Telegram Bot/频道历史连接列表，支持切换新频道后仍可回源读取旧图。 |

### 反向代理与 Cloudflare 部署

`deploy/` 目录提供了 `Caddyfile.example` 与 `pic-tg.service.example`：

- 若前面经过 Cloudflare -> 本地 Caddy/Nginx -> `127.0.0.1:8798`，请将 `trust_proxy_header` 设为 `"CF-Connecting-IP"`，以便登录暴力破解限速按真实访客 IP 生效。
- 若仅经过单层本地 Nginx/Caddy，设为 `"X-Real-IP"` 或 `"X-Forwarded-For"`。
- 切勿在未配置反向代理覆盖请求头的情况下开启 `trust_proxy_header`。

## API 与 PicGo 接入

认证请求头支持 `Authorization: Bearer <API_KEY>` 或 `X-API-Key: <API_KEY>`：

- `POST /api/upload`：`multipart/form-data`，字段 `file` 为图片文件（单文件上限 20MB，受 Telegram Bot API 限制）。
- `GET /api/objects?limit=60&offset=0`：服务端分页查询图片索引，支持 `q`（搜索）、`sort`（`new`/`old`/`big`/`small`）、`date`（`YYYY-MM-DD`）、`tz_offset`（时区偏移分钟数）与 `summary=1`（全库摘要）。
- `GET /api/download/{key}`：以附件形式下载原图。
- `DELETE /api/objects`：JSON 请求体 `{"key":"..."}` 或 `{"keys":["..."]}`，删除本地索引并尝试同步删除 Telegram 频道消息。

PicGo（配合 `web-uploader` 插件）配置示例可在登录后的 Web「API」页面一键复制，URL 会自动适配当前站点域名。

## 开发与测试

```bash
# Go 单元测试与静态检查
go test -count=1 ./...
go vet ./...
CGO_ENABLED=1 go test -race -count=1 ./...

# 前端逻辑回归测试（需 Node.js 22+）
for t in server/*.cjs; do node "$t"; done
```

仓库内还包含可选的无头浏览器回归脚本 `server/workspace_browser_test.mjs` 与 `server/redaction_browser_test.mjs`，可通过 Chrome DevTools Protocol 端点对真实页面交互和打码导出进行端到端验证。

## 许可协议

[MIT License](./LICENSE)
