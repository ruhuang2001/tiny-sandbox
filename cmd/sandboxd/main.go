// Command sandboxd is the reference server for tiny-sandbox.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerbackend"
)

var version = "dev"

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	dockerHost := flag.String("docker", defaultDockerSocket(), "Docker socket path")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if !isLoopback(*addr) && os.Getenv("SANDBOX_API_KEY") == "" {
		log.Fatalf("sandboxd: refusing to listen on non-loopback %s without SANDBOX_API_KEY", *addr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backend, err := dockerbackend.Dial(ctx, *dockerHost)
	if err != nil {
		log.Fatalf("sandboxd: connect to docker at %s: %v", *dockerHost, err)
	}

	// State is in memory, so containers from a previous process are orphans.
	// Removing them by the owner label is what keeps a restart from leaking
	// sandboxes; the label is the listen address, the only stable identity a
	// restart has.
	owner := *addr
	if removed, err := backend.RemoveOwned(ctx, daemon.OwnerLabelKey, owner); err != nil {
		log.Printf("sandboxd: startup cleanup removed %d containers, some failed: %v", removed, err)
	} else if removed > 0 {
		log.Printf("sandboxd: startup cleanup removed %d leftover containers", removed)
	}

	store := daemon.New(backend, daemon.WithOwnerLabel(owner))
	go store.Run(ctx)

	handler := daemon.NewHandler(store, version)
	if key := os.Getenv("SANDBOX_API_KEY"); key != "" {
		handler = handler.WithAPIKey(key)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("sandboxd: shutdown: %v", err)
		}
	}()

	log.Printf("sandboxd %s listening on %s", version, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("sandboxd: %v", err)
	}
}

// defaultDockerSocket honours DOCKER_HOST and otherwise uses the standard
// unix socket.
func defaultDockerSocket() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		const prefix = "unix://"
		if after, ok := strings.CutPrefix(host, prefix); ok {
			return after
		}
		log.Printf("sandboxd: ignoring non-unix DOCKER_HOST %q", host)
	}
	return "/var/run/docker.sock"
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
