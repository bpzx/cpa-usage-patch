package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type TokenStats struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

type RequestDetail struct {
	Timestamp time.Time  `json:"timestamp"`
	LatencyMs int64      `json:"latency_ms"`
	Source    string     `json:"source"`
	AuthIndex string     `json:"auth_index"`
	Tokens    TokenStats `json:"tokens"`
	Failed    bool       `json:"failed"`
}

type PersistedUsageRecord struct {
	APIName   string `json:"api_name"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Endpoint  string `json:"endpoint"`
	AuthType  string `json:"auth_type"`
	APIKey    string `json:"api_key"`
	RequestID string `json:"request_id"`
	RequestDetail
}

type StatisticsSnapshot struct {
	TotalRequests int64 `json:"total_requests"`
	SuccessCount  int64 `json:"success_count"`
	FailureCount  int64 `json:"failure_count"`
	TotalTokens   int64 `json:"total_tokens"`

	APIs map[string]APISnapshot `json:"apis"`

	RequestsByDay  map[string]int64 `json:"requests_by_day"`
	RequestsByHour map[string]int64 `json:"requests_by_hour"`
	TokensByDay    map[string]int64 `json:"tokens_by_day"`
	TokensByHour   map[string]int64 `json:"tokens_by_hour"`
}

type APISnapshot struct {
	TotalRequests int64                    `json:"total_requests"`
	TotalTokens   int64                    `json:"total_tokens"`
	Models        map[string]ModelSnapshot `json:"models"`
}

type ModelSnapshot struct {
	TotalRequests int64           `json:"total_requests"`
	TotalTokens   int64           `json:"total_tokens"`
	Details       []RequestDetail `json:"details"`
}

type MergeResult struct {
	Added   int64 `json:"added"`
	Skipped int64 `json:"skipped"`
}

type apiStats struct {
	TotalRequests int64
	TotalTokens   int64
	Models        map[string]*modelStats
}

type modelStats struct {
	TotalRequests int64
	TotalTokens   int64
	Details       []RequestDetail
}

type UsageStore struct {
	mu sync.RWMutex

	totalRequests int64
	successCount  int64
	failureCount  int64
	totalTokens   int64

	apis           map[string]*apiStats
	requestsByDay  map[string]int64
	requestsByHour map[int]int64
	tokensByDay    map[string]int64
	tokensByHour   map[int]int64
	seen           map[string]struct{}

	recordsPath   string
	fileMu        sync.Mutex
	lastRecordAt  time.Time
	recordsLoaded int64
}

func NewUsageStore(recordsPath string) (*UsageStore, error) {
	store := &UsageStore{
		recordsPath:    recordsPath,
		apis:           make(map[string]*apiStats),
		requestsByDay:  make(map[string]int64),
		requestsByHour: make(map[int]int64),
		tokensByDay:    make(map[string]int64),
		tokensByHour:   make(map[int]int64),
		seen:           make(map[string]struct{}),
	}

	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *UsageStore) load() error {
	if s.recordsPath == "" {
		return nil
	}
	file, err := os.Open(s.recordsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record PersistedUsageRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if s.applyRecord(record) {
			s.recordsLoaded++
		}
	}
	return scanner.Err()
}

func (s *UsageStore) IngestRecord(record PersistedUsageRecord) (bool, error) {
	if !s.applyRecord(record) {
		return false, nil
	}
	if err := s.appendRecord(record); err != nil {
		return false, err
	}
	return true, nil
}

func (s *UsageStore) ImportSnapshot(snapshot StatisticsSnapshot) (MergeResult, error) {
	result := MergeResult{}
	for apiName, apiSnapshot := range snapshot.APIs {
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			continue
		}
		for modelName, modelSnapshot := range apiSnapshot.Models {
			for _, detail := range modelSnapshot.Details {
				record := PersistedUsageRecord{
					APIName:       apiName,
					Provider:      "",
					Model:         modelName,
					Endpoint:      "",
					AuthType:      "",
					APIKey:        "",
					RequestID:     "",
					RequestDetail: normalizeRequestDetail(detail),
				}
				added, err := s.IngestRecord(record)
				if err != nil {
					return result, err
				}
				if added {
					result.Added++
				} else {
					result.Skipped++
				}
			}
		}
	}
	return result, nil
}

func (s *UsageStore) Snapshot() StatisticsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := StatisticsSnapshot{
		TotalRequests:  s.totalRequests,
		SuccessCount:   s.successCount,
		FailureCount:   s.failureCount,
		TotalTokens:    s.totalTokens,
		APIs:           make(map[string]APISnapshot, len(s.apis)),
		RequestsByDay:  make(map[string]int64, len(s.requestsByDay)),
		RequestsByHour: make(map[string]int64, len(s.requestsByHour)),
		TokensByDay:    make(map[string]int64, len(s.tokensByDay)),
		TokensByHour:   make(map[string]int64, len(s.tokensByHour)),
	}

	for apiName, stats := range s.apis {
		apiSnapshot := APISnapshot{
			TotalRequests: stats.TotalRequests,
			TotalTokens:   stats.TotalTokens,
			Models:        make(map[string]ModelSnapshot, len(stats.Models)),
		}
		for modelName, modelStatsValue := range stats.Models {
			details := make([]RequestDetail, len(modelStatsValue.Details))
			copy(details, modelStatsValue.Details)
			apiSnapshot.Models[modelName] = ModelSnapshot{
				TotalRequests: modelStatsValue.TotalRequests,
				TotalTokens:   modelStatsValue.TotalTokens,
				Details:       details,
			}
		}
		out.APIs[apiName] = apiSnapshot
	}

	for day, count := range s.requestsByDay {
		out.RequestsByDay[day] = count
	}
	for hour, count := range s.requestsByHour {
		out.RequestsByHour[formatHour(hour)] = count
	}
	for day, count := range s.tokensByDay {
		out.TokensByDay[day] = count
	}
	for hour, count := range s.tokensByHour {
		out.TokensByHour[formatHour(hour)] = count
	}

	return out
}

func (s *UsageStore) Status() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]any{
		"total_requests":   s.totalRequests,
		"success_count":    s.successCount,
		"failure_count":    s.failureCount,
		"total_tokens":     s.totalTokens,
		"records_loaded":   s.recordsLoaded,
		"last_record_at":   s.lastRecordAt,
		"tracked_api_keys": len(s.apis),
	}
}

func (s *UsageStore) appendRecord(record PersistedUsageRecord) error {
	if s.recordsPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.recordsPath), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}

	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	file, err := os.OpenFile(s.recordsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func (s *UsageStore) applyRecord(record PersistedUsageRecord) bool {
	record = normalizePersistedRecord(record)
	if record.APIName == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := dedupKey(record)
	if _, exists := s.seen[key]; exists {
		return false
	}
	s.seen[key] = struct{}{}

	stats, ok := s.apis[record.APIName]
	if !ok {
		stats = &apiStats{Models: make(map[string]*modelStats)}
		s.apis[record.APIName] = stats
	}
	modelName := record.Model
	if modelName == "" {
		modelName = "unknown"
	}
	modelStatsValue, ok := stats.Models[modelName]
	if !ok {
		modelStatsValue = &modelStats{}
		stats.Models[modelName] = modelStatsValue
	}

	totalTokens := record.Tokens.TotalTokens
	stats.TotalRequests++
	stats.TotalTokens += totalTokens
	modelStatsValue.TotalRequests++
	modelStatsValue.TotalTokens += totalTokens
	modelStatsValue.Details = append(modelStatsValue.Details, record.RequestDetail)

	s.totalRequests++
	if record.Failed {
		s.failureCount++
	} else {
		s.successCount++
	}
	s.totalTokens += totalTokens

	dayKey := record.Timestamp.Format("2006-01-02")
	hourKey := record.Timestamp.Hour()
	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.tokensByDay[dayKey] += totalTokens
	s.tokensByHour[hourKey] += totalTokens

	if record.Timestamp.After(s.lastRecordAt) {
		s.lastRecordAt = record.Timestamp
	}
	return true
}

func normalizePersistedRecord(record PersistedUsageRecord) PersistedUsageRecord {
	record.APIName = strings.TrimSpace(record.APIName)
	record.Provider = strings.TrimSpace(record.Provider)
	record.Model = strings.TrimSpace(record.Model)
	record.Endpoint = strings.TrimSpace(record.Endpoint)
	record.AuthType = strings.TrimSpace(record.AuthType)
	record.APIKey = strings.TrimSpace(record.APIKey)
	record.RequestID = strings.TrimSpace(record.RequestID)
	record.RequestDetail = normalizeRequestDetail(record.RequestDetail)
	if record.APIName == "" {
		record.APIName = resolveAPIName(record)
	}
	if record.Model == "" {
		record.Model = "unknown"
	}
	return record
}

func normalizeRequestDetail(detail RequestDetail) RequestDetail {
	detail.Source = strings.TrimSpace(detail.Source)
	detail.AuthIndex = strings.TrimSpace(detail.AuthIndex)
	if detail.Timestamp.IsZero() {
		detail.Timestamp = time.Now().UTC()
	} else {
		detail.Timestamp = detail.Timestamp.UTC()
	}
	if detail.LatencyMs < 0 {
		detail.LatencyMs = 0
	}
	detail.Tokens = normalizeTokenStats(detail.Tokens)
	return detail
}

func normalizeTokenStats(tokens TokenStats) TokenStats {
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}
	if tokens.TotalTokens < 0 {
		tokens.TotalTokens = 0
	}
	return tokens
}

func resolveAPIName(record PersistedUsageRecord) string {
	if key := strings.TrimSpace(record.APIKey); key != "" {
		return key
	}
	if endpoint := strings.TrimSpace(record.Endpoint); endpoint != "" {
		return endpoint
	}
	if provider := strings.TrimSpace(record.Provider); provider != "" {
		return provider
	}
	return "unknown"
}

func dedupKey(record PersistedUsageRecord) string {
	if record.RequestID != "" {
		return "request_id:" + record.RequestID
	}
	return fmt.Sprintf(
		"%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d|%d",
		record.APIName,
		record.Model,
		record.Timestamp.UTC().Format(time.RFC3339Nano),
		record.Source,
		record.AuthIndex,
		record.Failed,
		record.Tokens.InputTokens,
		record.Tokens.OutputTokens,
		record.Tokens.ReasoningTokens,
		record.Tokens.CachedTokens,
		record.Tokens.TotalTokens,
		record.LatencyMs,
	)
}

func formatHour(hour int) string {
	if hour < 0 {
		hour = 0
	}
	hour = hour % 24
	return fmt.Sprintf("%02d", hour)
}
