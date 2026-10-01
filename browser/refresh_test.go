package browser

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestStatusPollCannotInvalidatePendingQR(t *testing.T) {
	var revision int
	save := func() error { revision++; return nil }
	resume := PauseCookieRefresh()
	for i := 0; i < 5; i++ {
		if err := refreshCookies(save); err != nil {
			t.Fatal(err)
		}
	}
	if revision != 0 {
		t.Fatal("status poll consumed QR revision")
	}
	if err := save(); err != nil {
		t.Fatal(err)
	} // 显式扫码提交不被普通刷新暂停影响。
	resume()
	resume()
	if err := refreshCookies(save); err != nil {
		t.Fatal(err)
	}
	if revision != 2 {
		t.Fatal("refresh did not resume")
	}
}

func TestNewQRPauseWaitsForPendingRefresh(t *testing.T) {
	entered, release, paused := make(chan struct{}), make(chan struct{}), make(chan func(), 1)
	var committed atomic.Bool
	go func() {
		_ = refreshCookies(func() error { close(entered); <-release; committed.Store(true); return nil })
	}()
	<-entered
	go func() { paused <- PauseCookieRefresh() }()
	select {
	case stop := <-paused:
		stop()
		t.Fatal("QR loaded before prior refresh finished")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	stop := <-paused
	defer stop()
	if !committed.Load() {
		t.Fatal("prior refresh was not committed")
	}
}

func TestOverlappingQRPauses(t *testing.T) {
	first, second := PauseCookieRefresh(), PauseCookieRefresh()
	first()
	_ = refreshCookies(func() error { t.Fatal("new QR still active"); return nil })
	second()
	called := false
	_ = refreshCookies(func() error { called = true; return nil })
	if !called {
		t.Fatal("all QR browsers closed but refresh stayed paused")
	}
}
