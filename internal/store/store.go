package store

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

// 可注入时钟，测试用固定时间。
var clock = func() int64 { return time.Now().Unix() }

func SetClock(f func() int64) { clock = f }
func ResetClock()             { clock = func() int64 { return time.Now().Unix() } }

// Now 返回当前存储时钟（与 SetClock 注入的时钟同源），供其他包（assessor）
// 复用同一可测试时钟，避免双时钟导致画像过期判定不一致。
func Now() int64 { return clock() }

type Store struct {
	db *sql.DB
}

type ProfileRow struct {
	SubKey        string
	BandwidthMbps float64
	Friendly      string // friendly|unfriendly|throttled|unknown
	SuggestedN    int
	IsSlow        bool
	UpdatedAt     int64
}

type BlockKey struct {
	SubKey   string
	FilePath string
	Version  string
	BlockIdx int64
}

type LRUBlock struct {
	BlockKey
	Size     int64
	LastUsed int64
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 写串行
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS profiles (
  sub_key TEXT PRIMARY KEY,
  bandwidth_mbps REAL NOT NULL DEFAULT 0,
  friendly TEXT NOT NULL DEFAULT 'unknown',
  suggested_n INTEGER NOT NULL DEFAULT 1,
  is_slow INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS samples (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sub_key TEXT NOT NULL,
  throughput REAL NOT NULL,
  ts INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_sub ON samples(sub_key, id);
CREATE TABLE IF NOT EXISTS blocks (
  sub_key TEXT NOT NULL,
  file_path TEXT NOT NULL,
  version TEXT NOT NULL,
  block_idx INTEGER NOT NULL,
  data BLOB NOT NULL,
  size INTEGER NOT NULL,
  last_used INTEGER NOT NULL,
  PRIMARY KEY (sub_key, file_path, version, block_idx)
);
CREATE INDEX IF NOT EXISTS idx_blocks_lru ON blocks(last_used);
`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// ---- profiles ----

func (s *Store) GetProfile(subKey string) (ProfileRow, bool, error) {
	var p ProfileRow
	var isSlow int
	err := s.db.QueryRow(
		`SELECT sub_key, bandwidth_mbps, friendly, suggested_n, is_slow, updated_at FROM profiles WHERE sub_key=?`,
		subKey,
	).Scan(&p.SubKey, &p.BandwidthMbps, &p.Friendly, &p.SuggestedN, &isSlow, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	p.IsSlow = isSlow != 0
	return p, true, nil
}

func (s *Store) SaveProfile(p ProfileRow) error {
	_, err := s.db.Exec(
		`INSERT INTO profiles(sub_key, bandwidth_mbps, friendly, suggested_n, is_slow, updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(sub_key) DO UPDATE SET
  bandwidth_mbps=excluded.bandwidth_mbps,
  friendly=excluded.friendly,
  suggested_n=excluded.suggested_n,
  is_slow=excluded.is_slow,
  updated_at=excluded.updated_at`,
		p.SubKey, p.BandwidthMbps, p.Friendly, p.SuggestedN, boolToInt(p.IsSlow), p.UpdatedAt)
	return err
}

// ---- samples ----

func (s *Store) AppendSample(subKey string, throughput float64) error {
	_, err := s.db.Exec(
		`INSERT INTO samples(sub_key, throughput, ts) VALUES(?,?,?)`,
		subKey, throughput, clock())
	return err
}

// GetSamples 返回最近 limit 条样本（按插入正序）。
func (s *Store) GetSamples(subKey string, limit int) ([]float64, error) {
	rows, err := s.db.Query(
		`SELECT throughput FROM samples WHERE sub_key=? ORDER BY id DESC LIMIT ?`, subKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append([]float64{v}, out...) // 翻回正序
	}
	return out, rows.Err()
}

// ---- blocks ----

func (s *Store) PutBlock(bk BlockKey, data []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO blocks(sub_key, file_path, version, block_idx, data, size, last_used)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(sub_key, file_path, version, block_idx) DO UPDATE SET
  data=excluded.data, size=excluded.size, last_used=excluded.last_used`,
		bk.SubKey, bk.FilePath, bk.Version, bk.BlockIdx, data, len(data), clock())
	return err
}

func (s *Store) GetBlock(bk BlockKey) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRow(
		`SELECT data FROM blocks WHERE sub_key=? AND file_path=? AND version=? AND block_idx=?`,
		bk.SubKey, bk.FilePath, bk.Version, bk.BlockIdx).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	// 更新 last_used
	s.db.Exec(
		`UPDATE blocks SET last_used=? WHERE sub_key=? AND file_path=? AND version=? AND block_idx=?`,
		clock(), bk.SubKey, bk.FilePath, bk.Version, bk.BlockIdx)
	return data, true, nil
}

func (s *Store) HasBlock(bk BlockKey) (bool, error) {
	var x int
	err := s.db.QueryRow(
		`SELECT 1 FROM blocks WHERE sub_key=? AND file_path=? AND version=? AND block_idx=?`,
		bk.SubKey, bk.FilePath, bk.Version, bk.BlockIdx).Scan(&x)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) DeleteBlock(bk BlockKey) error {
	_, err := s.db.Exec(
		`DELETE FROM blocks WHERE sub_key=? AND file_path=? AND version=? AND block_idx=?`,
		bk.SubKey, bk.FilePath, bk.Version, bk.BlockIdx)
	return err
}

func (s *Store) ListLRUBlocks(limit int) ([]LRUBlock, error) {
	rows, err := s.db.Query(
		`SELECT sub_key, file_path, version, block_idx, size, last_used FROM blocks ORDER BY last_used ASC LIMIT ?`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LRUBlock
	for rows.Next() {
		var b LRUBlock
		if err := rows.Scan(&b.SubKey, &b.FilePath, &b.Version, &b.BlockIdx, &b.Size, &b.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) CacheTotalSize() (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(size),0) FROM blocks`).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n.Int64, nil
}

// InvalidateFile 删除某文件所有块（ETag 不匹配时整文件失效）。
func (s *Store) InvalidateFile(subKey, filePath string) error {
	_, err := s.db.Exec(
		`DELETE FROM blocks WHERE sub_key=? AND file_path=?`, subKey, filePath)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
