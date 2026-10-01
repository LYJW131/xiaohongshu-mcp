package browser

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/headless_browser"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

type browserConfig struct {
	// fingerprintSeed 固定指纹 seed；>0 时钉死，同账号每次同一套指纹。0 = 每次随机。
	fingerprintSeed int
	// proxy 代理地址；非空时启用。
	proxy string
}

type Option func(*browserConfig)

// WithProxy 设置代理（http/https/socks5）。空字符串视为不启用。
func WithProxy(proxy string) Option {
	return func(c *browserConfig) {
		c.proxy = proxy
	}
}

// WithFingerprintSeed 设置 seed，seed<=0 视为未设，回退每次随机。
func WithFingerprintSeed(seed int) Option {
	return func(c *browserConfig) {
		c.fingerprintSeed = seed
	}
}

// maskProxyCredentials masks username and password in proxy URL for safe logging.
func maskProxyCredentials(proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil || u.User == nil {
		return proxyURL
	}
	cred := "***"
	if _, hasPassword := u.User.Password(); hasPassword {
		cred = "***:***"
	}
	// 直接在原串替换 userinfo，避免 url.String() 把 * 编码成 %2A（日志变乱码）。
	return strings.Replace(proxyURL, u.User.String()+"@", cred+"@", 1)
}

// Browser 保存创建时读取的版本，旧浏览器不能覆盖新登录或退出状态。
type Browser struct {
	*headless_browser.Browser
	store        cookies.Cookier
	rodBrowser   *rod.Browser
	mu           sync.Mutex
	refresh      bool
	closed       bool
	stopActivity func()
}

func (b *Browser) NewPage() *rod.Page {
	page := b.Browser.NewPage()
	b.mu.Lock()
	b.rodBrowser = page.Browser()
	b.mu.Unlock()
	return page
}

func (b *Browser) DisableCookieRefresh() { b.mu.Lock(); b.refresh = false; b.mu.Unlock() }

// SaveCookies 等待持久化完成；登录成功必须显式调用并检查错误。
func (b *Browser) SaveCookies() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.saveCookies()
}

// RefreshCookies 在后台扫码期间跳过普通刷新，扫码提交使用 SaveCookies。
func (b *Browser) RefreshCookies() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return refreshCookies(b.saveCookies)
}

func (b *Browser) saveCookies() error {
	if b.rodBrowser == nil {
		return nil
	}
	cks, err := b.rodBrowser.GetCookies()
	if err != nil {
		return err
	}
	data, err := json.Marshal(cks)
	if err != nil {
		return err
	}
	if string(data) == "null" {
		data = []byte("[]")
	}
	return b.store.SaveCookies(data)
}

func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	if b.stopActivity != nil {
		defer b.stopActivity()
	}
	if b.refresh {
		if err := refreshCookies(b.saveCookies); err != nil {
			logrus.Errorf("刷新会话持久化失败: %v", err)
		}
	}
	b.Browser.Close()
}

func NewBrowser(headless bool, options ...Option) *Browser {
	stopActivity := cookies.KeepActive()
	initialized := false
	defer func() {
		if !initialized {
			stopActivity()
		}
	}()
	cfg := &browserConfig{}
	for _, opt := range options {
		opt(cfg)
	}

	cookieLoader := cookies.NewLoadCookie(cookies.GetCookiesFilePath())
	cookieData, cookieErr := cookieLoader.LoadCookies()
	if cookieErr != nil {
		if cookies.ExternalEnabled() {
			panic(fmt.Sprintf("会话存储不可用，拒绝启动浏览器: %v", cookieErr))
		}
		logrus.Warnf("failed to load cookies: %v", cookieErr)
	}
	if cookies.ExternalEnabled() {
		if seed := cookieLoader.LoadSeed(); seed > 0 {
			cfg.fingerprintSeed = seed
		}
		if cfg.fingerprintSeed > 0 && cookieLoader.LoadSeed() == 0 {
			if err := cookieLoader.SaveSeed(cfg.fingerprintSeed); err != nil {
				panic("会话 seed 持久化失败")
			}
		}
	}

	// 只用内置浏览器，没有别的来源。二进制必须显式传给 go-rod，
	// 否则 rod 会自行下载一个默认 Chromium：它不是内置浏览器，也不认识下面
	// 这些 flag（未知 flag 被静默忽略，日志照样打印 "fingerprint enabled"），
	// 属于无声降级。宁可不启动，也不启动一个不对的浏览器。
	binPath, err := EnsureBrowser()
	if err != nil {
		panic(fmt.Sprintf("内置浏览器不可用，拒绝启动: %v", err))
	}

	opts := []headless_browser.Option{
		headless_browser.WithHeadless(headless),
		// 用内置浏览器的默认配置，不强制 UA。
		headless_browser.WithFingerprint(""), // 空 = 按运行 OS 自动：Linux→windows，mac→macos
		headless_browser.WithStealthJS(false),
		headless_browser.WithLanguage("zh-CN"), // 面向小红书
		// 品牌报 Chrome。
		// 注：hardware-concurrency 不设，交给 seed 派生。
		headless_browser.WithExtraFlags(map[string]string{"fingerprint-brand": "Chrome"}),
	}
	opts = append(opts, headless_browser.WithChromeBinPath(binPath))

	// 代理（由调用方经 Option 传入，env 读取放在入口层）。
	if cfg.proxy != "" {
		opts = append(opts, headless_browser.WithProxy(cfg.proxy))
		logrus.Infof("Using proxy: %s", maskProxyCredentials(cfg.proxy))
	}

	// 固定指纹 seed（由调用方经 Option 传入，env 读取放在入口层）。
	if cfg.fingerprintSeed > 0 {
		opts = append(opts, headless_browser.WithFingerprintSeed(cfg.fingerprintSeed))
		logrus.Infof("fingerprint seed pinned: %d", cfg.fingerprintSeed)
	}

	if len(cookieData) > 0 {
		opts = append(opts, headless_browser.WithCookies(string(cookieData)))
	}
	b := headless_browser.New(opts...)
	initialized = true
	return &Browser{Browser: b, store: cookieLoader, refresh: cookies.ExternalEnabled(), stopActivity: stopActivity}
}
