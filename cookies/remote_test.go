package cookies

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const syntheticCookies = `[{"name":"web_session","value":"synthetic-session-only","domain":".xiaohongshu.com","expires":1.75e9}]`
const syntheticSeed = 23088

var operationUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type recordedMutation struct {
	method   string
	body     []byte
	mutation sessionMutation
}

// sessionBackend 只保存合成会话，模拟 CAS、tombstone 和 operation ID 去重。
type sessionBackend struct {
	mu            sync.Mutex
	snapshot      sessionSnapshot
	lastOperation *recordedMutation
	requests      []recordedMutation
	commits       int
	failStatus    int
	dropNextReply bool
}

func newSessionBackend(t *testing.T) (*sessionBackend, *httptest.Server) {
	t.Helper()
	backend := &sessionBackend{}
	server := httptest.NewServer(http.HandlerFunc(backend.serveHTTP))
	t.Cleanup(server.Close)
	return backend, server
}

func (s *sessionBackend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("X-XHS-Session-Client") != "1" || r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "invalid bridge headers", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet {
		if s.failStatus != 0 {
			http.Error(w, "synthetic-session-only", s.failStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(s.snapshot)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		http.Error(w, "invalid method", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	var mutation sessionMutation
	if json.Unmarshal(body, &mutation) != nil || !operationUUID.MatchString(mutation.OperationID) {
		http.Error(w, "invalid mutation", http.StatusBadRequest)
		return
	}
	record := recordedMutation{r.Method, body, mutation}
	s.requests = append(s.requests, record)
	if s.failStatus != 0 {
		http.Error(w, "synthetic-session-only", s.failStatus)
		return
	}
	if previous := s.lastOperation; previous != nil && previous.mutation.OperationID == mutation.OperationID {
		if previous.method != r.Method || !bytes.Equal(previous.body, body) {
			http.Error(w, "operation ID reused with different content", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(s.snapshot)
		return
	}
	if mutation.ExpectedRevision != s.snapshot.Revision {
		http.Error(w, "stale revision", http.StatusConflict)
		return
	}
	if r.Method == http.MethodPut {
		if mutation.Session == nil || mutation.Session.Version != 2 || !validCookies(mutation.Session.Cookies) {
			http.Error(w, "invalid session", http.StatusBadRequest)
			return
		}
		s.snapshot.Session = mutation.Session
	} else {
		s.snapshot.Session = nil
	}
	s.snapshot.Revision++
	s.commits++
	s.lastOperation = &record
	if s.dropNextReply {
		s.dropNextReply = false
		// 提交后直接断开连接，重试必须沿用同一 operation ID。
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	_ = json.NewEncoder(w).Encode(s.snapshot)
}

func assertRemoteSession(t *testing.T, store Cookier, wantCookies string, wantSeed int) {
	t.Helper()
	got, err := store.LoadCookies()
	if err != nil {
		t.Fatalf("LoadCookies: %v", err)
	}
	var gotJSON, wantJSON any
	if json.Unmarshal(got, &gotJSON) != nil || json.Unmarshal([]byte(wantCookies), &wantJSON) != nil {
		t.Fatal("session does not contain valid JSON")
	}
	gotCanonical, _ := json.Marshal(gotJSON)
	wantCanonical, _ := json.Marshal(wantJSON)
	if !bytes.Equal(gotCanonical, wantCanonical) {
		t.Fatal("restored cookies differ from the synthetic session")
	}
	if got := store.LoadSeed(); got != wantSeed {
		t.Fatalf("LoadSeed = %d, want %d", got, wantSeed)
	}
}

func TestRemoteSessionRoundTripWithoutDisk(t *testing.T) {
	_, server := newSessionBackend(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", dir)
	store := newRemoteCookie(server.URL)
	if err := VerifyStore(store); err != nil {
		t.Fatal(err)
	}
	assertRemoteSession(t, store, "[]", 0)
	if err := store.SaveSeed(syntheticSeed); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCookies([]byte(syntheticCookies)); err != nil {
		t.Fatal(err)
	}
	assertRemoteSession(t, newRemoteCookie(server.URL), syntheticCookies, syntheticSeed)
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("remote session wrote local files: %v, count=%d", err, len(files))
	}
	// 读取结果不能与缓存共享可写内存。
	got, err := store.LoadCookies()
	if err != nil {
		t.Fatal(err)
	}
	got[0] = '!'
	assertRemoteSession(t, store, syntheticCookies, syntheticSeed)
}

func TestRemoteCookiesThenSeedPreservesBoth(t *testing.T) {
	_, server := newSessionBackend(t)
	store := newRemoteCookie(server.URL)
	if err := store.SaveCookies([]byte(syntheticCookies)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSeed(syntheticSeed); err != nil {
		t.Fatal(err)
	}
	assertRemoteSession(t, newRemoteCookie(server.URL), syntheticCookies, syntheticSeed)
}

func TestRemoteLogoutRejectsStaleBrowser(t *testing.T) {
	backend, server := newSessionBackend(t)
	store := newRemoteCookie(server.URL)
	if err := store.SaveSeed(syntheticSeed); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCookies([]byte(syntheticCookies)); err != nil {
		t.Fatal(err)
	}
	stale := newRemoteCookie(server.URL)
	assertRemoteSession(t, stale, syntheticCookies, syntheticSeed)
	if err := store.DeleteCookies(); err != nil {
		t.Fatal(err)
	}
	assertRemoteSession(t, newRemoteCookie(server.URL), "[]", 0)
	if err := stale.SaveCookies([]byte(syntheticCookies)); !errors.Is(err, ErrConflict) {
		t.Fatalf("unchanged stale cookie write = %v, want ErrConflict", err)
	}
	changed := []byte(`[{"name":"web_session","value":"stale-rotation"}]`)
	if err := stale.SaveCookies(changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale cookie write = %v, want ErrConflict", err)
	}
	if err := stale.SaveSeed(syntheticSeed + 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale seed write = %v, want ErrConflict", err)
	}
	if err := stale.DeleteCookies(); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete = %v, want ErrConflict", err)
	}
	assertRemoteSession(t, newRemoteCookie(server.URL), "[]", 0)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.snapshot.Revision != 3 || backend.snapshot.Session != nil || backend.commits != 3 {
		t.Fatal("stale browser changed the logout tombstone")
	}
}

func TestRemoteTwoBrowsersCompareAndSwap(t *testing.T) {
	_, server := newSessionBackend(t)
	first, second := newRemoteCookie(server.URL), newRemoteCookie(server.URL)
	assertRemoteSession(t, first, "[]", 0)
	assertRemoteSession(t, second, "[]", 0)
	if err := first.SaveSeed(syntheticSeed); err != nil {
		t.Fatal(err)
	}
	if err := second.SaveCookies([]byte(syntheticCookies)); !errors.Is(err, ErrConflict) {
		t.Fatalf("second browser write = %v, want ErrConflict", err)
	}
	// 冲突后不能自动获取新版本并合并旧浏览器 cookie。
	if err := second.SaveCookies([]byte(syntheticCookies)); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeated stale write = %v, want ErrConflict", err)
	}
	assertRemoteSession(t, newRemoteCookie(server.URL), "[]", syntheticSeed)
	assertRemoteSession(t, second, "[]", 0)
}

func TestRemoteConcurrentBrowsersHaveOneWinner(t *testing.T) {
	backend, server := newSessionBackend(t)
	clients := []*remoteCookie{newRemoteCookie(server.URL), newRemoteCookie(server.URL)}
	for _, client := range clients {
		assertRemoteSession(t, client, "[]", 0)
	}
	start := make(chan struct{})
	results := make(chan error, len(clients))
	for i, client := range clients {
		go func() {
			<-start
			results <- client.SaveSeed(syntheticSeed + i)
		}()
	}
	close(start)
	var successes, conflicts int
	for range clients {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent write: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want one each", successes, conflicts)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.commits != 1 || backend.snapshot.Revision != 1 {
		t.Fatal("concurrent stale browsers both committed")
	}
}

func TestRemoteRetriesLostCommittedResponseIdempotently(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			backend, server := newSessionBackend(t)
			store := newRemoteCookie(server.URL)
			if err := store.SaveCookies([]byte(syntheticCookies)); err != nil {
				t.Fatal(err)
			}
			backend.mu.Lock()
			backend.dropNextReply = true
			beforeCommits := backend.commits
			beforeRequests := len(backend.requests)
			backend.mu.Unlock()
			var err error
			if method == http.MethodPut {
				err = store.SaveSeed(syntheticSeed)
			} else {
				err = store.DeleteCookies()
			}
			if err != nil {
				t.Fatalf("retry after lost commit reply: %v", err)
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			requests := backend.requests[beforeRequests:]
			if len(requests) != 2 || backend.commits != beforeCommits+1 {
				t.Fatalf("requests=%d, commit delta=%d; want 2 and 1", len(requests), backend.commits-beforeCommits)
			}
			if requests[0].method != method || requests[1].method != method || !bytes.Equal(requests[0].body, requests[1].body) {
				t.Fatal("retry changed method, operation ID, expected revision, or session")
			}
			if store.snapshot.Revision != backend.snapshot.Revision {
				t.Fatal("client did not retain the idempotent response revision")
			}
			if method == http.MethodDelete && store.snapshot.Session != nil {
				t.Fatal("delete retry did not retain the tombstone")
			}
		})
	}
}

func TestRemoteLostResponseReplayAfterLogoutIsRejected(t *testing.T) {
	backend, server := newSessionBackend(t)
	store := newRemoteCookie(server.URL)
	transport := store.client.Transport
	var intercepted bool
	store.client.Transport = remoteRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err != nil || r.Method != http.MethodPut || intercepted {
			return response, err
		}
		intercepted = true
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		// 原写入已提交，但响应丢失；在重试前另一个客户端完成退出。
		if err := newRemoteCookie(server.URL).DeleteCookies(); err != nil {
			return nil, fmt.Errorf("synthetic logout failed: %w", err)
		}
		return nil, errors.New("synthetic response loss after logout")
	})
	if err := store.SaveCookies([]byte(syntheticCookies)); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay after newer logout = %v, want ErrConflict", err)
	}
	assertRemoteSession(t, store, "[]", 0)
	assertRemoteSession(t, newRemoteCookie(server.URL), "[]", 0)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.commits != 2 || backend.snapshot.Revision != 2 || backend.snapshot.Session != nil {
		t.Fatal("old operation replay changed the newer logout tombstone")
	}
	if len(backend.requests) != 3 || !bytes.Equal(backend.requests[0].body, backend.requests[2].body) {
		t.Fatal("lost response retry did not replay the original mutation")
	}
}

func TestRemoteServerFailureLeavesSnapshotUnchanged(t *testing.T) {
	for _, operation := range []string{"cookies", "seed", "delete"} {
		t.Run(operation, func(t *testing.T) {
			backend, server := newSessionBackend(t)
			store := newRemoteCookie(server.URL)
			if err := store.SaveCookies([]byte(syntheticCookies)); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveSeed(syntheticSeed); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(store.snapshot)
			backend.mu.Lock()
			backend.failStatus = http.StatusInternalServerError
			beforeRequests := len(backend.requests)
			backend.mu.Unlock()
			var err error
			switch operation {
			case "cookies":
				err = store.SaveCookies([]byte(`[{"name":"web_session","value":"replacement"}]`))
			case "seed":
				err = store.SaveSeed(syntheticSeed + 1)
			case "delete":
				err = store.DeleteCookies()
			}
			if err == nil || strings.Contains(err.Error(), "synthetic-session-only") {
				t.Fatalf("failure was not reported safely: %v", err)
			}
			after, _ := json.Marshal(store.snapshot)
			if !bytes.Equal(before, after) {
				t.Fatal("failed persistence modified the cached snapshot")
			}
			assertRemoteSession(t, store, syntheticCookies, syntheticSeed)
			backend.mu.Lock()
			defer backend.mu.Unlock()
			requests := backend.requests[beforeRequests:]
			if len(requests) != 2 || !bytes.Equal(requests[0].body, requests[1].body) {
				t.Fatal("server-error retry did not reuse the exact mutation")
			}
		})
	}
}

func TestRemoteInvalidResponsesFailClosedWithoutLocalFallback(t *testing.T) {
	valid := `{"version":2,"seed":23088,"cookies":` + syntheticCookies + `}`
	cases := map[string]string{
		"broken JSON":         `{"synthetic-session-only":`,
		"JSON null":           `null`,
		"empty object":        `{}`,
		"missing revision":    `{"session":` + valid + `}`,
		"missing session":     `{"revision":1}`,
		"null revision":       `{"revision":null,"session":null}`,
		"negative revision":   `{"revision":-1,"session":null}`,
		"fractional revision": `{"revision":1.5,"session":null}`,
		"unsafe revision":     `{"revision":9007199254740992,"session":null}`,
		"wrong version":       `{"revision":1,"session":{"version":1,"cookies":[]}}`,
		"negative seed":       `{"revision":1,"session":{"version":2,"seed":-1,"cookies":[]}}`,
		"null cookies":        `{"revision":1,"session":{"version":2,"cookies":null}}`,
		"object cookies":      `{"revision":1,"session":{"version":2,"cookies":{}}}`,
		"unnamed cookie":      `{"revision":1,"session":{"version":2,"cookies":[{"value":"synthetic-session-only"}]}}`,
		"non-string cookie":   `{"revision":1,"session":{"version":2,"cookies":[{"name":"web_session","value":1}]}}`,
		"null cookie value":   `{"revision":1,"session":{"version":2,"cookies":[{"name":"web_session","value":null}]}}`,
		"malformed expires":   `{"revision":1,"session":{"version":2,"cookies":[{"name":"web_session","value":"synthetic-session-only","expires":"tomorrow"}]}}`,
		"malformed httpOnly":  `{"revision":1,"session":{"version":2,"cookies":[{"name":"web_session","value":"synthetic-session-only","httpOnly":"true"}]}}`,
		"oversized response":  `{"revision":1,"session":null,"padding":"` + strings.Repeat("x", maxSessionBytes+1024) + `"}`,
		"oversized cookies":   `{"revision":1,"session":{"version":2,"cookies":[{"name":"web_session","value":"` + strings.Repeat("x", maxSessionBytes) + `"}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "cookies.json")
			local := []byte(`{"version":2,"seed":98765,"cookies":[{"name":"web_session","value":"local-stale-secret"}]}`)
			if err := os.WriteFile(path, local, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XHS_SESSION_STORE", "cloudflare")
			var mutationCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutationCalls.Add(1)
				}
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			store, ok := NewLoadCookie(path).(*remoteCookie)
			if !ok {
				t.Fatal("external mode selected the local store")
			}
			store.endpoint = server.URL
			got, err := store.LoadCookies()
			if err == nil || got != nil || store.loaded {
				t.Fatalf("invalid response accepted: error=%v loaded=%v", err, store.loaded)
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-session-only") {
				t.Fatal("remote error exposed cookie content")
			}
			if err := VerifyStore(store); err == nil {
				t.Fatal("VerifyStore accepted the corrupt response")
			}
			if store.LoadSeed() != 0 {
				t.Fatal("failed remote load fell back to the local seed")
			}
			if err := store.SaveCookies([]byte(syntheticCookies)); err == nil {
				t.Fatal("SaveCookies proceeded after a failed remote load")
			}
			if mutationCalls.Load() != 0 {
				t.Fatal("invalid snapshot allowed a remote mutation")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(local, after) {
				t.Fatal("remote failure modified the local fallback file")
			}
		})
	}
}

func TestRemoteRejectsInvalidOutgoingSession(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"revision":0,"session":null}`)
	}))
	defer server.Close()
	store := newRemoteCookie(server.URL)
	for _, input := range []string{
		"null", `{}`, `[null]`, `[{"name":"web_session"}]`,
		`[{"name":"web_session","value":null}]`,
		`[{"name":"","value":"secret"}]`,
		`[{"name":"web_session","value":"synthetic-only","expires":"tomorrow"}]`,
		`[{"name":"web_session","value":"synthetic-only","httpOnly":"true"}]`,
		`[{"name":"web_session","value":"` + strings.Repeat("x", maxSessionBytes) + `"}]`,
	} {
		if err := store.SaveCookies([]byte(input)); err == nil {
			t.Fatal("invalid outgoing cookie array accepted")
		}
	}
	for _, seed := range []int{0, -1} {
		if err := store.SaveSeed(seed); err == nil {
			t.Fatal("nonpositive seed accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid outgoing data reached the bridge")
	}
}

