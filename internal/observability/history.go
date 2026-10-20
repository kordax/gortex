package observability

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// HistoryRetention is the maximum age of locally retained metric snapshots.
const HistoryRetention = 24 * time.Hour

const (
	historyMaxRows         = 8640
	historyMaxSeries       = 4096
	historyMaxSampleBytes  = 512 << 10
	historyMaxChartBytes   = 32 << 10
	historyMaxPayloadBytes = 32 << 20
	historyReadBytes       = 32 << 20
)

// HistorySample contains numeric measurements, never request arguments or logs.
// At is Unix milliseconds. Values uses canonical Prometheus sample identifiers,
// with label names sorted lexicographically.
type HistorySample struct {
	At     int64              `json:"at"`
	Values map[string]float64 `json:"values"`
}

// History is a dedicated, bounded SQLite store independent of the code graph.
// A single connection serializes its short transactions. Close waits for active
// operations and is safe to call more than once.
type History struct {
	mu       sync.RWMutex
	db       *sql.DB
	now      func() time.Time
	maxRows  int
	maxBytes int64
}

// OpenHistory opens a private metric history file. Missing directories are
// created with mode 0700 and the database is mode 0600. Existing parent aliases
// are resolved once; leaf symlinks and special files are refused. These checks
// do not protect against arbitrary concurrent filesystem changes by outsiders.
func OpenHistory(path string) (*History, error) { return openHistory(path, time.Now) }

func openHistory(path string, now func() time.Time) (_ *History, err error) {
	if path == "" {
		return nil, errors.New("metric history: empty path")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("metric history path: %w", err)
	}
	dir, err := historyDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, filepath.Base(path))
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if err := historyRegularFile(path + suffix); err != nil {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open metric history: %w", err)
	}
	defer func() { _ = file.Close() }()
	opened, statErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		_ = file.Close()
		return nil, errors.New("metric history: database entry changed during open")
	}
	db, err := sql.Open("sqlite", historyDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var applicationID, tableCount int
	if err = db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil {
		return nil, fmt.Errorf("identify metric history: %w", err)
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'").Scan(&tableCount); err != nil {
		return nil, err
	}
	if (applicationID != 0 && applicationID != 1196705864) || (applicationID == 0 && tableCount != 0) {
		return nil, errors.New("metric history: refusing a database owned by another application")
	}
	if err = file.Chmod(0600); err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	// DELETE avoids a long-lived WAL retaining expired measurements. FULL
	// auto-vacuum truncates freed pages at commit; secure_delete clears pages
	// that remain allocated. The database is capped at 72 MiB, allowing for
	// overflow-page slack on top of the 32 MiB payload quota. During a
	// transaction its DELETE rollback journal can temporarily hold at most
	// one copy of those pages plus headers, bounded below 73 MiB.
	for _, statement := range []string{
		"PRAGMA page_size=4096", "PRAGMA journal_mode=DELETE", "PRAGMA auto_vacuum=FULL", "PRAGMA max_page_count=18432", "PRAGMA application_id=1196705864",
		"CREATE TABLE IF NOT EXISTS metric_history (at INTEGER PRIMARY KEY, payload BLOB NOT NULL, raw_bytes INTEGER NOT NULL CHECK(raw_bytes > 0 AND raw_bytes <= 524288), chart_payload BLOB NOT NULL, chart_raw_bytes INTEGER NOT NULL CHECK(chart_raw_bytes > 0 AND chart_raw_bytes <= 32768))",
	} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			return nil, fmt.Errorf("initialize metric history: %w", err)
		}
	}
	var chartColumns int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('metric_history') WHERE name IN ('chart_payload','chart_raw_bytes')").Scan(&chartColumns); err != nil {
		return nil, err
	}
	if chartColumns != 2 {
		return nil, errors.New("metric history: unsupported schema, chart columns are missing")
	}
	var autoVacuum int
	if err = db.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&autoVacuum); err != nil {
		return nil, err
	}
	if autoVacuum != 1 {
		return nil, errors.New("metric history: database does not support required automatic reclamation")
	}
	h := &History{db: db, now: now, maxRows: historyMaxRows, maxBytes: historyMaxPayloadBytes}
	if err = h.Prune(ctx, now()); err != nil {
		return nil, err
	}
	return h, nil
}

