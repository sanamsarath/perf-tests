package server

import (
	"context"
	"flag" // added flag import
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	klog "k8s.io/klog/v2"
)

func setKlogLevelHandler(w http.ResponseWriter, r *http.Request) {
	levelStr := r.URL.Query().Get("level")
	if levelStr == "" {
		http.Error(w, "missing 'level' parameter", http.StatusBadRequest)
		return
	}
	level, err := strconv.Atoi(levelStr)
	if err != nil {
		http.Error(w, "invalid 'level' parameter", http.StatusBadRequest)
		return
	}
	// Update klog verbosity
	if f := flag.Lookup("v"); f != nil {
		if err := f.Value.Set(levelStr); err != nil {
			http.Error(w, "failed to update log level", http.StatusInternalServerError)
			return
		}
		klog.Infof("Updated klog level to %d", level)
		w.Write([]byte("klog level updated"))
	} else {
		http.Error(w, "flag 'v' not found", http.StatusInternalServerError)
	}
}

// Start initializes and runs both plain text (h2c) and TLS HTTP/2 servers.
// It now parses port flags: -plain and -tls.
func Start() error {
	// Initialize klog flags to register "v"
	klog.InitFlags(nil)
	defer klog.Flush()

	// Parse flags for ports.
	plainPort := flag.String("plain", "8080", "Port for plain text (h2c) server")
	plainPort2 := flag.String("plain2", "9090", "Port 2 for plain text (h2c) server")
	tlsPort := flag.String("tls", "8443", "Port for TLS server")
	flag.Parse()

	// Common handler responds with the greeting and HTTP protocol used.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if klog.V(2).Enabled() {
			klog.Infof("Received request from %s, protocol: %s, method: %s", r.RemoteAddr, r.Proto, r.Method)
		}
	})

	// HTTP/2 cleartext (plain text) server with h2c.
	plainServer := &http.Server{
		Addr:    ":" + *plainPort,
		Handler: h2c.NewHandler(handler, &http2.Server{}),
	}

	// HTTP/2 cleartext (plain text) server with h2c.
	plainServer2 := &http.Server{
		Addr:    ":" + *plainPort2,
		Handler: h2c.NewHandler(handler, &http2.Server{}),
	}

	// TLS HTTP/2 server; TLS config auto-enables HTTP/2.
	tlsServer := &http.Server{
		Addr:    ":" + *tlsPort,
		Handler: handler,
	}

	// Register handler to update klog level
	http.HandleFunc("/set-klog", setKlogLevelHandler)
	klog.Info("Starting server on :8090")
	go func() {
		if err := http.ListenAndServe(":8090", nil); err != nil && err != http.ErrServerClosed {
			klog.Errorf("Server error: %v", err)
		}
	}()

	// Run the plain text server concurrently.
	go func() {
		log.Println("Starting HTTP/2 (h2c) server on port", *plainPort)
		if err := plainServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Errorf("Plain server error: %v", err)
		}
	}()

	go func() {
		log.Println("Starting HTTP/2 (h2c) server on port", *plainPort2)
		if err := plainServer2.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Errorf("Plain server error: %v", err)
		}
	}()

	// Run the TLS server concurrently.
	go func() {
		log.Println("Starting HTTP/2 TLS server on port", *tlsPort)
		// Update certificate paths as needed.
		if err := tlsServer.ListenAndServeTLS("server.crt", "server.key"); err != nil && err != http.ErrServerClosed {
			klog.Errorf("TLS server error: %v", err)
		}
	}()

	// Wait for OS termination signal.
	waitForShutdown(plainServer, tlsServer, plainServer2)
	return nil
}

// waitForShutdown listens for SIGINT and SIGTERM signals to gracefully shutdown servers.
func waitForShutdown(servers ...*http.Server) {
	// Listen for incoming OS signals.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	// Begin graceful shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}

	// wait for all servers to shutdown
	<-ctx.Done()
	log.Println("Servers shut down gracefully")
}