func TestRemoteMutationEnvelopeSizeIsBounded(t *testing.T) {
	backend, server := newSessionBackend(t)
	store := newRemoteCookie(server.URL)
	prefix, suffix := `[{"name":"web_session","value":"`, `"}]`
	input := prefix + strings.Repeat("x", maxSessionBytes-len(prefix)-len(suffix)) + suffix
	if !validCookies([]byte(input)) {
		t.Fatal("test cookie array must fit exactly within the cookie limit")
	}
	if err := store.SaveCookies([]byte(input)); err == nil {
		t.Fatal("oversized mutation envelope accepted")
	}
	assertRemoteSession(t, store, "[]", 0)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.requests) != 0 || backend.commits != 0 {
		t.Fatal("oversized mutation reached the bridge")
	}
}

func TestRemoteInvalidAcknowledgementDoesNotChangeSnapshot(t *testing.T) {
	for _, revision := range []int64{0, 2} {
		t.Run(fmt.Sprint(revision), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"revision":0,"session":null}`)
					return
				}
				_ = json.NewEncoder(w).Encode(sessionSnapshot{Revision: revision, Session: &sessionFile{
					Version: 2, Seed: syntheticSeed, Cookies: json.RawMessage(syntheticCookies),
				}})
			}))
			defer server.Close()
			store := newRemoteCookie(server.URL)
			if err := store.SaveCookies([]byte(syntheticCookies)); err == nil {
				t.Fatal("mutation accepted an acknowledgement for the wrong revision")
			}
			assertRemoteSession(t, store, "[]", 0)
			if store.snapshot.Revision != 0 {
				t.Fatal("invalid acknowledgement changed the cached revision")
			}
		})
	}
}

func TestRemoteRedirectsAreBlocked(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var targetRequests atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetRequests.Add(1)
				_, _ = io.WriteString(w, `{"revision":1,"session":null}`)
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"revision":0,"session":null}`)
					return
				}
				http.Redirect(w, r, target.URL, status)
			}))
			defer source.Close()
			store := newRemoteCookie(source.URL)
			if err := store.SaveCookies([]byte(syntheticCookies)); err == nil {
				t.Fatal("redirected mutation succeeded")
			}
			if targetRequests.Load() != 0 {
				t.Fatal("bridge redirected cookies to another destination")
			}
		})
	}
}

