package marker

import (
	"fmt"
	"path"
	"strings"

	"github.com/gem/webdav-proxy/internal/assessor"
	"github.com/gem/webdav-proxy/internal/source"
)

type Marker struct {
	videoExts map[string]bool
}

func New(exts []string) *Marker {
	m := &Marker{videoExts: map[string]bool{}}
	for _, e := range exts {
		m.videoExts[strings.ToLower(e)] = true
	}
	return m
}

func IsVideoFile(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return markerSingleton.videoExts[ext]
}

// 包级单例供 IsVideoFile 用（由 server 启动时初始化）。
var markerSingleton = New(defaultExts())

var defaultExts = func() []string {
	return []string{".mkv", ".mp4", ".ts", ".avi", ".mov", ".flv", ".m4v"}
}

// IsVideoFolder 判断该目录的条目列表里是否直接含视频文件。
func (m *Marker) IsVideoFolder(entries []string) bool {
	for _, n := range entries {
		if m.videoExts[strings.ToLower(path.Ext(n))] {
			return true
		}
	}
	return false
}

// MarkFolderName 对慢源下的影视文件夹追加标记。快源不改名。
// 标记用中点 · 分隔，避开方括号/圆括号等通配字符。
func (m *Marker) MarkFolderName(ss source.SubSource, folderName string, prof assessor.Profile) string {
	if !prof.IsSlow {
		return folderName
	}
	return fmt.Sprintf("%s·slow·%vMBps", folderName, prof.BandwidthMbps)
}

// StripMarker 从显示路径中剥掉标记，还原真实路径。
// 返回 (realPath, true)；若路径无标记也返回原路径与 true。
func (m *Marker) StripMarker(displayPath string) (string, bool) {
	// 标记形如 ·slow·xMBps，插在某个路径段末尾。逐段处理。
	parts := strings.Split(displayPath, "/")
	for i, seg := range parts {
		if idx := strings.Index(seg, "·slow·"); idx > 0 {
			parts[i] = seg[:idx]
			return strings.Join(parts, "/"), true
		}
	}
	return displayPath, true
}
