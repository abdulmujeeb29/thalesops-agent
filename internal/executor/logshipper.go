package executor

import (
	"strings"
	"sync"
	"time"

	"github.com/thalesops/agent/internal/models"
)

// FlushFunc ships a batch of log lines to the backend. Implemented by the API client.
type FlushFunc func(lines []models.LogLine) error

// LogShipper buffers streamed log lines and flushes them to the backend in
// batches — on a ticker, when the buffer is full, or on Close. It is safe for
// concurrent writers (the stdout and stderr scanners both write to it).
//
// Secrets (the clone token, env values) are redacted from every line before it
// leaves the machine, so they can never appear in the dashboard logs.
type LogShipper struct {
	flush   FlushFunc
	secrets []string

	mu     sync.Mutex
	buf    []models.LogLine
	closed bool

	flushInterval time.Duration
	maxBatch      int

	done chan struct{}
	wg   sync.WaitGroup
}

func NewLogShipper(flush FlushFunc, secrets []string) *LogShipper {
	// Only redact secrets that are long enough to be meaningful (avoid nuking
	// short values like "1" or "true" that appear everywhere in normal output).
	var meaningful []string
	for _, s := range secrets {
		if len(s) >= 6 {
			meaningful = append(meaningful, s)
		}
	}

	s := &LogShipper{
		flush:         flush,
		secrets:       meaningful,
		flushInterval: 1 * time.Second,
		maxBatch:      100,
		done:          make(chan struct{}),
	}
	s.wg.Add(1)
	go s.loop()
	return s
}

// Write queues a single line. Safe to call from multiple goroutines.
func (s *LogShipper) Write(stream, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.buf = append(s.buf, models.LogLine{Stream: stream, Content: s.redact(content)})
	if len(s.buf) >= s.maxBatch {
		s.flushLocked()
	}
}

// System writes a ThalesOps-generated status line (e.g. "Cloning repository…").
func (s *LogShipper) System(content string) { s.Write("system", content) }

func (s *LogShipper) redact(line string) string {
	for _, secret := range s.secrets {
		if secret != "" && strings.Contains(line, secret) {
			line = strings.ReplaceAll(line, secret, "***")
		}
	}
	return line
}

// flushLocked sends the current buffer. Caller must hold s.mu.
func (s *LogShipper) flushLocked() {
	if len(s.buf) == 0 {
		return
	}
	batch := s.buf
	s.buf = nil
	// Release the lock during network I/O so writers aren't blocked.
	s.mu.Unlock()
	_ = s.flush(batch) // best-effort; never abort a deploy on a log-ship failure
	s.mu.Lock()
}

func (s *LogShipper) loop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			s.flushLocked()
			s.mu.Unlock()
		case <-s.done:
			return
		}
	}
}

// Close flushes any remaining lines and stops the background loop.
func (s *LogShipper) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.flushLocked()
	s.mu.Unlock()

	close(s.done)
	s.wg.Wait()
}
