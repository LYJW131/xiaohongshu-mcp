// sessionprobe 仅用固定合成数据验证容器销毁后的会话持久化。
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

const (
	markerPath       = "/app/data/session-probe-marker"
	cookiePath       = "/app/data/cookies.json"
	initialCookies   = `[{"name":"xhs-synthetic-session-probe","value":"synthetic-initial-no-login","domain":"session-probe.invalid","path":"/"}]`
	refreshedCookies = `[{"name":"xhs-synthetic-session-probe","value":"synthetic-refreshed-no-login","domain":"session-probe.invalid","path":"/"}]`
	syntheticSeed    = 23088
)

var version = "dev"

type phaseReport struct {
	Version    string          `json:"version"`
	Phase      string          `json:"phase"`
	Result     string          `json:"result"`
	Checks     map[string]bool `json:"checks"`
	MarkerLost bool            `json:"markerLost"`
	ProcessID  int             `json:"processId"`
	Error      string          `json:"error,omitempty"`
}

type storeFactory func() cookies.Cookier

func newReport(phase string) phaseReport {
	return phaseReport{Version: version, Phase: phase, Result: "running", Checks: map[string]bool{}, ProcessID: os.Getpid()}
}

func validPhase(phase string) bool { return phase == "write" || phase == "restore" || phase == "empty" }

// 错误只暴露固定诊断，不把远端响应或 cookie 内容写入日志。
func runPhase(phase, marker string, factory storeFactory) (report phaseReport) {
	report = newReport(phase)
	report.Result = "failed"
	report.Error = "Synthetic persistence check failed"
	if !validPhase(phase) {
		return report
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		return report
	}
	if phase != "write" {
		report.MarkerLost = true
		report.Checks["ephemeralMarkerLost"] = true
	}
	store := factory()
	data, err := store.LoadCookies()
	if err != nil {
		return report
	}
	switch phase {
	case "write":
		// 拒绝覆盖已有会话，即便它看起来像另一轮测试。
		if !sameCookies(data, "[]") || store.LoadSeed() != 0 {
			return report
		}
		report.Checks["initialStateEmpty"] = true
		if store.SaveCookies([]byte(initialCookies)) != nil || store.SaveSeed(syntheticSeed) != nil {
			return report
		}
		if !matchesSession(factory(), initialCookies, syntheticSeed) {
			return report
		}
		report.Checks["syntheticStateSaved"] = true
	case "restore":
		if !sameCookies(data, initialCookies) || store.LoadSeed() != syntheticSeed {
			return report
		}
		report.Checks["syntheticStateRestored"] = true
		// store 保留首次加载的快照，用于验证删除后的陈旧写入。
		fresh := factory()
		if !matchesSession(fresh, initialCookies, syntheticSeed) {
			return report
		}
		if fresh.SaveCookies([]byte(refreshedCookies)) != nil || !matchesSession(factory(), refreshedCookies, syntheticSeed) {
			return report
		}
		report.Checks["syntheticStateRefreshed"] = true
		if fresh.DeleteCookies() != nil {
			return report
		}
		report.Checks["syntheticStateDeleted"] = true
		if !errors.Is(store.SaveCookies([]byte(initialCookies)), cookies.ErrConflict) {
			return report
		}
		report.Checks["staleWriteRejected"] = true
		if !matchesSession(factory(), "[]", 0) {
			return report
		}
		report.Checks["deletedStateEmpty"] = true
	case "empty":
		if !sameCookies(data, "[]") || store.LoadSeed() != 0 {
			return report
		}
		report.Checks["deletedStateStayedEmpty"] = true
	}
	if phase != "empty" {
		if writeMarker(marker) != nil {
			return report
		}
		report.Checks["ephemeralMarkerCreated"] = true
	}
	report.Result, report.Error = "passed", ""
	return report
}

func sameCookies(data []byte, want string) bool {
	var gotValue, wantValue []map[string]any
	return json.Unmarshal(data, &gotValue) == nil && gotValue != nil &&
		json.Unmarshal([]byte(want), &wantValue) == nil && reflect.DeepEqual(gotValue, wantValue)
}

func matchesSession(store cookies.Cookier, want string, seed int) bool {
	data, err := store.LoadCookies()
	return err == nil && sameCookies(data, want) && store.LoadSeed() == seed
}

func writeMarker(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = file.WriteString("synthetic-persistence-probe\n"); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func healthHandler(snapshot func() phaseReport) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(snapshot())
	})
}

func main() {
	phase := os.Getenv("XHS_SESSION_PROBE_PHASE")
	if !validPhase(phase) || !cookies.ExternalEnabled() {
		os.Exit(1)
	}
	var mu sync.RWMutex
	report := newReport(phase)
	go func() {
		result := runPhase(phase, markerPath, func() cookies.Cookier { return cookies.NewLoadCookie(cookiePath) })
		mu.Lock()
		report = result
		mu.Unlock()
	}()
	server := &http.Server{
		Addr:              ":18060",
		Handler:           healthHandler(func() phaseReport { mu.RLock(); defer mu.RUnlock(); return report }),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		os.Exit(1)
	}
}
