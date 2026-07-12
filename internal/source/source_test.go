package source

import "testing"

func TestParseSubSource(t *testing.T) {
	cases := []struct {
		endpoint, path, wantTop string
	}{
		{"https://alist.example.com", "/阿里云盘/电影/x.mkv", "阿里云盘"},
		{"https://alist.example.com", "/夸克网盘/剧/S01E01.mkv", "夸克网盘"},
		{"https://h.example.com", "/百度网盘", "百度网盘"},
		{"https://h.example.com", "/", ""},
		{"https://h.example.com", "", ""},
	}
	for _, c := range cases {
		ss := ParseSubSource(c.endpoint, c.path)
		if ss.Endpoint != c.endpoint {
			t.Errorf("endpoint = %q, want %q", ss.Endpoint, c.endpoint)
		}
		if ss.TopSegment != c.wantTop {
			t.Errorf("TopSegment for %q = %q, want %q", c.path, ss.TopSegment, c.wantTop)
		}
	}
}

func TestSubSourceKey(t *testing.T) {
	ss := SubSource{Endpoint: "https://a.com", TopSegment: "阿里云盘"}
	if got := ss.Key(); got != "https://a.com|阿里云盘" {
		t.Errorf("Key() = %q, want https://a.com|阿里云盘", got)
	}
}

func TestRestPath(t *testing.T) {
	ss := ParseSubSource("https://a.com", "/阿里云盘/电影/x.mkv")
	if got := ss.RestPath("/阿里云盘/电影/x.mkv"); got != "/电影/x.mkv" {
		t.Errorf("RestPath = %q, want /电影/x.mkv", got)
	}
}
