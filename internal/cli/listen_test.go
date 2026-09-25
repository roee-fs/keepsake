package cli

import (
	"context"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

// freeAddr returns a loopback address with no listener on it.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startListen runs listen with a hanging handler on each addr and returns once every handler holds a request.
func startListen(t *testing.T, addrs ...string) <-chan error {
	t.Helper()
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	entered := make(chan struct{}, len(addrs))
	apps := map[string]http.Handler{}
	for _, addr := range addrs {
		apps[addr] = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			entered <- struct{}{}
			<-hang
		})
	}
	done := make(chan error, 1)
	go func() { done <- listen(apps) }()
	for _, addr := range addrs {
		go func() {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr, nil)
			for t.Context().Err() == nil {
				if resp, err := http.DefaultClient.Do(req); err == nil {
					resp.Body.Close()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	for range addrs {
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("listen: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("no request reached a handler")
		}
	}
	return done
}

func TestShutdownGivesUpOnARequestThatNeverFinishes(t *testing.T) {
	defer setShutdownTimeout(100 * time.Millisecond)()
	done := startListen(t, freeAddr(t))
	// listen holds SIGTERM, so the test process survives it.
	syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("listen waited on a request that never finishes")
	}
}

func TestShutdownStopsEveryListenerAtOnce(t *testing.T) {
	defer setShutdownTimeout(time.Second)()
	a, b := freeAddr(t), freeAddr(t)
	done := startListen(t, a, b)
	syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	var d net.Dialer
	for _, addr := range []string{a, b} {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		if c, err := d.DialContext(ctx, "tcp", addr); err == nil {
			c.Close()
			t.Errorf("%s still accepts connections while another server drains", addr)
		}
		cancel()
	}
	<-done
}
