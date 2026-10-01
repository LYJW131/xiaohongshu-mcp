package configs

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// seedPersistenceStub 仅模拟持久化失败，不访问外部会话或真实 cookie。
type seedPersistenceStub struct {
	seed      int
	saveErr   error
	saveCalls int
	attempted int
}

func (s *seedPersistenceStub) LoadCookies() ([]byte, error) { return []byte("[]"), nil }
func (s *seedPersistenceStub) SaveCookies([]byte) error     { return nil }
func (s *seedPersistenceStub) DeleteCookies() error         { return nil }
func (s *seedPersistenceStub) LoadSeed() int                { return s.seed }
func (s *seedPersistenceStub) SaveSeed(seed int) error {
	s.saveCalls++
	s.attempted = seed
	if s.saveErr == nil {
		s.seed = seed
	}
	return s.saveErr
}

func TestResolveFingerprintSeedStrictPersistsOverride(t *testing.T) {
	t.Setenv("XHS_SESSION_STORE", "")
	t.Setenv("XHS_FP_SEED", "11111")
	path := filepath.Join(t.TempDir(), "session.json")
	store := cookies.NewLoadCookie(path)
	if err := store.SaveSeed(22222); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCookies([]byte(`[{"name":"web_session","value":"synthetic-only"}]`)); err != nil {
		t.Fatal(err)
	}
	seed, err := ResolveFingerprintSeedStrict(store)
	if err != nil || seed != 11111 {
		t.Fatalf("ResolveFingerprintSeedStrict = (%d, %v), want (11111, nil)", seed, err)
	}
	// 环境变量消失后仍使用上次真正采用的 seed。
	t.Setenv("XHS_FP_SEED", "")
	fresh := cookies.NewLoadCookie(path)
	seed, err = ResolveFingerprintSeedStrict(fresh)
	if err != nil || seed != 11111 || fresh.LoadSeed() != 11111 {
		t.Fatalf("restored seed = (%d, %v), want (11111, nil)", seed, err)
	}
	data, err := fresh.LoadCookies()
	if err != nil || string(data) == "[]" {
		t.Fatal("strict seed persistence removed existing cookies")
	}
}

func TestResolveFingerprintSeedStrictNeverReturnsUnsavedSeed(t *testing.T) {
	for _, test := range []struct {
		name string
		env  string
		seed int
	}{
		{"new random seed", "", 0},
		{"saved seed", "", 22222},
		{"environment override", "11111", 22222},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XHS_FP_SEED", test.env)
			failure := errors.New("synthetic persistence failure")
			store := &seedPersistenceStub{seed: test.seed, saveErr: failure}
			seed, err := ResolveFingerprintSeedStrict(store)
			if seed != 0 || !errors.Is(err, failure) {
				t.Fatalf("strict resolution = (%d, %v), want (0, persistence error)", seed, err)
			}
			if store.saveCalls != 1 || store.attempted <= 0 || store.seed != test.seed {
				t.Fatal("failed save changed the persisted seed or skipped persistence")
			}
			if test.env != "" && store.attempted != 11111 {
				t.Fatal("environment seed did not take priority")
			}
		})
	}
}

func TestResolveFingerprintSeedStrictReusesPersistedSeed(t *testing.T) {
	t.Setenv("XHS_FP_SEED", "")
	store := &seedPersistenceStub{}
	first, err := ResolveFingerprintSeedStrict(store)
	if err != nil || first <= 0 || store.seed != first {
		t.Fatalf("first strict resolution = (%d, %v)", first, err)
	}
	second, err := ResolveFingerprintSeedStrict(store)
	if err != nil || second != first {
		t.Fatalf("second strict resolution = (%d, %v), want %d", second, err, first)
	}
}
