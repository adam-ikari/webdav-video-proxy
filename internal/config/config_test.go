package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{
		"CACHE_MAX_SIZE", "CACHE_BLOCK_SIZE", "CACHE_HIGH_WATERMARK", "CACHE_LOW_WATERMARK",
		"CACHE_TTL", "SLOW_SOURCE_THRESHOLD_MBPS", "DEFAULT_MAX_CONCURRENCY",
		"PROBE_MIN_INTERVAL_SEC", "PROFILE_MAX_SAMPLES", "PROFILE_MAX_AGE_SEC",
	} {
		os.Unsetenv(k)
	}
	c := Load()
	if c.CacheMaxSize != 50*1024*1024*1024 {
		t.Errorf("CacheMaxSize = %d, want 50GB", c.CacheMaxSize)
	}
	if c.CacheBlockSize != 4*1024*1024 {
		t.Errorf("CacheBlockSize = %d, want 4MB", c.CacheBlockSize)
	}
	if c.CacheHighWatermark != 0.9 || c.CacheLowWatermark != 0.7 {
		t.Errorf("watermarks = %v/%v, want 0.9/0.7", c.CacheHighWatermark, c.CacheLowWatermark)
	}
	if c.CacheTTL != 7*24*3600 {
		t.Errorf("CacheTTL = %d, want 7d", c.CacheTTL)
	}
	if c.SlowSourceThresholdMbps != 2.0 {
		t.Errorf("slow threshold = %v, want 2.0", c.SlowSourceThresholdMbps)
	}
	if c.DefaultMaxConcurrency != 4 {
		t.Errorf("max concurrency = %d, want 4", c.DefaultMaxConcurrency)
	}
	if c.ProbeMinIntervalSec != 3600 {
		t.Errorf("probe interval = %d, want 3600", c.ProbeMinIntervalSec)
	}
	if c.ProfileMaxSamples != 20 || c.ProfileMaxAgeSec != 6*3600 {
		t.Errorf("profile sample/age = %d/%d, want 20/21600", c.ProfileMaxSamples, c.ProfileMaxAgeSec)
	}
}

func TestLoadFromEnv(t *testing.T) {
	os.Setenv("CACHE_MAX_SIZE", "1073741824")
	os.Setenv("CACHE_BLOCK_SIZE", "1048576")
	os.Setenv("CACHE_HIGH_WATERMARK", "0.8")
	os.Setenv("CACHE_LOW_WATERMARK", "0.5")
	defer func() {
		os.Unsetenv("CACHE_MAX_SIZE")
		os.Unsetenv("CACHE_BLOCK_SIZE")
		os.Unsetenv("CACHE_HIGH_WATERMARK")
		os.Unsetenv("CACHE_LOW_WATERMARK")
	}()
	c := Load()
	if c.CacheMaxSize != 1073741824 {
		t.Errorf("CacheMaxSize = %d, want 1GB", c.CacheMaxSize)
	}
	if c.CacheBlockSize != 1048576 {
		t.Errorf("CacheBlockSize = %d, want 1MB", c.CacheBlockSize)
	}
	if c.CacheHighWatermark != 0.8 || c.CacheLowWatermark != 0.5 {
		t.Errorf("watermarks = %v/%v, want 0.8/0.5", c.CacheHighWatermark, c.CacheLowWatermark)
	}
}
