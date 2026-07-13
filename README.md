# webdav-video-proxy

Docker-deployed WebDAV video proxy for local players (IINA/PotPlayer/nPlayer/网易爆米花 etc.). Solves stutter / slow start / seek lag against multi-source WebDAV upstreams (e.g. Alist aggregating 阿里云盘/夸克/百度).

## What it does

- **Rate assessment (§1)** — per-subsource profiling: passive throughput samples + active multi-connection friendliness probe. Subsource = `(endpoint, top-path-segment)`.
- **Multi-connection segment fetch (§3)** — single-source, single-file, N-way parallel Range fetch with out-of-order→ordered emission. First-block probe detects Range support; falls back to single-connection whole-file stream if upstream ignores Range.
- **Block cache (§2)** — block-level disk cache (SQLite), LRU eviction with high/low watermarks, refcount-protected blocks (no eviction of in-use blocks), TTL sweep.
- **Preloading (§4)** — sequential readahead during playback + next-episode prefetch at 70% (natural-sort `S01E02` → `S01E03`), throttled to never starve the main path.
- **Slow-source marking (§5)** — slow subsources' video folders get a virtual name suffix (`电影·slow·1.5MBps`). No upstream mutation, no custom headers.

## Deploy

```bash
docker compose up -d
# point your player at http://<host>:8080/
```

Required env: `UPSTREAM_ENDPOINT`. All others have defaults.

| Env | Default | Meaning |
|-----|---------|---------|
| `UPSTREAM_ENDPOINT` | — | Upstream WebDAV root URL (required) |
| `LISTEN_ADDR` | `:8080` | Listen address |
| `CACHE_DIR` | `/data/cache` | Cache + SQLite index dir |
| `CACHE_MAX_SIZE` | 50GB | Cache hard limit |
| `CACHE_BLOCK_SIZE` | 4MB | Block size (also segment size) |
| `CACHE_TTL` | 7d | Per-block max age |
| `DEFAULT_MAX_CONCURRENCY` | 4 | Max parallel connections for friendly subsources |
| `HEAD_REVALIDATE_SEC` | 60 | Per-file HEAD revalidation interval |
| `SLOW_SOURCE_THRESHOLD_MBPS` | 2.0 | Below this → marked slow |
| `VIDEO_EXTS` | `.mkv,.mp4,.ts,...` | Video extensions for folder detection |
| `META_CACHE_MAX_ENTRIES` | 4096 | Cap for per-file version/dir-listing caches |

## Transparent degradation (never breaks the main path)

- Cache miss / disk full → single-connection fetch, no caching.
- Upstream ignores Range (200 whole file) → single-connection whole-file stream.
- HEAD fails → reuse stale version, or `unknown` version, continue.
- 429/503 → retry with backoff, mark subsource throttled (forces N=1).
- Eviction can't keep up → skip caching that block.
- All side-path failures degrade; the player never sees a broken stream from a side-path fault.

## Status

v1.1 — production-usable. The v1 review's Important findings (I1–I6) and Minors (M1–M7) are resolved; an additional real bug (cache-check overrun past the requested range) was found and fixed via integration tests. All tests pass under `-race`; binary is static (pure-Go SQLite, no CGO).

See `docs/superpowers/specs/` for the design and `.superpowers/sdd/progress.md` for the per-task review trail.
