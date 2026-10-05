package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func (s *server) enqueueCardSpeech(card Card) {
	word := strings.TrimSpace(card.Word)
	if word == "" {
		return
	}
	if err := s.enqueueSpeech(context.Background(), word, "af_heart", ttsPriorityBackground, false); err != nil {
		log.Printf("TTS background enqueue failed error=%q", err)
	}
}

func migrateLegacyAudio(sourceDir, destinationDir string) (int, int64, error) {
	sourceAbs, sourceErr := filepath.Abs(sourceDir)
	destinationAbs, destinationErr := filepath.Abs(destinationDir)
	if sourceErr == nil && destinationErr == nil && filepath.Clean(sourceAbs) == filepath.Clean(destinationAbs) {
		return 0, 0, nil
	}
	entries, err := os.ReadDir(sourceDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if err := os.MkdirAll(destinationDir, 0o750); err != nil {
		return 0, 0, err
	}
	copied := 0
	var bytes int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".wav") {
			continue
		}
		sourcePath := filepath.Join(sourceDir, entry.Name())
		info, err := os.Stat(sourcePath)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		destinationPath := filepath.Join(destinationDir, entry.Name())
		source, err := os.Open(sourcePath)
		if err != nil {
			return copied, bytes, err
		}
		destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if errors.Is(err, os.ErrExist) {
			source.Close()
			continue
		}
		if err != nil {
			source.Close()
			return copied, bytes, err
		}
		n, copyErr := io.Copy(destination, source)
		closeErr := destination.Close()
		source.Close()
		if copyErr != nil {
			return copied, bytes, copyErr
		}
		if closeErr != nil {
			return copied, bytes, closeErr
		}
		copied++
		bytes += n
	}
	// Move byte-identical legacy files once every copy succeeded. This leaves
	// one authoritative directory for the management endpoint to report/clear.
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".wav") {
			continue
		}
		sourcePath := filepath.Join(sourceDir, entry.Name())
		destinationPath := filepath.Join(destinationDir, entry.Name())
		if !equalFiles(sourcePath, destinationPath) {
			return copied, bytes, errors.New("legacy speech file conflicts with destination: " + entry.Name())
		}
		if err := os.Remove(sourcePath); err != nil {
			return copied, bytes, err
		}
	}
	return copied, bytes, nil
}

func equalFiles(leftPath, rightPath string) bool {
	left, err := os.Open(leftPath)
	if err != nil {
		return false
	}
	defer left.Close()
	right, err := os.Open(rightPath)
	if err != nil {
		return false
	}
	defer right.Close()
	leftHash, rightHash := sha256.New(), sha256.New()
	if _, err := io.Copy(leftHash, left); err != nil {
		return false
	}
	if _, err := io.Copy(rightHash, right); err != nil {
		return false
	}
	return bytes.Equal(leftHash.Sum(nil), rightHash.Sum(nil))
}

func (s *server) handleAudioManagement(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		files, bytes, err := audioStorageSummary(s.audioStorageDir())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": s.audioStorageDir(), "files": files, "bytes": bytes, "queue": s.ttsQueueSnapshot()})
	case http.MethodDelete:
		files, bytes, cancelled, err := s.clearSpeechStorage(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		log.Printf("speech storage cleanup files=%d audio_bytes=%d cancelled_tasks=%d", files, bytes, cancelled)
		writeJSON(w, http.StatusOK, map[string]any{"deletedFiles": files, "deletedBytes": bytes, "cancelledTasks": cancelled})
	default:
		methodNotAllowed(w)
	}
}

func audioStorageSummary(directory string) (int, int64, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	files := 0
	var bytes int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".wav") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files++
		bytes += info.Size()
	}
	return files, bytes, nil
}

