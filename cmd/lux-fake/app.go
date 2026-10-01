package main

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// app is `lux-fake app <port> <checkout> <counter file>`: a preview's
// hello world. Each page shows the checkout's commit and its message.txt,
// and a visit counter kept in a file (on a state volume, it survives
// stops, moves and wakes). Assets and API calls (/api/…, anything with a
// dot) are served without counting.
func app(args []string) {
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: lux-fake app <port> <checkout> <counter file>")
		os.Exit(2)
	}
	port, dir, counter := args[0], args[1], args[2]
	var mu sync.Mutex
	visit := func() int {
		mu.Lock()
		defer mu.Unlock()
		b, _ := os.ReadFile(counter)
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		n++
		_ = os.MkdirAll(filepath.Dir(counter), 0o755)
		_ = os.WriteFile(counter, []byte(strconv.Itoa(n)+"\n"), 0o644)
		return n
	}
	commit := func() string {
		out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
		if err != nil {
			return "unknown"
		}
		return strings.TrimSpace(string(out))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/commit", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, commit())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ".") {
			w.Header().Set("Content-Type", "text/css")
			fmt.Fprintln(w, "body { font-family: system-ui, sans-serif; margin: 3em; }")
			return
		}
		msg, _ := os.ReadFile(filepath.Join(dir, "message.txt"))
		n := visit()
		fmt.Printf("GET %s visit %d at %s\n", r.URL.Path, n, commit())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><title>preview</title><link rel="stylesheet" href="/style.css">
<h1>%s</h1>
<p>commit <code id="commit">%s</code></p>
<p>visits <b id="visits">%d</b> (kept on the state volume)</p>
<p>path <code>%s</code></p>
`, html.EscapeString(strings.TrimSpace(string(msg))), commit(), n, html.EscapeString(r.URL.Path))
	})
	fmt.Printf("app on :%s, commit %s\n", port, commit())
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
}
