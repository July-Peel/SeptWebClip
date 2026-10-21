# SeptWebClip

按口令划分的共享便签墙。电脑上 Ctrl+V、手机上长按粘贴，文字、图片和文件就成了墙上的一张便签，用同一口令的其他设备会实时看到。

- 后端：Go（标准库 `net/http` + SQLite，纯 Go 实现、无 CGO），编译成一个约 15 MB 的静态文件，空闲内存占用约 15 MB
- 前端：原生 HTML / CSS / JS，打包进同一个可执行文件
- 实时同步：SSE（服务器推送事件）
- 图片存储：本地目录或 WebDAV

## 功能

- **口令划分墙**：口令相同的设备看到同一面墙，墙上的人都能查看、复制、钉住、删除所有便签
- **粘贴共享**：PC 端在页面任意位置 Ctrl+V 粘贴文字、截图或文件，也可以把文件拖进来；iPhone 在输入框长按粘贴，或点「＋ 图片」「＋ 文件」选择。一次可以贴多个
- **文件分享**：支持任意类型的文件，单个不超过 50 MB（可配置）。PNG、JPG、GIF、WebP 等常见图片显示为图片便签；其他格式，以及超过图片上限的大图，都按文件分享，只能下载，不会在页面里直接打开
- **有效期**：未钉住的便签到期后自动删除，时长可以配置（默认 3 天）
- **钉住**：钉住的便签不会被自动删除，只能手动撕掉
- **统计**：查看次数、复制次数（复制按钮和手动选中复制都算）、下载次数、贴上的时间和 IP
- **访问日志**：点便签右下角「日志 ↗」翻到背面，查看每一次贴上、查看、复制、下载、钉住的 IP、设备和时间
- **删除只删记录**：手动删除和过期清理都只删数据库记录，图片文件保留在存储里

## 部署（Debian 13 + Docker + 宿主机 nginx）

```bash
# 1. 把项目放到服务器上，例如 /opt/SeptWebClip
cd /opt/SeptWebClip

# 2. 按需修改配置（可选）
cp .env.example .env

# 3. 在服务器上编译并启动
docker compose up -d --build

# 4. 查看状态和日志
docker compose ps
docker compose logs -f
```

然后参考 [deploy/nginx.conf.example](deploy/nginx.conf.example) 给宿主机的 nginx 加一个站点，反代到 `127.0.0.1:8080`。**一定要配置 HTTPS**：浏览器的「复制」按钮（剪贴板写入）只在 HTTPS 下可用。

更新版本：拉取新代码后再执行一次 `docker compose up -d --build`。

## 配置

都通过环境变量（`.env`）设置：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `CLIP_TTL` | `3d` | 未钉住的便签多久后自动删除，支持 `3d`、`72h`、`90m` |
| `CLEANUP_INTERVAL` | `10m` | 检查过期便签的间隔 |
| `VIEW_DEDUP` | `30m` | 同一设备（IP + 浏览器）重复查看同一便签，在这段时间内只记一次 |
| `MAX_IMAGE_MB` | `20` | 图片便签的单张上限，超过的图片按文件分享 |
| `MAX_FILE_MB` | `50` | 任意文件的单个上限，nginx 的 `client_max_body_size` 要比它大几 MB |
| `MAX_TEXT_KB` | `1024` | 单张文字便签上限 |
| `MIN_CODE_LEN` | `4` | 口令最少字符数 |
| `IMAGE_STORAGE` | `local` | 图片存储方式：`local` 或 `webdav` |
| `IMAGES_HOST_DIR` | `./data/images` | local 模式下图片在宿主机上的目录 |
| `WEBDAV_URL` / `WEBDAV_USER` / `WEBDAV_PASSWORD` | 空 | webdav 模式的目录地址和账号，目录不存在会自动创建 |
| `DATA_HOST_DIR` | `./data` | 数据库 `septwebclip.db` 和密钥 `secret.key` 所在目录 |
| `TRUST_PROXY` | `true` | 信任 nginx 传来的 `X-Real-IP`。端口直接暴露在公网时必须改成 `false`，否则 IP 可以被伪造 |
| `SECRET` | 自动生成 | 口令换算成墙 ID 用的密钥。**更换后所有旧墙都无法再进入** |
| `FONT_CSS_URL` | 霞鹜文楷（jsDelivr） | 手写字体样式表，留空则使用系统楷体 |
| `BIND_ADDR` / `PORT` | `127.0.0.1` / `8080` | 容器映射到宿主机的地址和端口 |

## 数据和隐私

- 服务器不保存口令原文，只保存它经 HMAC 换算后的墙 ID
- 每个 IP 每 10 分钟最多尝试 20 次口令，防止暴力猜测；仍然建议用长一点的口令
- 访问日志会记录查看者的 IP 和浏览器 UA，同一面墙的人都能看到
- 过期和被删除的便签，数据库记录和日志会一起删掉，但**图片和文件本身不会删除**。本地模式下按 `年/月/日/` 分目录存放，需要时可以自己定期清理
- 分享的文件一律以附件形式下载（`application/octet-stream`），即使是 HTML 文件也不会在本站点里被打开执行

## 已知限制

- iPhone 长按图片「拷贝」或「存储到照片」、电脑上右键「复制图片」，浏览器不会通知网页，所以这几种操作不计数
- iOS 不同版本对「在输入框里长按粘贴图片」的支持不一致，粘不进来时请用「＋ 图片」按钮
- SQLite 只能单实例运行，不要同时启动多个容器指向同一个数据目录

## 本地开发

需要 Go 1.26+：

```bash
go run .
# 打开 http://localhost:8080 ，数据写在 ./data
```

## 目录结构

```
main.go                     入口：读取配置、启动 HTTP 服务和过期清理
internal/config             环境变量配置
internal/store              SQLite：便签、墙、访问日志
internal/storage            图片存储：本地目录 / WebDAV
internal/server             HTTP API、SSE 推送、静态页面
web/                        前端页面（编译时嵌入可执行文件）
deploy/nginx.conf.example   宿主机 nginx 配置示例
```

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/session` | 是否已进入墙，以及有效期、大小上限等配置 |
| `POST` | `/api/session` | `{code}` 进入一面墙（写入 Cookie） |
| `DELETE` | `/api/session` | 离开当前墙 |
| `GET` | `/api/clips` | 当前墙的所有便签 |
| `POST` | `/api/clips` | `{content, pinned}` 贴一张文字便签 |
| `POST` | `/api/clips/image` | multipart：`file`、可选 `thumb`、`pinned` |
| `POST` | `/api/clips/file` | multipart：`file`、`pinned`，任意文件 |
| `GET` | `/api/clips/{id}/image` | 原图；`?thumb` 取缩略图，`?dl` 作为附件下载并计数 |
| `GET` | `/api/clips/{id}/file` | 下载文件并计数 |
| `POST` | `/api/clips/{id}/copy` | 记一次复制 |
| `POST` | `/api/views` | `{ids}` 批量记查看（按 `VIEW_DEDUP` 去重） |
| `POST` | `/api/clips/{id}/pin` | `{pinned}` 钉住 / 取消钉住 |
| `DELETE` | `/api/clips/{id}` | 删除便签（只删记录） |
| `GET` | `/api/clips/{id}/logs` | 访问日志 |
| `GET` | `/api/events` | SSE：`create` / `update` / `delete` / `presence` |