func historyDSN(path string) string {
	slashed := filepath.ToSlash(path)
	// Drive-letter paths must be URI paths, not authorities: /C:/... renders
	// file:///C:/... while C:/... renders file://C:/... and SQLite rejects it.
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	dsn := url.URL{Scheme: "file", Path: slashed}
	query := dsn.Query()
	for _, pragma := range []string{"busy_timeout(2000)", "secure_delete(ON)", "temp_store(MEMORY)"} {
		query.Add("_pragma", pragma)
	}
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func historyDirectory(dir string) (string, error) {
	var missing []string
	existing := dir
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		missing = append(missing, filepath.Base(existing))
		next := filepath.Dir(existing)
		if next == existing {
			return "", errors.New("metric history: no existing parent directory")
		}
		existing = next
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
		if err := os.Mkdir(resolved, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err := os.Lstat(resolved)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("metric history: unsafe directory entry")
		}
	}
	return resolved, nil
}

func historyRegularFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("metric history: refusing symlink or special file")
	}
	return nil
}

// Append validates and atomically records a snapshot, discarding expired and
// oldest excess records. A repeated timestamp replaces that snapshot.
func (h *History) Append(ctx context.Context, sample HistorySample) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := h.now()
	if sample.At <= now.Add(-HistoryRetention).UnixMilli() || sample.At > now.UnixMilli() {
		return errors.New("metric history: timestamp outside retention window")
	}
	if err := validateHistoryValues(sample.Values); err != nil {
		return err
	}
	raw, err := json.Marshal(sample.Values)
	if err != nil {
		return err
	}
	if len(raw) > historyMaxSampleBytes {
		return errors.New("metric history: sample exceeds byte limit")
	}
	var payload bytes.Buffer
	writer, err := gzip.NewWriterLevel(&payload, gzip.BestSpeed)
	if err != nil {
		return err
	}
	if _, err = writer.Write(raw); err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	chartRaw, err := json.Marshal(dashboardChartSample(sample).Values)
	if err != nil {
		return err
	}
	if len(chartRaw) > historyMaxChartBytes {
		return errors.New("metric history: chart sample exceeds byte limit")
	}
	var chartPayload bytes.Buffer
	writer.Reset(&chartPayload)
	if _, err = writer.Write(chartRaw); err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	payloadBytes := int64(payload.Len() + chartPayload.Len())
	if payloadBytes > h.maxBytes {
		return errors.New("metric history: compressed sample exceeds storage limit")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.db == nil {
		return errors.New("metric history: closed")
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Reserve space before inserting, so a full database can reuse freed
	// pages instead of failing every new sample until old rows expire.
	if _, err = tx.ExecContext(ctx, "DELETE FROM metric_history WHERE at <= ? OR at > ? OR at = ?", now.Add(-HistoryRetention).UnixMilli(), now.UnixMilli(), sample.At); err != nil {
		return err
	}
	var newerRows int
	var newerBytes int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(length(payload)+length(chart_payload)),0) FROM metric_history WHERE at > ?", sample.At).Scan(&newerRows, &newerBytes); err != nil {
		return err
	}
	if newerRows >= h.maxRows || newerBytes > h.maxBytes-payloadBytes {
		// An out-of-order snapshot older than the retained prefix cannot
		// displace more recent measurements simply by arriving later.
		if err = h.pruneTx(ctx, tx, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err = trimHistoryTx(ctx, tx, h.maxRows-1, h.maxBytes-payloadBytes); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO metric_history(at,payload,raw_bytes,chart_payload,chart_raw_bytes) VALUES(?,?,?,?,?)", sample.At, payload.Bytes(), len(raw), chartPayload.Bytes(), len(chartRaw)); err != nil {
		return err
	}
	return tx.Commit()
}

