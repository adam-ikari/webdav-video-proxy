---
layout: home

hero:
  name: WebDAV 视频网盘代理
  text: 让多源网盘视频流畅播放
  tagline: Docker 一键部署 · 多连接分片加速 · 块缓存 · 慢源可视化标记
  actions:
    - theme: brand
      text: 快速开始
      link: /guide/getting-started
    - theme: alt
      text: GitHub
      link: https://github.com/adam-ikari/webdav-video-proxy

features:
  - title: 资源速率评估
    details: 对每个子源（如阿里云盘、夸克、百度）持续测速，被动采样 + 主动探测多连接友好度，产出带宽档位与建议并发度。
  - title: 多连接分片提速
    details: 对支持多连接的源，单源单文件多路 Range 并行拉取，乱序到达、顺序吐出，显著提升起播速度。不支持 Range 的源自动降级整文件流。
  - title: 块级磁盘缓存
    details: 按块缓存到本地（SQLite 索引），LRU 淘汰 + 高低水位 + 引用计数保护 + TTL 兜底。重复请求直接命中缓存，不再打上游。
  - title: 预加载加速
    details: 播放时顺序预读后续块；播放到 70% 时按自然排序预取下一集首段，切集零等待。
  - title: 慢源可视化标记
    details: 慢源下的影视文件夹自动追加速度标记（如 电影·slow·1.5MBps），在播放器目录里一眼看出哪个源慢。不碰上游真实文件名。
  - title: 永不断流
    details: 所有旁路失败（缓存满、HEAD 失败、上游限流 429）都透明降级，主路径绝不因侧链故障而断流。
---
