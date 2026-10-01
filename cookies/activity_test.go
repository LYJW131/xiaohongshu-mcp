package cookies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestActivityCoversBackgroundWorkAndStops(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-XHS-Session-Client") != "1" {
			t.Error("invalid activity request")
		}
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { activityLoop(ctx, server.Client(), server.URL, time.Millisecond*5); close(done) }()
	deadline := time.After(time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatal("background activity did not renew")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("activity did not stop")
	}
	before := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != before {
		t.Fatal("activity continued after browser close")
	}
}

func TestLocalModeHasNoHeartbeat(t *testing.T) {
	t.Setenv("XHS_SESSION_STORE", "")
	stop := KeepActive()
	stop()
	stop()
}
