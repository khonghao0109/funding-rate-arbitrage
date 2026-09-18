package coordinator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The lock file. A CACHE of what engines hold (rule 7): the venues are the
// evidence, and ReconcileActivePositions re-derives occupancy from them whatever
// the file says.

// lockFileVersion changes whenever the file's meaning does. A file of another
// version is not read as this one — it is moved aside like a corrupt one.
const lockFileVersion = 1

type lockFile struct {
	Version     int          `json:"version"`
	WrittenAtMs int64        `json:"written_at_ms"`
	Locks       []SymbolLock `json:"locks"`
}

// LoadReport says what New found on disk.
type LoadReport struct {
	Path  string
	Found bool
	Locks int

	// CorruptMovedTo is where an unreadable file was moved. It is kept, never
	// overwritten: it is the only record of what the last process believed.
	CorruptMovedTo string
	ProblemsVI     []string
}

// loadLocks reads the file. A missing file is an empty table; a file that does
// not decode as this version is moved aside and is also an empty table. Only a
// failure to READ or to MOVE is an error: without the move, the next write
// would destroy the evidence.
func loadLocks(path string, now func() time.Time) (LoadReport, []SymbolLock, error) {
	report := LoadReport{Path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		report.ProblemsVI = append(report.ProblemsVI, "không có tệp khóa — bắt đầu rỗng, chờ đối soát với sàn")
		return report, nil, nil
	}
	if err != nil {
		return report, nil, fmt.Errorf("coordinator: đọc tệp khóa %s: %w", path, err)
	}
	report.Found = true

	var f lockFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	decodeErr := dec.Decode(&f)
	if decodeErr == nil && f.Version != lockFileVersion {
		decodeErr = fmt.Errorf("phiên bản tệp %d, mã này đọc phiên bản %d", f.Version, lockFileVersion)
	}
	if decodeErr != nil {
		moved := path + ".corrupt-" + strconv.FormatInt(now().UnixMilli(), 10)
		if err := os.Rename(path, moved); err != nil {
			return report, nil, fmt.Errorf("coordinator: tệp khóa %s hỏng (%v) và không dời sang bên được (%w) — từ chối khởi động để không ghi đè bằng chứng", path, decodeErr, err)
		}
		report.CorruptMovedTo = moved
		report.ProblemsVI = append(report.ProblemsVI, fmt.Sprintf("tệp khóa hỏng (%v) — đã dời sang %s, bắt đầu rỗng, chờ đối soát với sàn", decodeErr, moved))
		return report, nil, nil
	}
	return report, f.Locks, nil
}

// persistLocked writes the whole table. The caller holds c.mu.
func (c *Coordinator) persistLocked() error {
	data, err := json.MarshalIndent(lockFile{
		Version: lockFileVersion, WrittenAtMs: c.cfg.Now().UnixMilli(), Locks: c.snapshotLocked(),
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(c.cfg.Path, append(data, '\n'))
}

// writeFileAtomic replaces path with data so that a reader — or a crash — sees
// either the old file or the new one, never half of either: write a temp file in
// the same directory, fsync it, rename it over the target, fsync the directory.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".coordinator-locks-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	// The rename is durable only once the directory entry is. Best effort: not
	// every filesystem lets a directory be synced, and the file itself already is.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
