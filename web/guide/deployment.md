# 部署与配置

## Docker 部署

### docker-compose（推荐）

```yaml
version: "3.9"
services:
  proxy:
    build: .
    # 或直接用镜像：image: ghcr.io/adam-ikari/webdav-video-proxy:latest
    ports:
      - "8080:8080"
    environment:
      UPSTREAM_ENDPOINT: "https://your-alist.example.com"
      CACHE_DIR: "/data/cache"
    volumes:
      - ./data:/data
    restart: unless-stopped
```

```bash
docker compose up -d
```

### 直接 docker run

```bash
docker run -d --name webdav-proxy \
  -p 8080:8080 \
  -e UPSTREAM_ENDPOINT=https://your-alist.example.com \
  -v "$PWD/data:/data" \
  --restart unless-stopped \
  webdav-video-proxy
```

## 环境变量

`UPSTREAM_ENDPOINT` 是唯一**必填**项，其余均有默认值。

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `UPSTREAM_ENDPOINT` | — | 上游 WebDAV 根 URL（**必填**） |
| `LISTEN_ADDR` | `:8080` | 监听地址 |
| `CACHE_DIR` | `/data/cache` | 缓存与 SQLite 索引目录（需持久化到卷） |
| `CACHE_MAX_SIZE` | `53687091200` (50GB) | 缓存硬上限（字节） |
| `CACHE_BLOCK_SIZE` | `4194304` (4MB) | 缓存块大小（同时是分片大小） |
| `CACHE_HIGH_WATERMARK` | `0.9` | 高水位（触发淘汰） |
| `CACHE_LOW_WATERMARK` | `0.7` | 低水位（停止淘汰） |
| `CACHE_TTL` | `604800` (7 天) | 单块最长存活秒数 |
| `DEFAULT_MAX_CONCURRENCY` | `4` | 友好子源的最大并发连接数 |
| `PROBE_MIN_INTERVAL_SEC` | `3600` | 同一子源主动探测最小间隔秒数 |
| `PROFILE_MAX_SAMPLES` | `20` | 画像滚动样本数 |
| `PROFILE_MAX_AGE_SEC` | `21600` (6 小时) | 画像最大时效秒数 |
| `SLOW_SOURCE_THRESHOLD_MBPS` | `2.0` | 低于此带宽标为慢源 |
| `HEAD_REVALIDATE_SEC` | `60` | 每文件 HEAD 一致性校验间隔秒数 |
| `VIDEO_EXTS` | `.mkv,.mp4,.ts,.avi,.mov,.flv,.m4v` | 影视文件夹判定的扩展名集合 |
| `META_CACHE_MAX_ENTRIES` | `4096` | 每文件版本/目录列表缓存的条目上限 |

## 调参建议

### 缓存大小

- **小 VPS**（磁盘 20-40GB）：`CACHE_MAX_SIZE` 设为可用磁盘的 50-70%，留余量给系统和 SQLite 索引。
- **本地 NAS**（磁盘充裕）：可设 100GB+，配合大 `CACHE_BLOCK_SIZE`（如 8MB）减少块数量。
- 缓存命中率低 → 调大 `CACHE_MAX_SIZE` 或 `CACHE_TTL`。

### 并发度

- `DEFAULT_MAX_CONCURRENCY` 控制对友好子源开几路。对单连接限速的网盘（如某些 Alist 后端）调高有效；对按账号限速的调高无用（决策器会自动检测降级，无需手动改）。
- 上游频繁 429 → 调低 `DEFAULT_MAX_CONCURRENCY` 到 2-3，或增大 `PROBE_MIN_INTERVAL_SEC`。

### 慢源标记

- `SLOW_SOURCE_THRESHOLD_MBPS` 决定多慢算「慢」。按你的网络 baseline 调：家宽 100Mbps 下游可设 5.0；移动网络可设 1.0。
- 标记字符目前固定为 `·slow·<带宽>MBps`。

## 数据持久化

`CACHE_DIR` 必须挂载到持久卷，否则容器重启缓存丢失。SQLite 索引（`index.db`）与缓存块都在此目录。重启后索引自动重建。

## 镜像构建

```bash
docker build -t webdav-video-proxy .
```

镜像为多阶段构建：`golang:1.22-alpine` 编译 → `alpine:3.20` 运行。纯 Go SQLite（`modernc.org/sqlite`），`CGO_ENABLED=0` 静态编译，无 CGO 依赖。

## 从源码运行

```bash
export UPSTREAM_ENDPOINT=https://your-alist.example.com
go run ./cmd/proxy
```

需 Go 1.22+。
