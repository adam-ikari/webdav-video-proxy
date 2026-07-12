package marker

import (
	"strings"
	"testing"

	"github.com/gem/webdav-proxy/internal/assessor"
	"github.com/gem/webdav-proxy/internal/source"
)

func TestIsVideoFile(t *testing.T) {
	cases := map[string]bool{
		"x.mkv": true, "X.MP4": true, "a.ts": true, "a.txt": false, "": false,
	}
	for name, want := range cases {
		if got := IsVideoFile(name); got != want {
			t.Errorf("IsVideoFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMarkFolderNameSlow(t *testing.T) {
	ss := source.SubSource{Endpoint: "https://a.com", TopSegment: "百度网盘"}
	slow := assessor.Profile{SubSource: ss, BandwidthMbps: 1.5, IsSlow: true}
	m := New([]string{".mkv", ".mp4"})
	got := m.MarkFolderName(ss, "肖申克的救赎", slow)
	if !strings.Contains(got, "肖申克的救赎") {
		t.Errorf("lost original name: %q", got)
	}
	if !strings.Contains(got, "slow") {
		t.Errorf("missing slow marker: %q", got)
	}
	if !strings.Contains(got, "1.5") {
		t.Errorf("missing bandwidth: %q", got)
	}
	if strings.HasSuffix(got, ".mkv") {
		t.Errorf("should not touch extension: %q", got)
	}
	// 必须以原文件夹名开头（标记只追加）
	if !strings.HasPrefix(got, "肖申克的救赎") {
		t.Errorf("marker should prepend original name: %q", got)
	}
}

func TestMarkFolderNameFastNoChange(t *testing.T) {
	ss := source.SubSource{Endpoint: "https://a.com", TopSegment: "阿里云盘"}
	fast := assessor.Profile{SubSource: ss, BandwidthMbps: 38, IsSlow: false}
	m := New([]string{".mkv"})
	got := m.MarkFolderName(ss, "肖申克的救赎", fast)
	if got != "肖申克的救赎" {
		t.Errorf("fast source should not be renamed: %q", got)
	}
}

func TestStripMarker(t *testing.T) {
	ss := source.SubSource{Endpoint: "https://a.com", TopSegment: "百度网盘"}
	slow := assessor.Profile{SubSource: ss, BandwidthMbps: 1.5, IsSlow: true}
	m := New([]string{".mkv"})
	display := m.MarkFolderName(ss, "肖申克的救赎", slow)
	// 构造带标记的完整路径
	displayPath := "/百度网盘/" + display + "/movie.mkv"
	realPath, ok := m.StripMarker(displayPath)
	if !ok {
		t.Fatalf("StripMarker failed to map: %q", displayPath)
	}
	if realPath != "/百度网盘/肖申克的救赎/movie.mkv" {
		t.Errorf("realPath = %q, want /百度网盘/肖申克的救赎/movie.mkv", realPath)
	}
}

func TestStripMarkerNoMarker(t *testing.T) {
	m := New([]string{".mkv"})
	realPath, ok := m.StripMarker("/阿里云盘/电影/x.mkv")
	if !ok {
		t.Fatalf("should be ok for path without marker")
	}
	if realPath != "/阿里云盘/电影/x.mkv" {
		t.Errorf("realPath = %q, want unchanged", realPath)
	}
}