func (s *server) clearSpeechStorage(ctx context.Context) (files int, bytes int64, cancelled int, resultErr error) {
	s.audioCleanupMu.Lock()
	defer s.audioCleanupMu.Unlock()
	s.startTTSQueue()
	s.ttsMu.Lock()
	s.ttsClearing = true
	cancelled = s.cancelQueuedTTSLocked()
	s.ttsCond.Broadcast()
	for s.ttsActive != nil {
		done := s.ttsActive.done
		s.ttsMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			s.ttsMu.Lock()
			s.ttsClearing = false
			s.ttsCond.Broadcast()
			s.ttsMu.Unlock()
			return 0, 0, cancelled, ctx.Err()
		}
		s.ttsMu.Lock()
	}
	s.ttsMu.Unlock()
	defer func() {
		s.ttsMu.Lock()
		s.ttsClearing = false
		s.ttsCond.Broadcast()
		s.ttsMu.Unlock()
	}()

	entries, err := os.ReadDir(s.audioStorageDir())
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, cancelled, nil
	}
	if err != nil {
		return 0, 0, cancelled, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".wav") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(s.audioStorageDir(), entry.Name())); err != nil {
			return files, bytes, cancelled, err
		}
		files++
		bytes += info.Size()
	}
	return files, bytes, cancelled, nil
}

func (s *server) handleOps(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 1 || r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	switch parts[0] {
	case "metrics":
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		uptime := 0.0
		if !s.startedAt.IsZero() {
			uptime = time.Since(s.startedAt).Seconds()
		}
		files, bytes, storageErr := audioStorageSummary(s.audioStorageDir())
		storage := map[string]any{"path": s.audioStorageDir(), "files": files, "bytes": bytes}
		if storageErr != nil {
			storage["error"] = storageErr.Error()
		}
		databaseBytes := int64(0)
		if info, err := os.Stat(filepath.Join(s.dataDir, "vocab.db")); err == nil {
			databaseBytes = info.Size()
		}
		aiMetrics, aiErr := s.getAIMetrics(r.Context())
		appResources := processMetrics()
		appResources["uptimeSeconds"] = uptime
		appResources["goroutines"] = runtime.NumGoroutine()
		appResources["heapAllocBytes"] = memory.HeapAlloc
		appResources["heapSysBytes"] = memory.HeapSys
		appResources["heapObjects"] = memory.HeapObjects
		result := map[string]any{
			"sampledAt":       time.Now().UTC(),
			"app":             appResources,
			"speechStorage":   storage,
			"databaseStorage": map[string]any{"path": filepath.Join(s.dataDir, "vocab.db"), "bytes": databaseBytes},
			"tts":             s.ttsQueueSnapshot(),
			"ai":              aiMetrics,
		}
		if aiErr != nil {
			result["aiError"] = aiErr.Error()
		}
		writeJSON(w, http.StatusOK, result)
	case "logs":
		logs := map[string]any{"app": s.apiLogs.snapshot(300), "ai": []string{}}
		response, _, err := s.callAIRequest(r.Context(), http.MethodGet, "/v1/logs?limit=300", nil)
		if err != nil {
			logs["aiError"] = err.Error()
		} else {
			var aiLogs []string
			if err := json.Unmarshal(response, &aiLogs); err != nil {
				logs["aiError"] = "AI 服务日志格式不可用"
			} else {
				logs["ai"] = aiLogs
			}
		}
		writeJSON(w, http.StatusOK, logs)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func processMetrics() map[string]any {
	result := map[string]any{"rssBytes": nil, "containerMemory": containerMemoryMetrics()}
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if !strings.HasPrefix(line, "VmRSS:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) > 1 {
				if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					result["rssBytes"] = value * 1024
				}
			}
			break
		}
	}
	return result
}

func containerMemoryMetrics() map[string]any {
	result := map[string]any{"usageBytes": nil, "limitBytes": nil}
	for _, pair := range [][2]string{{"/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.max"}, {"/sys/fs/cgroup/memory/memory.usage_in_bytes", "/sys/fs/cgroup/memory/memory.limit_in_bytes"}} {
		usageRaw, usageErr := os.ReadFile(pair[0])
		limitRaw, limitErr := os.ReadFile(pair[1])
		if usageErr != nil || limitErr != nil {
			continue
		}
		usage, err := strconv.ParseUint(strings.TrimSpace(string(usageRaw)), 10, 64)
		if err != nil {
			continue
		}
		result["usageBytes"] = usage
		if strings.TrimSpace(string(limitRaw)) != "max" {
			if limit, err := strconv.ParseUint(strings.TrimSpace(string(limitRaw)), 10, 64); err == nil {
				result["limitBytes"] = limit
			}
		}
		break
	}
	return result
}

func (s *server) getAIMetrics(ctx context.Context) (map[string]any, error) {
	response, _, err := s.callAIRequest(ctx, http.MethodGet, "/v1/metrics", nil)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(response, &result); err != nil {
		return nil, err
	}
	return result, nil
}
