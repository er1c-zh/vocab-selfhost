package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ttsPriorityInteractive = 0
	ttsPriorityBackground  = 10
)

type ttsJob struct {
	filename string
	text     string
	voice    string
	priority int
	sequence uint64
	done     chan struct{}
	err      error
	running  bool
}

type ttsRequest struct {
	Text  string `json:"text"`
	Voice string `json:"voice"`
}

func (s *server) audioStorageDir() string {
	if s.audioDir != "" {
		return s.audioDir
	}
	return filepath.Join(s.dataDir, "audio")
}

func speechFilename(voice, text string) string {
	key := sha256.Sum256([]byte(voice + "\x00" + text))
	return hex.EncodeToString(key[:]) + ".wav"
}

func (s *server) startTTSQueue() {
	s.ttsInit.Do(func() {
		s.ttsCond = sync.NewCond(&s.ttsMu)
		s.ttsJobs = make(map[string]*ttsJob)
		go s.ttsWorker()
	})
}

// enqueueSpeech joins identical pending work. Interactive playback promotes an
// already queued background task, keeping one synthesis per voice and phrase.
func (s *server) enqueueSpeech(ctx context.Context, text, voice string, priority int, wait bool) error {
	filename := speechFilename(voice, text)
	fullPath := filepath.Join(s.audioStorageDir(), filename)
	if _, err := os.Stat(fullPath); err == nil {
		return nil
	}
	s.startTTSQueue()

	s.ttsMu.Lock()
	job := s.ttsJobs[filename]
	if job == nil {
		s.ttsSequence++
		job = &ttsJob{filename: filename, text: text, voice: voice, priority: priority, sequence: s.ttsSequence, done: make(chan struct{})}
		s.ttsJobs[filename] = job
		s.ttsQueue = append(s.ttsQueue, job)
		s.sortTTSQueueLocked()
		s.ttsEnqueued++
		s.ttsCond.Signal()
	} else if !job.running && priority < job.priority {
		job.priority = priority
		s.sortTTSQueueLocked()
		s.ttsPromoted++
	}
	s.ttsMu.Unlock()

	if !wait {
		return nil
	}
	select {
	case <-job.done:
		return job.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *server) sortTTSQueueLocked() {
	sort.SliceStable(s.ttsQueue, func(i, j int) bool {
		if s.ttsQueue[i].priority != s.ttsQueue[j].priority {
			return s.ttsQueue[i].priority < s.ttsQueue[j].priority
		}
		return s.ttsQueue[i].sequence < s.ttsQueue[j].sequence
	})
}

func (s *server) ttsWorker() {
	for {
		s.ttsMu.Lock()
		for len(s.ttsQueue) == 0 || s.ttsClearing {
			s.ttsCond.Wait()
		}
		job := s.ttsQueue[0]
		s.ttsQueue = s.ttsQueue[1:]
		job.running = true
		s.ttsActive = job
		s.ttsMu.Unlock()

		started := time.Now()
		err := s.generateSpeech(job)
		duration := time.Since(started)

		s.ttsMu.Lock()
		job.err = err
		if err == nil {
			s.ttsCompleted++
			s.ttsLastError = ""
		} else {
			s.ttsFailed++
			s.ttsLastError = err.Error()
		}
		s.ttsLastDuration = duration
		delete(s.ttsJobs, job.filename)
		s.ttsActive = nil
		close(job.done)
		s.ttsCond.Broadcast()
		s.ttsMu.Unlock()

		if err != nil {
			log.Printf("TTS task priority=%d duration=%.3fs result=error error=%q", job.priority, duration.Seconds(), err)
		} else {
			log.Printf("TTS task priority=%d duration=%.3fs result=stored", job.priority, duration.Seconds())
		}
	}
}

func (s *server) generateSpeech(job *ttsJob) error {
	fullPath := filepath.Join(s.audioStorageDir(), job.filename)
	if _, err := os.Stat(fullPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
		return fmt.Errorf("cannot access speech storage: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	started := time.Now()
	response, status, err := s.callAI(ctx, "/v1/tts", ttsRequest{Text: job.text, Voice: job.voice})
	if err != nil {
		return fmt.Errorf("AI speech generation failed (status %d): %w", status, err)
	}
	var generated struct {
		AudioBase64 string `json:"audioBase64"`
	}
	if err := json.Unmarshal(response, &generated); err != nil || generated.AudioBase64 == "" {
		return errors.New("AI service returned no audio")
	}
	wav, err := base64.StdEncoding.DecodeString(generated.AudioBase64)
	if err != nil || len(wav) < 44 || len(wav) > 24<<20 || string(wav[:4]) != "RIFF" {
		return errors.New("AI service returned invalid WAV audio")
	}
	tmpPath := fullPath + ".tmp"
	if err := os.WriteFile(tmpPath, wav, 0o640); err != nil {
		return fmt.Errorf("cannot write generated audio: %w", err)
	}
	if err := os.Rename(tmpPath, fullPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("cannot publish generated audio: %w", err)
	}
	log.Printf("TTS generation ai_duration=%.3fs audio_bytes=%d", time.Since(started).Seconds(), len(wav))
	return nil
}

func (s *server) cancelQueuedTTSLocked() int {
	queued := s.ttsQueue
	s.ttsQueue = nil
	for _, job := range queued {
		job.err = errors.New("speech task cancelled by audio cleanup")
		delete(s.ttsJobs, job.filename)
		close(job.done)
		s.ttsCancelled++
	}
	return len(queued)
}

func (s *server) ttsQueueSnapshot() map[string]any {
	s.ttsMu.Lock()
	defer s.ttsMu.Unlock()
	interactive, background := 0, 0
	for _, job := range s.ttsQueue {
		if job.priority == ttsPriorityInteractive {
			interactive++
		} else {
			background++
		}
	}
	active := any(nil)
	if s.ttsActive != nil {
		active = map[string]any{"priority": s.ttsActive.priority, "started": true}
	}
	return map[string]any{
		"queued": len(s.ttsQueue), "interactiveQueued": interactive, "backgroundQueued": background,
		"active": active, "enqueued": s.ttsEnqueued, "completed": s.ttsCompleted,
		"failed": s.ttsFailed, "cancelled": s.ttsCancelled, "promoted": s.ttsPromoted,
		"lastDurationSeconds": s.ttsLastDuration.Seconds(), "lastError": s.ttsLastError,
	}
}

func validateTTSRequest(req *ttsRequest) error {
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" || len(req.Text) > 1000 {
		return errors.New("text must contain 1-1000 characters")
	}
	if req.Voice == "" {
		req.Voice = "af_heart"
	}
	return nil
}
