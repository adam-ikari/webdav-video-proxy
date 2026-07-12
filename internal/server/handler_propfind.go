package server

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/gem/webdav-proxy/internal/source"
)

func (s *Server) handlePropFind(w http.ResponseWriter, r *http.Request) {
	displayPath := r.URL.Path
	realPath, _ := s.mark.StripMarker(displayPath)
	ss := source.ParseSubSource(s.endpoint, realPath)
	rest := ss.RestPath(realPath)

	depth := 1
	if d := r.Header.Get("Depth"); d == "0" {
		depth = 0
	}
	entries, err := s.cli.PropFind(s.endpoint, rest, depth)
	if err != nil {
		http.Error(w, "upstream PROPFIND failed", http.StatusBadGateway)
		return
	}

	// 收集每个子文件夹的子条目名，判定影视文件夹
	children := map[string][]string{} // dirHref -> []childName
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		parent := parentHref(e.Href)
		children[parent] = append(children[parent], e.DisplayName)
	}

	prof := s.asm.GetProfile(ss)

	// propstat 的内嵌类型需具名，否则字面量无法构造。
	type propT struct {
		DisplayName string `xml:"displayname"`
		IsDir       bool   `xml:"resourcetype>collection"`
	}
	type propstatT struct {
		Prop   propT  `xml:"prop"`
		Status string `xml:"status"`
	}
	type respT struct {
		XMLName  xml.Name    `xml:"response"`
		Href     string      `xml:"href"`
		Propstat []propstatT `xml:"propstat"`
	}
	var ms struct {
		XMLName   xml.Name `xml:"multistatus"`
		Responses []respT  `xml:"response"`
	}
	for _, e := range entries {
		displayName := e.DisplayName
		if e.IsDir && s.mark.IsVideoFolder(children[e.Href]) && prof.IsSlow {
			displayName = s.mark.MarkFolderName(ss, displayName, prof)
		}
		ms.Responses = append(ms.Responses, respT{
			Href: e.Href,
			Propstat: []propstatT{{
				Prop:   propT{DisplayName: displayName, IsDir: e.IsDir},
				Status: "HTTP/1.1 200 OK",
			}},
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(&ms)
}

func parentHref(href string) string {
	h := strings.TrimRight(href, "/")
	if i := strings.LastIndex(h, "/"); i >= 0 {
		return h[:i+1]
	}
	return h
}
