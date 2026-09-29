package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// serve is `lux-fake serve <port> [text]`: a small web server for tests of
// a Run's servers and their previews.
//
//	/          text/html: <text> (default "hello from lux-fake"), and the host it was asked for
//	/headers   the request's headers, as JSON
//	/cookie    sets cookies, one with Domain=, and says so
//	/events    server-sent events: three, a second apart
//	/exit      exits the process with code 3, after a line on stderr
func serve(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lux-fake serve <port> [text]")
		os.Exit(2)
	}
	text := "hello from lux-fake"
	if len(args) > 1 {
		text = strings.Join(args[1:], " ")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("GET %s\n", r.URL.Path)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<p>%s</p><p>host=%s</p>\n", text, r.Host)
	})
	mux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.Header)
	})
	mux.HandleFunc("/cookie", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "app=1; Domain=example.com; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "plain=2; Path=/")
		fmt.Fprintln(w, "cookies set")
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for i := range 3 {
			fmt.Fprintf(w, "data: tick %d\n\n", i)
			if f != nil {
				f.Flush()
			}
			time.Sleep(time.Second)
		}
	})
	mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(os.Stderr, "fatal: asked to exit")
		os.Exit(3)
	})
	fmt.Printf("serving on :%s\n", args[0])
	if err := http.ListenAndServe(":"+args[0], mux); err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
}
