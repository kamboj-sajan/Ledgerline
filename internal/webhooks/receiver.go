package webhooks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Receiver is a sample merchant-side webhook handler: what a merchant
// should write. It verifies the signature and timestamp, then processes each
// event id once. Delivery is at-least-once, so a duplicate (a retry after a
// lost response, or a replay inside the 5-minute window) is acknowledged with
// 200 but not processed again. That turns at-least-once delivery into
// exactly-once effects.
type Receiver struct {
	Secrets   []string // every currently valid secret (two during a rotation)
	Tolerance time.Duration
	Store     *FileStore
	Log       *slog.Logger
	Now       func() time.Time // nil means time.Now
}

// ServeHTTP implements http.Handler.
func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	now := time.Now()
	if rc.Now != nil {
		now = rc.Now()
	}
	// Verify against the raw bytes, before parsing anything.
	if _, err := Verify(r.Header.Get(SignatureHeader), body, rc.Secrets, now, rc.Tolerance); err != nil {
		rc.Log.Warn("rejected webhook", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var ev struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		http.Error(w, "not an event", http.StatusBadRequest)
		return
	}
	duplicate, err := rc.Store.Record(ev.ID, ev.Type)
	if err != nil {
		// Not processed: answer 500 so the dispatcher retries.
		rc.Log.Error("recording event", "event", ev.ID, "error", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if duplicate {
		rc.Log.Info("duplicate event acknowledged", "event", ev.ID)
	} else {
		rc.Log.Info("processed event", "event", ev.ID, "type", ev.Type)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"received":true,"duplicate":%t}`, duplicate)
}

// FileStore records processed event ids in an append-only file, one
// "<id> <type>" line each, synced to disk before the receiver answers 200.
// The file is reloaded on start, so dedupe survives a restart or a crash.
type FileStore struct {
	mu   sync.Mutex
	f    *os.File
	seen map[string]bool
}

// OpenFileStore opens (or creates) the store at path.
func OpenFileStore(path string) (*FileStore, error) {
	ids, err := ProcessedIDs(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is operator-supplied configuration
	if err != nil {
		return nil, err
	}
	// A crash mid-write can leave a torn last line with no newline. Start a
	// fresh line so the next record is not glued onto it.
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				_ = f.Close()
				return nil, err
			}
		}
	}
	return &FileStore{f: f, seen: seen}, nil
}

// Record processes id once. It returns duplicate=true if id was already
// processed, in which case nothing is written.
func (s *FileStore) Record(id, eventType string) (duplicate bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[id] {
		return true, nil
	}
	if _, err := fmt.Fprintf(s.f, "%s %s\n", id, eventType); err != nil {
		return false, err
	}
	if err := s.f.Sync(); err != nil {
		return false, err
	}
	s.seen[id] = true
	return false, nil
}

// Close closes the file.
func (s *FileStore) Close() error { return s.f.Close() }

// ProcessedIDs reads every recorded id in order, duplicates included (there
// should be none). A torn last line, left by a crash mid-write, is ignored:
// that event was never acknowledged, so it will be delivered again.
func ProcessedIDs(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied configuration
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var ids []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		id, _, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(id, "evt_") {
			continue
		}
		ids = append(ids, id)
	}
	return ids, sc.Err()
}
