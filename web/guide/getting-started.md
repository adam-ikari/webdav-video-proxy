# 快速开始

## 前置条件

- 一个 WebDAV 上游地址（如 [Alist](https://github.com/alist-org/alist) 暴露的 WebDAV 接口）
- 装有 Docker 的主机

## 三步启动

### 1. 准备 docker-compose.yml

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

### 2. 启动

```bash
docker compose up -d
```

唯一**必填**的环境变量是 `UPSTREAM_ENDPOINT`（你的上游 WebDAV 根 URL）。其余配置均有合理默认值。

### 3. 配置播放器

把播放器（IINA / PotPlayer / nPlayer / 网易爆米花 等）的 WebDAV 地址指向：

```
http://<你的主机>:8080/
```

播放器走 HTTP Range 取流，代理透明加速。无需任何客户端改造。

## 验证

启动后用 curl 抓一个文件确认代理工作：

```bash
curl -v -H "Range: bytes=0-1023" http://localhost:8080/阿里云盘/电影/x.mkv -o /dev/null
```

应返回 `206 Partial Content` + 正确字节。

## 下一步

- [架构总览](/guide/architecture) — 了解请求如何流经代理
- [核心功能](/guide/features) — 各功能详解
- [部署与配置](/guide/deployment) — 完整环境变量与调参
