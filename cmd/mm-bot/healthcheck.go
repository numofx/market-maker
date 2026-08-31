package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// The image is distroless and has no shell, so the container health check is the
// binary probing its own metrics server: ["CMD", "/app/mm-bot", "-healthcheck"].
//
// This runs before config.Load on purpose. A probe that parses the full
// environment would fail over a missing MM_SIGNER_PRIVATE_KEY rather than over the
// bot being unwell, which is the opposite of what a health check is for.

func isHealthcheckArg(args []string) bool {
	if len(args) != 2 {
		return false
	}
	return args[1] == "-healthcheck" || args[1] == "--healthcheck"
}

func runHealthcheck() int {
	addr := os.Getenv("MM_METRICS_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: bad MM_METRICS_ADDR %q: %v\n", addr, err)
		return 1
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: /healthz returned %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
