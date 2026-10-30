package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/agentlog"
	"github.com/Disesfgewu/GSD_Survey/agent-template-client/llmrouter"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

func loadEnv(path string) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return errors.New("invalid env line")
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, strings.Trim(strings.TrimSpace(value), "\"'"))
		}
	}
	return nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	config := flag.String("config", "models.example.json", "model configuration")
	env := flag.String("env", ".env", "provider environment file; empty to skip")
	tokenEnv := flag.String("token-env", "LLMROUTER_TOKEN", "environment variable holding the bearer token")
	timeout := flag.Duration("request-timeout", llmrouter.DefaultRequestTimeout, "retry and failover budget per request")
	logDir := flag.String("log-dir", "log", "audit log directory")
	flag.Parse()
	if err := loadEnv(*env); err != nil {
		fail(err)
	}
	token := os.Getenv(*tokenEnv)
	if err := llmrouter.RequireToken(*addr, token); err != nil {
		fail(err)
	}
	rec, err := agentlog.New("llmrouter", *logDir)
	if err != nil {
		fail(err)
	}
	defer rec.Close()
	ctx := agentlog.WithRecorder(context.Background(), rec)
	entries, skipped, err := llmrouter.LoadEntries(ctx, *config)
	if err != nil {
		fail(err)
	}
	server, err := llmrouter.New(entries, llmrouter.Options{Token: token, RequestTimeout: *timeout, Recorder: rec})
	if err != nil {
		fail(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Config.Name
	}
	fmt.Fprintf(os.Stderr, "llmrouter on %s models=%v skipped=%v audit=%s\n", *addr, names, skipped, rec.Path)
	httpServer := &http.Server{Addr: *addr, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	go func() {
		<-stop.Done()
		shutdown, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = httpServer.Shutdown(shutdown)
	}()
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		fail(err)
	}
}
