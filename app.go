package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web/usage-embed.html
var webAssets embed.FS

var (
	headOpenPattern   = regexp.MustCompile(`(?i)<head[^>]*>`)
	headClosePattern  = regexp.MustCompile(`(?i)</head>`)
	scriptOpenPattern = regexp.MustCompile(`(?i)<script\b`)
	bodyClosePattern  = regexp.MustCompile(`(?i)</body>`)
)

type UpstreamSession struct {
	APIBase       string    `json:"api_base"`
	ManagementKey string    `json:"management_key"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type CollectorState struct {
	Connected    bool      `json:"connected"`
	LastError    string    `json:"last_error"`
	LastPullAt   time.Time `json:"last_pull_at"`
	LastRecordAt time.Time `json:"last_record_at"`
	Imported     int64     `json:"imported"`
}

type UsageToggleState struct {
	Enabled      bool      `json:"enabled"`
	LastCheckAt  time.Time `json:"last_check_at"`
	LastEnableAt time.Time `json:"last_enable_at"`
	LastError    string    `json:"last_error"`
}

type App struct {
	cfg        Config
	logger     *log.Logger
	httpClient *http.Client
	store      *UsageStore
	usageHTML  []byte

	sessionMu      sync.RWMutex
	session        UpstreamSession
	sessionVersion atomic.Uint64

	collectorMu    sync.RWMutex
	collectorState CollectorState

	usageToggleMu    sync.RWMutex
	usageToggleState UsageToggleState
}

func NewApp(cfg Config) (*App, error) {
	usageHTML, err := webAssets.ReadFile("web/usage-embed.html")
	if err != nil {
		return nil, err
	}
	store, err := NewUsageStore(cfg.RecordsPath)
	if err != nil {
		return nil, err
	}

	return &App{
		cfg:    cfg,
		logger: log.New(os.Stdout, "[cpa-usage-patch] ", log.LstdFlags),
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		store:     store,
		usageHTML: usageHTML,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	if err := a.ensureManagementPatched(); err != nil {
		a.logger.Printf("initial patch warning: %v", err)
	}

	go a.patchLoop(ctx)
	go a.collectLoop(ctx)

	srv := &http.Server{
		Addr:              net.JoinHostPort(a.cfg.ListenHost, strconv.Itoa(a.cfg.ListenPort)),
		Handler:           http.HandlerFunc(a.handleHTTP),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	a.logger.Printf("listening on %s", a.publicBase())
	a.logger.Printf("watching %s", strings.Join(a.cfg.ManagementHTMLCandidates, ", "))
	a.logger.Printf("records file %s", a.cfg.RecordsPath)
	return srv.ListenAndServe()
}

func (a *App) publicBase() string {
	return "http://" + net.JoinHostPort(a.cfg.PublicHost, strconv.Itoa(a.cfg.ListenPort))
}

func (a *App) handleHTTP(w http.ResponseWriter, r *http.Request) {
	a.setCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch {
	case r.URL.Path == "/healthz":
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.URL.Path == "/api/session":
		a.handleSession(w, r)
	case r.URL.Path == "/api/state":
		a.handleState(w, r)
	case r.URL.Path == "/loader.js":
		a.serveLoaderJS(w, r)
	case r.URL.Path == "/usage-embed" || r.URL.Path == "/usage-embed.html":
		a.serveUsageEmbed(w, r)
	case r.URL.Path == "/v0/management/usage" && r.Method == http.MethodGet:
		a.handleUsageSnapshot(w, r)
	case r.URL.Path == "/v0/management/usage/export" && r.Method == http.MethodGet:
		a.handleUsageExport(w, r)
	case r.URL.Path == "/v0/management/usage/import" && r.Method == http.MethodPost:
		a.handleUsageImport(w, r)
	case strings.HasPrefix(r.URL.Path, "/v0/management/"):
		a.proxyManagement(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (a *App) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		APIBase       string `json:"api_base"`
		ManagementKey string `json:"management_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	apiBase := normalizeAPIBase(payload.APIBase)
	managementKey := strings.TrimSpace(payload.ManagementKey)

	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()

	if apiBase == "" {
		apiBase = a.session.APIBase
	}
	if managementKey == "" {
		managementKey = a.session.ManagementKey
	}
	if apiBase == "" {
		http.Error(w, "api_base is required", http.StatusBadRequest)
		return
	}

	changed := apiBase != a.session.APIBase || managementKey != a.session.ManagementKey
	a.session = UpstreamSession{
		APIBase:       apiBase,
		ManagementKey: managementKey,
		UpdatedAt:     time.Now().UTC(),
	}
	if changed {
		a.sessionVersion.Add(1)
		a.logger.Printf("updated upstream session: %s", apiBase)
		go func(session UpstreamSession) {
			if err := a.ensureUsageStatisticsEnabled(session); err != nil {
				a.logger.Printf("usage toggle warning: %v", err)
			}
		}(a.session)
	}

	a.writeJSON(w, http.StatusOK, a.session)
}

