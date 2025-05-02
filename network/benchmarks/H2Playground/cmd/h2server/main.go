package main

import (
	"log"

	"k8s.io/perf-tests/network/benchmarks/H2Playground/pkg/server"
)

func main() {
	if err := server.Start(); err != nil {
		log.Fatalf("Server exited with error: %v", err)
	}
}
