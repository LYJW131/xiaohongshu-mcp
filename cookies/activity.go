package cookies

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// KeepActive 在浏览器工作及提交 cookie 期间保活；不延长平台的硬截止时间。
func KeepActive() func() {
	if !ExternalEnabled() {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	client := newRemoteCookie(sessionEndpoint).client
	go activityLoop(ctx, client, "http://xhs-session.internal/v1/activity", 15*time.Second)
	var once sync.Once
	return func() { once.Do(cancel) }
}

func activityLoop(ctx context.Context, client *http.Client, endpoint string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
		if err != nil {
			return
		}
		req.Header.Set("X-XHS-Session-Client", "1")
		if response, err := client.Do(req); err == nil {
			response.Body.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
