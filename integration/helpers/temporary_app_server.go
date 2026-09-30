package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, "<!doctype html><title>Temporary server</title><p>APP_LABELLED_WORKER</p>")
	})
	mux.HandleFunc("/api", api)
	mux.HandleFunc("/mesh", api)
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/api", http.StatusFound) })
	mux.HandleFunc("/socket", echo)
	server := &http.Server{Addr: "127.0.0.1:" + os.Getenv("PORT"), Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func api(w http.ResponseWriter, r *http.Request) {
	cwd, _ := os.Getwd()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"pid": os.Getpid(), "cwd": cwd, "path": r.URL.Path})
}

func echo(w http.ResponseWriter, r *http.Request) {
	connection, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = connection.CloseNow() }()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	kind, payload, err := connection.Read(ctx)
	if err == nil {
		_ = connection.Write(ctx, kind, append(payload, []byte("|xfp="+r.Header.Get("X-Forwarded-Proto"))...))
	}
}
