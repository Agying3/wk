// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 gao-gao-zai and contributors.

// Package workbuddy2api embeds the distribution's legal notices.
package workbuddy2api

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

//go:embed LICENSE NOTICE LICENSES/*
var notices embed.FS

const legalPage = `<!doctype html><html lang="zh-CN"><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>WorkBuddy2API · 许可证与对应源码</title><body>
<h1>WorkBuddy2API · 许可证与对应源码</h1>
<p>本修改版采用 <a href="/legal/license">AGPL-3.0-only</a>，不提供任何担保。
Copyright © 2026 gao-gao-zai 及贡献者。第三方内容的版权和许可继续保留。</p>
<p>基于 / 包含了 <a href="https://github.com/Sliverkiss/workbuddy2api">Sliverkiss/workbuddy2api</a>、
<a href="https://github.com/linguo2625469/workbuddy2api-panel">linguo2625469/workbuddy2api-panel</a> 及
<a href="https://github.com/yreidev/workbuddy2api-panel">workbuddy2api-panel</a> 的代码或材料。
本项目是独立维护的修改版，不是上述项目或腾讯的官方版本。</p>
<ul><li><a href="/legal/license">AGPL 许可证全文</a></li>
<li><a href="/legal/notice">版权与来源声明</a></li>
<li><a href="/legal/third-party">第三方许可证与声明</a></li>
<li><a href="/legal/source">下载本发行包的对应源码</a></li></ul>
{{if not .}}<p><strong>当前安装缺少对应源码归档。</strong>部署者须将本次构建生成的 source.tar.gz
与程序一同安装，再向用户提供服务。仓库首页不能代替运行版本的对应源码。</p>{{end}}
<p>源码不包含令牌、账号、业务数据库或日志。项目仓库：
<a href="https://github.com/gao-gao-zai/wk">gao-gao-zai/wk</a>。</p></body></html>`

var pageTemplate = template.Must(template.New("legal").Parse(legalPage))

// LegalHandler serves only embedded notices and one fixed release archive.
// The archive is prepared at build time; no runtime directory is archived.
func LegalHandler(sourceArchive string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		switch r.URL.Path {
		case "/legal", "/legal/":
			info, err := os.Stat(sourceArchive)
			available := err == nil && info.Mode().IsRegular()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			if r.Method != http.MethodHead {
				_ = pageTemplate.Execute(w, available)
			}
		case "/legal/license":
			serveNotice(w, r, "LICENSE")
		case "/legal/notice":
			serveNotice(w, r, "NOTICE")
		case "/legal/third-party":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			entries, _ := fs.ReadDir(notices, "LICENSES")
			if r.Method == http.MethodHead {
				return
			}
			for _, entry := range entries {
				data, _ := notices.ReadFile("LICENSES/" + entry.Name())
				_, _ = w.Write([]byte("\n===== " + entry.Name() + " =====\n"))
				_, _ = w.Write(data)
			}
		case "/legal/source":
			f, err := os.Open(sourceArchive)
			if err != nil {
				http.Error(w, "Corresponding source archive missing; contact the service operator.", http.StatusServiceUnavailable)
				return
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil || !info.Mode().IsRegular() {
				http.Error(w, "Corresponding source archive unavailable.", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("Content-Disposition", `attachment; filename="workbuddy2api-source.tar.gz"`)
			http.ServeContent(w, r, "source.tar.gz", info.ModTime(), f)
		default:
			http.NotFound(w, r)
		}
	})
}

func serveNotice(w http.ResponseWriter, r *http.Request, name string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.Method != http.MethodHead {
		data, _ := notices.ReadFile(name)
		_, _ = w.Write(data)
	}
}

// ReleaseLegalHandler locates the immutable source companion beside the binary.
func ReleaseLegalHandler() http.Handler {
	path, err := os.Executable()
	if err != nil {
		return LegalHandler("")
	}
	return LegalHandler(filepath.Join(filepath.Dir(path), "source.tar.gz"))
}
