package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	BaseDir                  string
	ListenHost               string
	ListenPort               int
	PublicHost               string
	ManagementHTMLPath       string
	ManagementHTMLCandidates []string
	RecordsPath              string
	PatchQueryKey            string
	PatchRefreshEvery        time.Duration
	QueuePollEvery           time.Duration
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	app, err := NewApp(cfg)
	if err != nil {
		log.Fatalf("create app: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("run app: %v", err)
	}
}

func loadConfig() (Config, error) {
	exePath, err := os.Executable()
	if err != nil {
		return Config{}, err
	}
	baseDir := filepath.Dir(exePath)

	port := flag.Int("port", 8328, "local patch listen port")
	host := flag.String("host", "127.0.0.1", "local patch listen host")
	dir := flag.String("dir", baseDir, "directory containing cli-proxy-api.exe or management.html")
	flag.Parse()

	resolvedPort, err := findAvailablePort(*host, *port, 16)
	if err != nil {
		return Config{}, err
	}

	candidates := buildManagementHTMLCandidates(baseDir, *dir)
	managementPath := firstExistingPath(candidates)
	if managementPath == "" && len(candidates) > 0 {
		managementPath = candidates[0]
	}

	return Config{
		BaseDir:                  baseDir,
		ListenHost:               *host,
		ListenPort:               resolvedPort,
		PublicHost:               "127.0.0.1",
		ManagementHTMLPath:       managementPath,
		ManagementHTMLCandidates: candidates,
		RecordsPath:              filepath.Join(baseDir, "cpa-usage-patch-records.jsonl"),
		PatchQueryKey:            "cpa_usage_patch",
		PatchRefreshEvery:        15 * time.Second,
		QueuePollEvery:           900 * time.Millisecond,
	}, nil
}

func buildManagementHTMLCandidates(runtimeDir string, roots ...string) []string {
	candidates := make([]string, 0, 8)

	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		cleaned := filepath.Clean(path)
		for _, existing := range candidates {
			if strings.EqualFold(existing, cleaned) {
				return
			}
		}
		candidates = append(candidates, cleaned)
	}

	addEnvCandidate := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		cleaned := filepath.Clean(value)
		if strings.EqualFold(filepath.Base(cleaned), "management.html") {
			add(cleaned)
			return
		}
		add(filepath.Join(cleaned, "management.html"))
	}

	addRootCandidates := func(root string) {
		root = strings.TrimSpace(root)
		if root == "" {
			return
		}
		cleaned := filepath.Clean(root)
		if strings.EqualFold(filepath.Ext(cleaned), ".html") {
			add(cleaned)
			return
		}
		add(filepath.Join(cleaned, "management.html"))
		add(filepath.Join(cleaned, "static", "management.html"))
	}

	addEnvCandidate(os.Getenv("MANAGEMENT_STATIC_PATH"))
	for _, key := range []string{"WRITABLE_PATH", "writable_path"} {
		if writable := strings.TrimSpace(os.Getenv(key)); writable != "" {
			add(filepath.Join(filepath.Clean(writable), "static", "management.html"))
		}
	}

	addRootCandidates(runtimeDir)
	for _, root := range roots {
		addRootCandidates(root)
	}

	return candidates
}

func firstExistingPath(paths []string) string {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func findAvailablePort(host string, start, attempts int) (int, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		port := start + i
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			lastErr = err
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	return 0, lastErr
}