func (a *App) handleState(w http.ResponseWriter, _ *http.Request) {
	session, version, _ := a.currentSession()
	a.collectorMu.RLock()
	collector := a.collectorState
	a.collectorMu.RUnlock()
	a.usageToggleMu.RLock()
	usageToggle := a.usageToggleState
	a.usageToggleMu.RUnlock()

	a.writeJSON(w, http.StatusOK, map[string]any{
		"session":         session,
		"session_version": version,
		"collector":       collector,
		"usage_toggle":    usageToggle,
		"usage":           a.store.Status(),
		"public_base":     a.publicBase(),
		"management_html": a.managementHTMLPath(),
	})
}

func (a *App) serveLoaderJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, buildLoaderJS(a.publicBase(), a.cfg.PatchQueryKey))
}

func (a *App) serveUsageEmbed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(a.usageHTML)
}

func (a *App) handleUsageSnapshot(w http.ResponseWriter, _ *http.Request) {
	snapshot := a.store.Snapshot()
	a.writeJSON(w, http.StatusOK, map[string]any{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	})
}

func (a *App) handleUsageExport(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{
		"version":     1,
		"exported_at": time.Now().UTC(),
		"usage":       a.store.Snapshot(),
	}
	a.writeJSON(w, http.StatusOK, payload)
}