// Read returns oldest-to-newest samples within the retention window. A positive
// limit selects evenly spaced timestamps, including the newest. Zero defaults
// to 720 points. Counts and decoded payload bytes have additional hard bounds.
func (h *History) Read(ctx context.Context, since time.Time, limit int) ([]HistorySample, error) {
	return h.read(ctx, since, limit, false)
}

// ReadCharts reads a small projection saved at collection time, avoiding
// decompression of per-repository series during routine dashboard polling.
func (h *History) ReadCharts(ctx context.Context, since time.Time, limit int) ([]HistorySample, error) {
	return h.read(ctx, since, limit, true)
}

func (h *History) read(ctx context.Context, since time.Time, limit int, charts bool) ([]HistorySample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 0 {
		return nil, errors.New("metric history: negative sample limit")
	}
	if limit == 0 {
		limit = 720
	}
	if limit > historyMaxRows {
		limit = historyMaxRows
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.db == nil {
		return nil, errors.New("metric history: closed")
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = h.pruneTx(ctx, tx, h.now()); err != nil {
		return nil, err
	}
	payloadColumn, rawColumn := "payload", "raw_bytes"
	if charts {
		payloadColumn, rawColumn = "chart_payload", "chart_raw_bytes"
	}
	var count, maxRaw int
	if err = tx.QueryRowContext(ctx, "SELECT count(*), coalesce(max("+rawColumn+"),0) FROM metric_history WHERE at >= ?", since.UnixMilli()).Scan(&count, &maxRaw); err != nil {
		return nil, err
	}
	if count == 0 {
		return []HistorySample{}, tx.Commit()
	}
	if maxRaw <= 0 || maxRaw > historyMaxSampleBytes {
		return nil, errors.New("metric history: invalid stored sample size")
	}
	if bounded := historyReadBytes / maxRaw; limit > bounded {
		limit = bounded
	}
	if limit > count {
		limit = count
	}
	indices := make([]string, limit)
	args := []any{since.UnixMilli()}
	for i := 0; i < limit; i++ {
		index := count
		if limit > 1 {
			index = 1 + i*(count-1)/(limit-1)
		}
		indices[i] = "?"
		args = append(args, index)
	}
	query := "SELECT at," + payloadColumn + "," + rawColumn + " FROM metric_history WHERE at IN (SELECT at FROM (SELECT at,row_number() OVER (ORDER BY at) AS position FROM metric_history WHERE at >= ?) WHERE position IN (" + strings.Join(indices, ",") + ")) ORDER BY at"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]HistorySample, 0, limit)
	var decoder *gzip.Reader
	defer func() {
		if decoder != nil {
			_ = decoder.Close()
		}
	}()
	for rows.Next() {
		var sample HistorySample
		var payload []byte
		var rawBytes int
		if err = rows.Scan(&sample.At, &payload, &rawBytes); err != nil {
			break
		}
		sample.Values, err = decodeHistoryValuesReader(payload, rawBytes, charts, &decoder)
		if err != nil {
			break
		}
		out = append(out, sample)
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Prune removes expired/future records and enforces row and compressed-byte
// ceilings. It also runs on open, append and read, even without new samples.
func (h *History) Prune(ctx context.Context, now time.Time) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.db == nil {
		return errors.New("metric history: closed")
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = h.pruneTx(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *History) pruneTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM metric_history WHERE at <= ? OR at > ?", now.Add(-HistoryRetention).UnixMilli(), now.UnixMilli()); err != nil {
		return err
	}
	return trimHistoryTx(ctx, tx, h.maxRows, h.maxBytes)
}

func trimHistoryTx(ctx context.Context, tx *sql.Tx, maxRows int, maxBytes int64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM metric_history WHERE at IN (SELECT at FROM metric_history ORDER BY at DESC LIMIT -1 OFFSET ?)", maxRows); err != nil {
		return err
	}
	var total int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(sum(length(payload)+length(chart_payload)),0) FROM metric_history").Scan(&total); err != nil {
		return err
	}
	if total <= maxBytes {
		return nil
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM metric_history WHERE at IN (SELECT at FROM (SELECT at,sum(length(payload)+length(chart_payload)) OVER (ORDER BY at DESC) AS bytes FROM metric_history) WHERE bytes > ?)", maxBytes)
	return err
}

