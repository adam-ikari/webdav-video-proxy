package store

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	SetClock(func() int64 { return 1000 })
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close(); ResetClock() })
	return s
}

func TestProfileRoundTrip(t *testing.T) {
	s := newTestStore(t)
	p := ProfileRow{
		SubKey:        "https://a.com|阿里云盘",
		BandwidthMbps: 12.5,
		Friendly:      "friendly",
		SuggestedN:    4,
		IsSlow:        false,
		UpdatedAt:     1000,
	}
	if err := s.SaveProfile(p); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	got, ok, err := s.GetProfile("https://a.com|阿里云盘")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if !ok {
		t.Fatal("profile not found")
	}
	if got.BandwidthMbps != 12.5 || got.Friendly != "friendly" || got.SuggestedN != 4 {
		t.Errorf("got = %+v", got)
	}
}

func TestSamplesTrimToMax(t *testing.T) {
	s := newTestStore(t)
	key := "https://a.com|夸克网盘"
	for i := 0; i < 25; i++ {
		if err := s.AppendSample(key, float64(i)); err != nil {
			t.Fatalf("AppendSample: %v", err)
		}
	}
	samps, err := s.GetSamples(key, 20)
	if err != nil {
		t.Fatalf("GetSamples: %v", err)
	}
	if len(samps) != 20 {
		t.Fatalf("len = %d, want 20", len(samps))
	}
	if samps[0] != 5 || samps[19] != 24 {
		t.Errorf("samples range = %v..%v, want 5..24", samps[0], samps[19])
	}
}

func TestBlockPutGet(t *testing.T) {
	s := newTestStore(t)
	bk := BlockKey{
		SubKey:   "https://a.com|阿里云盘",
		FilePath: "/电影/x.mkv",
		Version:  "etag1",
		BlockIdx: 0,
	}
	data := []byte("hello-block-0")
	if err := s.PutBlock(bk, data); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	got, ok, err := s.GetBlock(bk)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if !ok {
		t.Fatal("block not found")
	}
	if string(got) != "hello-block-0" {
		t.Errorf("got = %q", got)
	}
}

func TestHasBlockAndDelete(t *testing.T) {
	s := newTestStore(t)
	bk := BlockKey{"https://a.com|阿里云盘", "/m.mkv", "e1", 3}
	s.PutBlock(bk, []byte("x"))
	ok, _ := s.HasBlock(bk)
	if !ok {
		t.Fatal("expected has block")
	}
	s.DeleteBlock(bk)
	ok, _ = s.HasBlock(bk)
	if ok {
		t.Fatal("expected deleted")
	}
}

func TestListLRUBlocks(t *testing.T) {
	s := newTestStore(t)
	for i := int64(0); i < 5; i++ {
		bk := BlockKey{"https://a.com|阿里云盘", "/m.mkv", "e1", i}
		s.PutBlock(bk, []byte{byte(i)})
	}
	rows, err := s.ListLRUBlocks(3)
	if err != nil {
		t.Fatalf("ListLRUBlocks: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len = %d, want 3", len(rows))
	}
}

func TestCacheTotalSize(t *testing.T) {
	s := newTestStore(t)
	for i := int64(0); i < 3; i++ {
		s.PutBlock(BlockKey{"k|s", "/m.mkv", "e1", i}, []byte{0, 1, 2, 3, 4, 5, 6, 7})
	}
	size, err := s.CacheTotalSize()
	if err != nil {
		t.Fatalf("CacheTotalSize: %v", err)
	}
	if size != 24 {
		t.Errorf("size = %d, want 24", size)
	}
}

func TestInvalidateFile(t *testing.T) {
	s := newTestStore(t)
	s.PutBlock(BlockKey{"k|s", "/m.mkv", "e1", 0}, []byte("x"))
	s.PutBlock(BlockKey{"k|s", "/m.mkv", "e1", 1}, []byte("y"))
	s.PutBlock(BlockKey{"k|s", "/other.mkv", "e1", 0}, []byte("z"))
	if err := s.InvalidateFile("k|s", "/m.mkv"); err != nil {
		t.Fatalf("InvalidateFile: %v", err)
	}
	ok, _ := s.HasBlock(BlockKey{"k|s", "/m.mkv", "e1", 0})
	if ok {
		t.Fatal("expected /m.mkv blocks gone")
	}
	ok, _ = s.HasBlock(BlockKey{"k|s", "/other.mkv", "e1", 0})
	if !ok {
		t.Fatal("expected /other.mkv block intact")
	}
}