func (a *App) handleUsageImport(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Version int                `json:"version"`
		Usage   StatisticsSnapshot `json:"usage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		http.Error(w, "unsupported version", http.StatusBadRequest)
		return
	}
	result, err := a.store.ImportSnapshot(payload.Usage)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	snapshot := a.store.Snapshot()
	a.writeJSON(w, http.StatusOK, map[string]any{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}

func (a *App) proxyManagement(w http.ResponseWriter, r *http.Request) {
	session, _, ok := a.currentSession()
	if !ok || session.APIBase == "" {
		http.Error(w, "upstream session unavailable", http.StatusServiceUnavailable)
		return
	}

	targetURL, err := buildUpstreamURL(session.APIBase, r.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	copyHeaders(req.Header, r.Header)
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	req.Header.Del("Host")
	if session.ManagementKey != "" {
		req.Header.Set("Authorization", "Bearer "+session.ManagementKey)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (a *App) patchLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.PatchRefreshEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.ensureManagementPatched(); err != nil {
				a.logger.Printf("patch warning: %v", err)
			}
		}
	}
}

func (a *App) ensureManagementPatched() error {
	path := a.managementHTMLPath()
	if strings.TrimSpace(path) == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	block := fmt.Sprintf(
		"<!-- CPA_USAGE_PATCH_BEGIN -->\n<script data-cpa-usage-patch=\"true\" src=\"%s/loader.js\"></script>\n<!-- CPA_USAGE_PATCH_END -->",
		a.publicBase(),
	)
	patched := injectPatchBlock(string(data), block)
	if patched == string(data) {
		return nil
	}
	return os.WriteFile(path, []byte(patched), 0o644)
}

func (a *App) managementHTMLPath() string {
	if path := firstExistingPath(a.cfg.ManagementHTMLCandidates); path != "" {
		return path
	}
	return strings.TrimSpace(a.cfg.ManagementHTMLPath)
}

func injectPatchBlock(content, block string) string {
	const startMarker = "<!-- CPA_USAGE_PATCH_BEGIN -->"
	const endMarker = "<!-- CPA_USAGE_PATCH_END -->"

	cleaned := content
	start := strings.Index(content, startMarker)
	end := strings.Index(content, endMarker)
	if start >= 0 && end > start {
		end += len(endMarker)
		cleaned = content[:start] + content[end:]
	}

	if loc := headOpenPattern.FindStringIndex(cleaned); loc != nil {
		return cleaned[:loc[1]] + "\n" + block + "\n" + cleaned[loc[1]:]
	}
	if loc := scriptOpenPattern.FindStringIndex(cleaned); loc != nil {
		return cleaned[:loc[0]] + block + "\n" + cleaned[loc[0]:]
	}
	if loc := headClosePattern.FindStringIndex(cleaned); loc != nil {
		return cleaned[:loc[0]] + block + "\n" + cleaned[loc[0]:]
	}
	if loc := bodyClosePattern.FindStringIndex(cleaned); loc != nil {
		return cleaned[:loc[0]] + block + "\n" + cleaned[loc[0]:]
	}
	return cleaned + "\n" + block + "\n"
}

func (a *App) collectLoop(ctx context.Context) {
	for {
		session, version, ok := a.currentSession()
		if !ok {
			a.setCollectorState(false, "waiting for upstream session", time.Time{}, time.Time{}, 0)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		if err := a.maybeEnsureUsageStatisticsEnabled(session); err != nil {
			a.logger.Printf("usage toggle warning: %v", err)
		}

		if err := a.consumeQueue(ctx, session, version); err != nil && !errors.Is(err, context.Canceled) {
			a.setCollectorState(false, err.Error(), time.Time{}, time.Time{}, 0)
			a.logger.Printf("collector warning: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *App) consumeQueue(ctx context.Context, session UpstreamSession, version uint64) error {
	address, err := resolveDialAddress(session.APIBase)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return err
	}
	if err := writeRESPCommand(writer, "AUTH", session.ManagementKey); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if err := readSimpleStringReply(reader); err != nil {
		return err
	}

	a.setCollectorState(true, "", time.Now().UTC(), time.Time{}, 0)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if a.sessionVersion.Load() != version {
			return nil
		}

		if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
			return err
		}
		if err := writeRESPCommand(writer, "RPOP", "queue", "256"); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		items, err := readArrayOfBulkStrings(reader)
		if err != nil {
			return err
		}

		imported := int64(0)
		lastRecordAt := time.Time{}
		for _, item := range items {
			record, err := parseQueuePayload(item)
			if err != nil {
				continue
			}
			added, err := a.store.IngestRecord(record)
			if err != nil {
				a.logger.Printf("persist warning: %v", err)
				continue
			}
			if added {
				imported++
				if record.Timestamp.After(lastRecordAt) {
					lastRecordAt = record.Timestamp
				}
			}
		}
		a.setCollectorState(true, "", time.Now().UTC(), lastRecordAt, imported)

		waitFor := a.cfg.QueuePollEvery
		if len(items) > 0 {
			waitFor = 150 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitFor):
		}
	}
}

func (a *App) setCollectorState(connected bool, lastError string, lastPullAt, lastRecordAt time.Time, imported int64) {
	a.collectorMu.Lock()
	defer a.collectorMu.Unlock()

	a.collectorState.Connected = connected
	a.collectorState.LastError = lastError
	if !lastPullAt.IsZero() {
		a.collectorState.LastPullAt = lastPullAt
	}
	if !lastRecordAt.IsZero() {
		a.collectorState.LastRecordAt = lastRecordAt
	}
	a.collectorState.Imported += imported
}

func (a *App) maybeEnsureUsageStatisticsEnabled(session UpstreamSession) error {
	a.usageToggleMu.RLock()
	lastCheckAt := a.usageToggleState.LastCheckAt
	a.usageToggleMu.RUnlock()

	if !lastCheckAt.IsZero() && time.Since(lastCheckAt) < 15*time.Second {
		return nil
	}
	return a.ensureUsageStatisticsEnabled(session)
}

func (a *App) ensureUsageStatisticsEnabled(session UpstreamSession) error {
	if session.APIBase == "" || session.ManagementKey == "" {
		return nil
	}

	checkedAt := time.Now().UTC()
	a.setUsageToggleState(false, checkedAt, time.Time{}, "")

	targetURL, err := buildUpstreamURL(session.APIBase, &url.URL{Path: "/v0/management/usage-statistics-enabled"})
	if err != nil {
		a.setUsageToggleState(false, checkedAt, time.Time{}, err.Error())
		return err
	}

	enabled, err := a.fetchUsageStatisticsEnabled(session, targetURL)
	if err != nil {
		a.setUsageToggleState(false, checkedAt, time.Time{}, err.Error())
		return err
	}
	if enabled {
		a.setUsageToggleState(true, checkedAt, checkedAt, "")
		return nil
	}

	if err := a.putUsageStatisticsEnabled(session, targetURL, true); err != nil {
		a.setUsageToggleState(false, checkedAt, time.Time{}, err.Error())
		return err
	}

	a.logger.Printf("enabled upstream usage statistics")
	a.setUsageToggleState(true, checkedAt, time.Now().UTC(), "")
	return nil
}

func (a *App) fetchUsageStatisticsEnabled(session UpstreamSession, targetURL string) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+session.ManagementKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, fmt.Errorf("get usage-statistics-enabled failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, err
	}
	return payload["usage-statistics-enabled"], nil
}

func (a *App) putUsageStatisticsEnabled(session UpstreamSession, targetURL string, enabled bool) error {
	body, err := json.Marshal(map[string]bool{"value": enabled})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, targetURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+session.ManagementKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("put usage-statistics-enabled failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (a *App) setUsageToggleState(enabled bool, lastCheckAt, lastEnableAt time.Time, lastError string) {
	a.usageToggleMu.Lock()
	defer a.usageToggleMu.Unlock()

	a.usageToggleState.Enabled = enabled
	if !lastCheckAt.IsZero() {
		a.usageToggleState.LastCheckAt = lastCheckAt
	}
	if !lastEnableAt.IsZero() {
		a.usageToggleState.LastEnableAt = lastEnableAt
	}
	a.usageToggleState.LastError = lastError
}

func (a *App) currentSession() (UpstreamSession, uint64, bool) {
	a.sessionMu.RLock()
	defer a.sessionMu.RUnlock()
	session := a.session
	version := a.sessionVersion.Load()
	ok := session.APIBase != "" && session.ManagementKey != ""
	return session, version, ok
}

func (a *App) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (a *App) setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
}

func normalizeAPIBase(input string) string {
	base := strings.TrimSpace(input)
	base = strings.TrimSuffix(base, "/")
	base = strings.TrimSuffix(base, "/v0/management")
	base = strings.TrimSuffix(base, "/v0/management/")
	if base == "" {
		return ""
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	return base
}

func buildUpstreamURL(apiBase string, reqURL *url.URL) (string, error) {
	base, err := url.Parse(normalizeAPIBase(apiBase))
	if err != nil {
		return "", err
	}
	target := *base
	target.Path = singleJoin(base.Path, reqURL.Path)
	target.RawQuery = reqURL.RawQuery
	return target.String(), nil
}

func resolveDialAddress(apiBase string) (string, error) {
	parsed, err := url.Parse(normalizeAPIBase(apiBase))
	if err != nil {
		return "", err
	}
	host := parsed.Hostname()
	if host == "" {
		return "", fmt.Errorf("invalid upstream host")
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port), nil
}

func parseQueuePayload(data []byte) (PersistedUsageRecord, error) {
	var record PersistedUsageRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return PersistedUsageRecord{}, err
	}
	record = normalizePersistedRecord(record)
	return record, nil
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func singleJoin(basePath, requestPath string) string {
	switch {
	case basePath == "" || basePath == "/":
		return requestPath
	case strings.HasSuffix(basePath, "/") && strings.HasPrefix(requestPath, "/"):
		return basePath + strings.TrimPrefix(requestPath, "/")
	case !strings.HasSuffix(basePath, "/") && !strings.HasPrefix(requestPath, "/"):
		return basePath + "/" + requestPath
	default:
		return basePath + requestPath
	}
}

func writeRESPCommand(writer *bufio.Writer, args ...string) error {
	if _, err := fmt.Fprintf(writer, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(writer, "$%d\r\n%s\r\n", len(arg), arg); err != nil {
			return err
		}
	}
	return nil
}

func readSimpleStringReply(reader *bufio.Reader) error {
	prefix, err := reader.ReadByte()
	if err != nil {
		return err
	}
	line, err := readRESPLine(reader)
	if err != nil {
		return err
	}
	switch prefix {
	case '+':
		return nil
	case '-':
		return errors.New(line)
	default:
		return fmt.Errorf("unexpected RESP prefix %q", prefix)
	}
}

func readArrayOfBulkStrings(reader *bufio.Reader) ([][]byte, error) {
	prefix, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if prefix == '-' {
		line, err := readRESPLine(reader)
		if err != nil {
			return nil, err
		}
		return nil, errors.New(line)
	}
	if prefix != '*' {
		return nil, fmt.Errorf("unexpected RESP prefix %q", prefix)
	}
	line, err := readRESPLine(reader)
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(line)
	if err != nil || count < 0 {
		return nil, fmt.Errorf("invalid RESP array length %q", line)
	}

	items := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		item, err := readBulkString(reader)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func readBulkString(reader *bufio.Reader) ([]byte, error) {
	prefix, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if prefix != '$' {
		return nil, fmt.Errorf("unexpected bulk string prefix %q", prefix)
	}
	line, err := readRESPLine(reader)
	if err != nil {
		return nil, err
	}
	length, err := strconv.Atoi(line)
	if err != nil {
		return nil, fmt.Errorf("invalid bulk length %q", line)
	}
	if length == -1 {
		return nil, nil
	}
	if length < 0 {
		return nil, fmt.Errorf("invalid bulk length %d", length)
	}

	payload := make([]byte, length+2)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	if payload[length] != '\r' || payload[length+1] != '\n' {
		return nil, fmt.Errorf("invalid bulk string terminator")
	}
	return payload[:length], nil
}

func readRESPLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(line, "\r\n") {
		return "", fmt.Errorf("invalid RESP line terminator")
	}
	return strings.TrimSuffix(line, "\r\n"), nil
}
