package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

type fakeBackend struct {
	data       string
	seed       int
	revision   int
	writes     int
	failLoad   bool
	failWrite  bool
	allowStale bool
}

type fakeStore struct {
	backend  *fakeBackend
	loaded   bool
	data     string
	seed     int
	revision int
}

func (b *fakeBackend) factory() cookies.Cookier { return &fakeStore{backend: b} }
func (s *fakeStore) LoadCookies() ([]byte, error) {
	if s.backend.failLoad {
		return nil, errors.New("private-cookie-value")
	}
	if !s.loaded {
		s.data, s.seed, s.revision = s.backend.data, s.backend.seed, s.backend.revision
		s.loaded = true
	}
	return []byte(s.data), nil
}
func (s *fakeStore) LoadSeed() int { _, _ = s.LoadCookies(); return s.seed }
func (s *fakeStore) save(data string, seed int) error {
	if _, err := s.LoadCookies(); err != nil {
		return err
	}
	if s.backend.failWrite {
		return errors.New("private-cookie-value")
	}
	if s.revision != s.backend.revision && !s.backend.allowStale {
		return cookies.ErrConflict
	}
	s.backend.writes++
	s.backend.revision++
	s.backend.data, s.backend.seed = data, seed
	s.data, s.seed, s.revision = data, seed, s.backend.revision
	return nil
}
func (s *fakeStore) SaveCookies(data []byte) error { return s.save(string(data), s.LoadSeed()) }
func (s *fakeStore) SaveSeed(seed int) error       { _, _ = s.LoadCookies(); return s.save(s.data, seed) }
func (s *fakeStore) DeleteCookies() error          { return s.save("[]", 0) }

func TestThreeFreshDisksPreserveThenDeleteSession(t *testing.T) {
	backend := &fakeBackend{data: "[]"}
	var previousMarker string
	for _, phase := range []string{"write", "restore", "empty"} {
		marker := filepath.Join(t.TempDir(), "session-probe-marker")
		if previousMarker != "" {
			if _, err := os.Stat(previousMarker); err != nil {
				t.Fatal("preceding startup did not leave its marker")
			}
		}
		report := runPhase(phase, marker, backend.factory)
		if report.Result != "passed" || report.Error != "" {
			t.Fatalf("%s: %#v", phase, report)
		}
		if report.MarkerLost != (phase != "write") {
			t.Fatalf("%s marker lost: %v", phase, report.MarkerLost)
		}
		if report.ProcessID != os.Getpid() {
			t.Fatal("unexpected process ID")
		}
		if phase == "restore" && (!report.Checks["staleWriteRejected"] || !report.Checks["syntheticStateRefreshed"] || !report.Checks["deletedStateEmpty"]) {
			t.Fatal("missing restore checks")
		}
		if phase != "empty" {
			previousMarker = marker
		}
	}
	if backend.data != "[]" || backend.seed != 0 {
		t.Fatal("session resurrected")
	}
	if backend.writes != 4 {
		t.Fatalf("committed mutations = %d, want 4", backend.writes)
	}
}

func TestWriteRefusesAnyExistingState(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		seed       int
	}{
		{"real cookie", `[{"name":"web_session","value":"private-cookie-value"}]`, 0},
		{"seed only", "[]", 123},
		{"existing synthetic", initialCookies, syntheticSeed},
		{"invalid data", "null", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeBackend{data: tc.data, seed: tc.seed}
			report := runPhase("write", filepath.Join(t.TempDir(), "marker"), backend.factory)
			if report.Result != "failed" || backend.writes != 0 {
				t.Fatal("existing state changed")
			}
			assertSanitized(t, report)
		})
	}
}

func TestRestartsRequireEphemeralMarkerLoss(t *testing.T) {
	for _, phase := range []string{"write", "restore", "empty"} {
		t.Run(phase, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "marker")
			if err := os.WriteFile(marker, []byte("marker"), 0600); err != nil {
				t.Fatal(err)
			}
			backend := &fakeBackend{data: initialCookies, seed: syntheticSeed}
			loads := 0
			report := runPhase(phase, marker, func() cookies.Cookier { loads++; return backend.factory() })
			if report.Result != "failed" || report.MarkerLost || loads != 0 || backend.writes != 0 {
				t.Fatal("persisted disk was accepted")
			}
		})
	}
}

func TestRestoreRefusesWrongSyntheticState(t *testing.T) {
	for _, tc := range []struct {
		data string
		seed int
	}{
		{"[]", 0}, {refreshedCookies, syntheticSeed}, {initialCookies, 123},
	} {
		backend := &fakeBackend{data: tc.data, seed: tc.seed}
		report := runPhase("restore", filepath.Join(t.TempDir(), "marker"), backend.factory)
		if report.Result != "failed" || backend.writes != 0 {
			t.Fatal("unexpected state was mutated")
		}
	}
}

func TestEmptyRejectsResurrectedCookieOrSeed(t *testing.T) {
	for _, tc := range []struct {
		data string
		seed int
	}{{initialCookies, 0}, {"[]", syntheticSeed}} {
		backend := &fakeBackend{data: tc.data, seed: tc.seed}
		report := runPhase("empty", filepath.Join(t.TempDir(), "marker"), backend.factory)
		if report.Result != "failed" || backend.writes != 0 {
			t.Fatal("resurrected state accepted")
		}
	}
}

func TestRestoreRequiresActualStaleWriteConflict(t *testing.T) {
	backend := &fakeBackend{data: initialCookies, seed: syntheticSeed, allowStale: true}
	report := runPhase("restore", filepath.Join(t.TempDir(), "marker"), backend.factory)
	if report.Result != "failed" || report.Checks["staleWriteRejected"] {
		t.Fatal("missing conflict accepted")
	}
}

func TestStoreErrorsAreSanitized(t *testing.T) {
	for _, load := range []bool{true, false} {
		backend := &fakeBackend{data: "[]", failLoad: load, failWrite: !load}
		report := runPhase("write", filepath.Join(t.TempDir(), "marker"), backend.factory)
		if report.Result != "failed" {
			t.Fatal("store error accepted")
		}
		assertSanitized(t, report)
	}
}

func TestInvalidPhaseDoesNotOpenStore(t *testing.T) {
	report := runPhase("unknown", filepath.Join(t.TempDir(), "marker"), func() cookies.Cookier { t.Fatal("store opened"); return nil })
	if report.Result != "failed" {
		t.Fatal("unknown phase accepted")
	}
}

func TestHealthOnlyReturnsSanitizedStructuredStatus(t *testing.T) {
	backend := &fakeBackend{data: "[]", failLoad: true}
	report := runPhase("write", filepath.Join(t.TempDir(), "marker"), backend.factory)
	handler := healthHandler(func() phaseReport { return report })
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/health", 200}, {"POST", "/health", 404}, {"GET", "/", 404}} {
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(tc.method, tc.path, nil))
		if out.Code != tc.status {
			t.Fatalf("status = %d", out.Code)
		}
		if tc.status == http.StatusOK {
			if out.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("health can be cached")
			}
			var got phaseReport
			if json.Unmarshal(out.Body.Bytes(), &got) != nil || got.Result != "failed" {
				t.Fatal("invalid health")
			}
			assertSanitized(t, got)
		}
	}
}

func assertSanitized(t *testing.T, report phaseReport) {
	t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-cookie-value", "synthetic-initial-no-login", "synthetic-refreshed-no-login"} {
		if strings.Contains(string(body), secret) {
			t.Fatal("report exposed cookie content")
		}
	}
}
