package cookies

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

const maxSessionBytes = 64 * 1024
const sessionEndpoint = "http://xhs-session.internal/v1/session"

var ErrConflict = errors.New("session changed; discard stale browser and retry")

// remoteCookie 的快照属于一个浏览器；冲突后禁止合并旧 cookie。
type remoteCookie struct {
	mu       sync.Mutex
	endpoint string
	client   *http.Client
	loaded   bool
	snapshot sessionSnapshot
}

type sessionSnapshot struct {
	Revision int64        `json:"revision"`
	Session  *sessionFile `json:"session"`
}

type sessionMutation struct {
	ExpectedRevision int64        `json:"expected_revision"`
	OperationID      string       `json:"operation_id"`
	Session          *sessionFile `json:"session,omitempty"`
}

// ExternalEnabled 仅允许内置私有桥，不接受任意外部 cookie 接收地址。
func ExternalEnabled() bool { return os.Getenv("XHS_SESSION_STORE") == "cloudflare" }

func newRemoteCookie(endpoint string) *remoteCookie {
	return &remoteCookie{endpoint: endpoint, client: &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{Proxy: nil}, // 不把会话交给环境变量里的代理。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// VerifyStore 在启动浏览器前确认远端可读；失败时绝不回退本地残留。
func VerifyStore(store Cookier) error {
	if _, ok := store.(*remoteCookie); !ok {
		return nil
	}
	_, err := store.LoadCookies()
	return err
}

func (c *remoteCookie) load() error {
	if c.loaded {
		return nil
	}
	snapshot, err := c.request(http.MethodGet, nil)
	if err != nil {
		return err
	}
	c.snapshot, c.loaded = snapshot, true
	return nil
}

func (c *remoteCookie) LoadCookies() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(); err != nil {
		return nil, err
	}
	if c.snapshot.Session == nil {
		return []byte("[]"), nil
	}
	return append([]byte(nil), c.snapshot.Session.Cookies...), nil
}

func (c *remoteCookie) LoadSeed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.load() != nil || c.snapshot.Session == nil {
		return 0
	}
	return c.snapshot.Session.Seed
}

func (c *remoteCookie) SaveSeed(seed int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if seed <= 0 {
		return errors.New("session seed must be positive")
	}
	if err := c.load(); err != nil {
		return err
	}
	if c.snapshot.Session != nil && c.snapshot.Session.Seed == seed {
		return nil
	}
	f := c.session()
	f.Seed = seed
	return c.mutate(http.MethodPut, &f)
}

func (c *remoteCookie) SaveCookies(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !validCookies(data) {
		return errors.New("invalid cookie array")
	}
	if err := c.load(); err != nil {
		return err
	}
	f := c.session()
	f.Cookies = append([]byte(nil), data...)
	return c.mutate(http.MethodPut, &f)
}

func (c *remoteCookie) DeleteCookies() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(); err != nil {
		return err
	}
	// 保留递增版本的 tombstone，不能让旧浏览器重新写回已删除会话。
	return c.mutate(http.MethodDelete, nil)
}

func (c *remoteCookie) session() sessionFile {
	if c.snapshot.Session != nil {
		return *c.snapshot.Session
	}
	return sessionFile{Version: 2, Cookies: json.RawMessage("[]")}
}

func (c *remoteCookie) mutate(method string, f *sessionFile) error {
	if f != nil {
		f.SavedAt = time.Now().UTC().Format(time.RFC3339)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return errors.New("session operation ID unavailable")
	}
	// UUIDv4；重试复用同一个 ID 和内容，处理已提交但响应丢失。
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	h := hex.EncodeToString(id[:])
	opID := h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	payload, err := json.Marshal(sessionMutation{c.snapshot.Revision, opID, f})
	if err != nil {
		return errors.New("invalid session encoding")
	}
	if len(payload) > maxSessionBytes {
		return errors.New("session exceeds 64 KiB")
	}
	snapshot, err := c.request(method, payload)
	if err != nil {
		return err
	}
	if snapshot.Revision != c.snapshot.Revision+1 {
		return errors.New("invalid session revision acknowledgement")
	}
	c.snapshot = snapshot // 只有持久化成功后才更新本进程快照。
	return nil
}

func (c *remoteCookie) request(method string, payload []byte) (sessionSnapshot, error) {
	var result sessionSnapshot
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(method, c.endpoint, bytes.NewReader(payload))
		if err != nil {
			return result, errors.New("invalid session bridge configuration")
		}
		req.Header.Set("X-XHS-Session-Client", "1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.client.Do(req)
		if err != nil {
			if attempt == 0 {
				continue
			}
			return result, errors.New("session bridge unavailable")
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSessionBytes+1024+1))
		resp.Body.Close()
		if resp.StatusCode == http.StatusConflict {
			return result, ErrConflict
		}
		if resp.StatusCode >= 500 && attempt == 0 {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return result, fmt.Errorf("session bridge rejected request (%d)", resp.StatusCode)
		}
		if readErr != nil {
			if attempt == 0 {
				continue
			}
			return result, errors.New("session response interrupted")
		}
		var envelope struct {
			Revision *int64          `json:"revision"`
			Session  json.RawMessage `json:"session"`
		}
		if len(data) > maxSessionBytes+1024 || json.Unmarshal(data, &envelope) != nil || envelope.Revision == nil || *envelope.Revision < 0 || *envelope.Revision > (1<<53)-1 || len(envelope.Session) == 0 || json.Unmarshal(data, &result) != nil {
			return sessionSnapshot{}, errors.New("invalid session bridge response")
		}
		if result.Session != nil && (result.Session.Version != 2 || result.Session.Seed < 0 || result.Session.Seed > (1<<53)-1 || !validCookies(result.Session.Cookies)) {
			return result, errors.New("invalid stored session")
		}
		return result, nil
	}
	return result, errors.New("session bridge unavailable")
}

func validCookies(data []byte) bool {
	if len(data) > maxSessionBytes {
		return false
	}
	// 先用浏览器实际使用的 CDP 类型校验可选字段，避免驱动静默丢弃整组 cookie。
	var typed []*proto.NetworkCookie
	if json.Unmarshal(data, &typed) != nil {
		return false
	}
	var list []map[string]json.RawMessage
	if json.Unmarshal(data, &list) != nil || list == nil {
		return false
	}
	for _, cookie := range list {
		var name, value string
		if json.Unmarshal(cookie["name"], &name) != nil || name == "" || json.Unmarshal(cookie["value"], &value) != nil || bytes.Equal(bytes.TrimSpace(cookie["value"]), []byte("null")) {
			return false
		}
	}
	return true
}