func (h *History) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.db == nil {
		return nil
	}
	err := h.db.Close()
	h.db = nil
	return err
}

func decodeHistoryValues(payload []byte, expected int) (map[string]float64, error) {
	var reader *gzip.Reader
	values, err := decodeHistoryValuesReader(payload, expected, false, &reader)
	if reader != nil {
		_ = reader.Close()
	}
	return values, err
}

func decodeHistoryValuesReader(payload []byte, expected int, charts bool, reader **gzip.Reader) (map[string]float64, error) {
	maximum := historyMaxSampleBytes
	if charts {
		maximum = historyMaxChartBytes
	}
	if expected <= 0 || expected > maximum || len(payload) > maximum+1024 {
		return nil, errors.New("metric history: invalid stored payload size")
	}
	var err error
	if *reader == nil {
		*reader, err = gzip.NewReader(bytes.NewReader(payload))
	} else {
		err = (*reader).Reset(bytes.NewReader(payload))
	}
	if err != nil {
		return nil, fmt.Errorf("metric history: corrupt compressed sample: %w", err)
	}
	// The stored size is already capped. Asking for one extra byte verifies
	// EOF and gzip checksum without ReadAll's repeated buffer growth.
	raw := make([]byte, expected+1)
	if _, err := io.ReadFull(*reader, raw[:expected]); err != nil {
		return nil, fmt.Errorf("metric history: corrupt compressed sample: %w", err)
	}
	read, readErr := io.ReadFull(*reader, raw[expected:])
	if read != 0 {
		return nil, errors.New("metric history: stored sample size mismatch")
	}
	if !errors.Is(readErr, io.EOF) {
		return nil, fmt.Errorf("metric history: corrupt compressed sample trailer: %w", readErr)
	}
	var values map[string]float64
	if err := json.Unmarshal(raw[:expected], &values); err != nil {
		return nil, fmt.Errorf("metric history: corrupt sample JSON: %w", err)
	}
	if charts && len(values) == 0 {
		return values, nil
	}
	if err := validateHistoryValues(values); err != nil {
		return nil, err
	}
	return values, nil
}

func validateHistoryValues(values map[string]float64) error {
	if len(values) == 0 || len(values) > historyMaxSeries {
		return errors.New("metric history: invalid series count")
	}
	for key, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("metric history: non-finite measurement")
		}
		if !validHistorySeries(key) {
			return errors.New("metric history: malformed series identifier")
		}
	}
	return nil
}

func validHistorySeries(key string) bool {
	if len(key) == 0 || len(key) > 2048 || !utf8.ValidString(key) {
		return false
	}
	metric, labels, hasLabels := strings.Cut(key, "{")
	if !historyName(metric, true) {
		return false
	}
	if !hasLabels {
		return true
	}
	if len(labels) < 2 || labels[len(labels)-1] != '}' {
		return false
	}
	labels = labels[:len(labels)-1]
	previous := ""
	for labels != "" {
		name, rest, ok := strings.Cut(labels, "=")
		if !ok || !historyName(name, false) || len(rest) < 2 || rest[0] != '"' {
			return false
		}
		if name <= previous {
			return false
		}
		previous = name
		end := 1
		for end < len(rest) && rest[end] != '"' {
			if rest[end] < 32 || rest[end] == 127 {
				return false
			}
			if rest[end] == '\\' {
				end++
				if end >= len(rest) || (rest[end] != '\\' && rest[end] != '"' && rest[end] != 'n') {
					return false
				}
			}
			end++
		}
		if end >= len(rest) {
			return false
		}
		if _, err := strconv.Unquote(rest[:end+1]); err != nil {
			return false
		}
		labels = rest[end+1:]
		if labels == "" {
			break
		}
		if labels[0] != ',' || len(labels) == 1 {
			return false
		}
		labels = labels[1:]
	}
	return true
}

func historyName(name string, metric bool) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if c == '_' || (metric && c == ':') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}
