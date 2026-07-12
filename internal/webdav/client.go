package webdav

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Entry struct {
	Href        string
	IsDir       bool
	DisplayName string
	ETag        string
	Size        int64
}

type Client struct {
	HTTP *http.Client
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	return hc.Do(req)
}

// GetRange 拉取 [start,end]（含）字节。返回 body、文件总长 total。
func (c *Client) GetRange(endpoint, path string, start, end int64) (io.ReadCloser, int64, error) {
	req, err := http.NewRequest("GET", endpoint+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := c.do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	total := parseTotal(resp)
	return resp.Body, total, nil
}

func parseTotal(resp *http.Response) int64 {
	cr := resp.Header.Get("Content-Range")
	// "bytes 0-3/10"
	if i := strings.LastIndex(cr, "/"); i >= 0 {
		if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
			return n
		}
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

func (c *Client) Head(endpoint, path string) (etag, lastMod string, size int64, err error) {
	req, err := http.NewRequest("HEAD", endpoint+path, nil)
	if err != nil {
		return "", "", 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	etag = resp.Header.Get("ETag")
	lastMod = resp.Header.Get("Last-Modified")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		size, _ = strconv.ParseInt(cl, 10, 64)
	}
	return etag, lastMod, size, nil
}

// ---- PROPFIND XML 解析 ----

type multistatus struct {
	XMLName   xml.Name   `xml:"multistatus"`
	Responses []response `xml:"response"`
}

type response struct {
	Href     string     `xml:"href"`
	Propstat []propstat `xml:"propstat"`
}

type propstat struct {
	Prop   prop   `xml:"prop"`
	Status string `xml:"status"`
}

type prop struct {
	DisplayName   string `xml:"displayname"`
	GetContentLen int64  `xml:"getcontentlength"`
	ETag          string `xml:"getetag"`
	ResourceType  struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
}

func (c *Client) PropFind(endpoint, path string, depth int) ([]Entry, error) {
	req, err := http.NewRequest("PROPFIND", endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", strconv.Itoa(depth))
	req.Header.Set("Content-Type", "application/xml")
	req.Body = io.NopCloser(strings.NewReader(`<?xml version="1.0"?>
<D:propfind xmlns:D="DAV:"><D:prop><D:displayname/><D:getcontentlength/><D:getetag/><D:resourcetype/></D:prop></D:propfind>`))
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 上游应回 207 Multi-Status；部分实现回 200 + 合法 multistatus 正文，
	// 透明降级：先按正文解析，解析成功即接受，避免对非 207 上游断流。
	var ms multistatus
	decErr := xml.NewDecoder(resp.Body).Decode(&ms)
	if resp.StatusCode != http.StatusMultiStatus && decErr != nil {
		return nil, fmt.Errorf("upstream PROPFIND status %d: %w", resp.StatusCode, decErr)
	}
	if decErr != nil {
		return nil, decErr
	}
	out := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		for _, ps := range r.Propstat {
			if ps.Status != "" && !strings.Contains(ps.Status, "200") {
				continue
			}
			e := Entry{
				Href:        r.Href,
				DisplayName: ps.Prop.DisplayName,
				ETag:        ps.Prop.ETag,
				Size:        ps.Prop.GetContentLen,
				IsDir:       ps.Prop.ResourceType.Collection != nil || strings.HasSuffix(r.Href, "/"),
			}
			if e.DisplayName == "" {
				e.DisplayName = lastPathSeg(r.Href)
			}
			out = append(out, e)
		}
	}
	return out, nil
}

func lastPathSeg(href string) string {
	h := strings.Trim(href, "/")
	if i := strings.LastIndex(h, "/"); i >= 0 {
		return h[i+1:]
	}
	return h
}
