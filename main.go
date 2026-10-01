package main

import (
	"flag"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// version 构建版本号，发布时通过 -ldflags "-X main.version=vX.Y.Z" 注入。
var version = "dev"

func main() {
	var (
		headless bool
		port     string
		token    string
	)
	flag.BoolVar(&headless, "headless", true, "是否无头模式")
	flag.StringVar(&port, "port", ":18060", "端口")
	flag.StringVar(&token, "token", "", "鉴权 Token，留空则读取 AUTH_TOKEN")
	flag.Parse()
	if token == "" {
		token = os.Getenv("AUTH_TOKEN")
	}

	logrus.Infof("xiaohongshu-mcp version: %s", version)

	// 外部会话必须先恢复并确认 seed 持久化，失败时停止启动。
	store := cookies.NewLoadCookie(cookies.GetCookiesFilePath())
	if cookies.ExternalEnabled() {
		seed, err := configs.ResolveFingerprintSeedStrict(store)
		if err != nil {
			logrus.Fatalf("会话存储不可用，拒绝启动: %v", err)
		}
		configs.SetFingerprintSeed(seed)
	} else {
		configs.SetFingerprintSeed(configs.ResolveFingerprintSeed(store))
	}

	// 只用内置浏览器。启动时就备好，缺它直接退出，不拖到第一个请求才失败。
	binPath, err := browser.EnsureBrowser()
	if err != nil {
		logrus.Fatalf("%v", err)
	}
	logrus.Infof("using browser binary: %s", binPath)

	configs.InitHeadless(headless)
	// 入口层解析代理，经 configs 透传给浏览器工厂。
	configs.SetProxy(configs.ProxyFromEnv())

	// 初始化服务
	xiaohongshuService := NewXiaohongshuService()

	// 创建并启动应用服务器
	appServer := NewAppServer(xiaohongshuService, token)
	if err := appServer.Start(port); err != nil {
		logrus.Fatalf("failed to run server: %v", err)
	}
}
