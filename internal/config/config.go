package config

import (
	"os"
	"strconv"
)

type Config struct {
	UpstreamsJSON           string
	CacheDir                string
	CacheMaxSize            int64
	CacheBlockSize          int64
	CacheHighWatermark      float64
	CacheLowWatermark       float64
	CacheTTL                int64 // 秒
	SlowSourceThresholdMbps float64
	DefaultMaxConcurrency   int
	ProbeMinIntervalSec     int64
	ProfileMaxSamples       int
	ProfileMaxAgeSec        int64
	ListenAddr              string
	VideoExts               string // 逗号分隔
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getenvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func Load() Config {
	return Config{
		UpstreamsJSON:           getenv("UPSTREAMS", "[]"),
		CacheDir:                getenv("CACHE_DIR", "/data/cache"),
		CacheMaxSize:            getenvInt("CACHE_MAX_SIZE", 50*1024*1024*1024),
		CacheBlockSize:          getenvInt("CACHE_BLOCK_SIZE", 4*1024*1024),
		CacheHighWatermark:      getenvFloat("CACHE_HIGH_WATERMARK", 0.9),
		CacheLowWatermark:       getenvFloat("CACHE_LOW_WATERMARK", 0.7),
		CacheTTL:                getenvInt("CACHE_TTL", 7*24*3600),
		SlowSourceThresholdMbps: getenvFloat("SLOW_SOURCE_THRESHOLD_MBPS", 2.0),
		DefaultMaxConcurrency:   int(getenvInt("DEFAULT_MAX_CONCURRENCY", 4)),
		ProbeMinIntervalSec:     getenvInt("PROBE_MIN_INTERVAL_SEC", 3600),
		ProfileMaxSamples:       int(getenvInt("PROFILE_MAX_SAMPLES", 20)),
		ProfileMaxAgeSec:        getenvInt("PROFILE_MAX_AGE_SEC", 6*3600),
		ListenAddr:              getenv("LISTEN_ADDR", ":8080"),
		VideoExts:               getenv("VIDEO_EXTS", ".mkv,.mp4,.ts,.avi,.mov,.flv,.m4v"),
	}
}
