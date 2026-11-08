// Command sandboxd is the reference server for tiny-sandbox. This scaffold
// serves the health endpoint only; lifecycle and execution handlers arrive
// with the server core described in docs/design.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

var version = "dev"

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if !isLoopback(*addr) && os.Getenv("SANDBOX_API_KEY") == "" {
		log.Fatalf("sandboxd: refusing to listen on non-loopback %s without SANDBOX_API_KEY", *addr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": version})
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("sandboxd %s listening on %s", version, *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("sandboxd: %v", err)
	}
}

// isLoopback reports whether addr binds a loopback interface. An empty host,
// such as ":8080", binds every interface and is not loopback.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
