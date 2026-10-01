package browser

import "sync"

// QR 等待时暂停普通浏览器的刷新提交，避免状态轮询抢占登录快照版本。
var refreshGate struct {
	sync.RWMutex
	paused int
}

func PauseCookieRefresh() func() {
	refreshGate.Lock()
	refreshGate.paused++
	refreshGate.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			refreshGate.Lock()
			refreshGate.paused--
			refreshGate.Unlock()
		})
	}
}

func refreshCookies(save func() error) error {
	refreshGate.RLock()
	defer refreshGate.RUnlock()
	if refreshGate.paused > 0 {
		return nil
	}
	return save()
}
