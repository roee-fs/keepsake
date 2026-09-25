package cli

import (
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

func TestShutdownGivesUpOnARequestThatNeverFinishes(t *testing.T) {
	defer setShutdownTimeout(100 * time.Millisecond)()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	entered, hang := make(chan struct{}), make(chan struct{})
	defer close(hang)
	done := make(chan error, 1)
	go func() {
		done <- listen(map[string]http.Handler{addr: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(entered)
			<-hang
		})})
	}()
	go func() {
		for {
			if resp, err := http.Get("http://" + addr); err == nil {
				resp.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	<-entered
	// listen holds SIGTERM, so the test process survives it.
	syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("listen waited on a request that never finishes")
	}
}
