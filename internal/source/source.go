package source

import "strings"

type SubSource struct {
	Endpoint   string
	TopSegment string // 顶级路径段，不带斜杠；根目录为空串
}

// ParseSubSource 从完整路径取第一段作为 TopSegment。
func ParseSubSource(endpoint, path string) SubSource {
	p := strings.Trim(path, "/")
	if p == "" {
		return SubSource{Endpoint: endpoint, TopSegment: ""}
	}
	seg := p
	if i := strings.Index(p, "/"); i >= 0 {
		seg = p[:i]
	}
	return SubSource{Endpoint: endpoint, TopSegment: seg}
}

// Key 作 SQLite 画像 key。
func (s SubSource) Key() string {
	return s.Endpoint + "|" + s.TopSegment
}

// RestPath 返回去掉顶级段后的剩余路径（保留前导斜杠）。
func (s SubSource) RestPath(fullPath string) string {
	p := strings.TrimLeft(fullPath, "/")
	if s.TopSegment == "" {
		return "/" + p
	}
	rest := strings.TrimPrefix(p, s.TopSegment)
	rest = strings.TrimLeft(rest, "/")
	return "/" + rest
}
