// Package console embeds the operator console (console/: React, built with
// Bun into console/dist) so luxd serves it at /console/. Without a build,
// luxd serves a page that says how to make one.
package console

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the console under /console/: files as they are (hashed
// assets cached for good), and index.html for every other path, where the
// app routes client-side.
func Handler() http.Handler {
	files, _ := fs.Sub(dist, "dist")
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		index = []byte(notBuilt)
	}
	fileServer := http.FileServer(http.FS(files))
	return http.StripPrefix("/console", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if st, err := fs.Stat(files, name); err == nil && !st.IsDir() && name != "index.html" {
			if strings.Contains(name, "-") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	}))
}

const notBuilt = `<!doctype html><meta charset="utf-8"><title>lux console</title>
<body style="font: 14px system-ui; margin: 3em">
<h1>lux console</h1><p>This luxd was built without the console. Build it with
<code>make build</code> (needs <a href="https://bun.sh">Bun</a>), or
<code>bun install &amp;&amp; cd console &amp;&amp; bun run build</code> before <code>go build</code>.</p>`