func TestRemoteDisablesEnvironmentProxy(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "http://127.0.0.1:1")
	}
	store := newRemoteCookie(sessionEndpoint)
	transport, ok := store.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("session transport may leak cookies through an environment proxy")
	}
	if store.client.CheckRedirect == nil {
		t.Fatal("session client permits redirects")
	}
}

type remoteRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn remoteRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestRemoteErrorsDoNotExposeCookieData(t *testing.T) {
	store := newRemoteCookie("http://synthetic-session-only.invalid/")
	store.client.Transport = remoteRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("upstream included synthetic-session-only")
	})
	if _, err := store.LoadCookies(); err == nil || strings.Contains(err.Error(), "synthetic-session-only") {
		t.Fatalf("transport error not sanitized: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "synthetic-session-only", http.StatusForbidden)
	}))
	defer server.Close()
	if _, err := newRemoteCookie(server.URL).LoadCookies(); err == nil || strings.Contains(err.Error(), "synthetic-session-only") {
		t.Fatalf("server error body not sanitized: %v", err)
	}
}

func TestRemoteSessionSurvivesProcessRestart(t *testing.T) {
	_, server := newSessionBackend(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"write", "restore", "logout", "empty"} {
		dir := t.TempDir()
		cmd := exec.Command(executable, "-test.run=^TestRemoteSessionProcessHelper$", "-test.count=1")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "XHS_TEST_SESSION_CHILD="+mode, "XHS_TEST_SESSION_ENDPOINT="+server.URL, "TMPDIR="+dir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fresh process %s: %v\n%s", mode, err, output)
		}
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != 0 {
			t.Fatalf("fresh process %s used local persistence: %v, count=%d", mode, err, len(files))
		}
	}
}

// 子进程仅连接父测试的 httptest 服务，不启动浏览器、不使用真实 cookie。
func TestRemoteSessionProcessHelper(t *testing.T) {
	mode := os.Getenv("XHS_TEST_SESSION_CHILD")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	endpoint := os.Getenv("XHS_TEST_SESSION_ENDPOINT")
	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Fatal("subprocess endpoint must be the local synthetic backend")
	}
	store := newRemoteCookie(endpoint)
	switch mode {
	case "write":
		if err := store.SaveSeed(syntheticSeed); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveCookies([]byte(syntheticCookies)); err != nil {
			t.Fatal(err)
		}
	case "restore":
		assertRemoteSession(t, store, syntheticCookies, syntheticSeed)
	case "logout":
		assertRemoteSession(t, store, syntheticCookies, syntheticSeed)
		if err := store.DeleteCookies(); err != nil {
			t.Fatal(err)
		}
	case "empty":
		assertRemoteSession(t, store, "[]", 0)
	default:
		t.Fatalf("unknown subprocess mode %q", mode)
	}
}
