package daemon

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDaemonStartStop(t *testing.T) {
	errCh := make(chan error)
	defer func() {
		select {
		case err := <-errCh:
			t.Fatal(err)
		default:
		}
	}()
	if len(os.Args) == 3 && os.Args[1] == "serve" {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		for range sigCh {
			_ = os.WriteFile("test", []byte("aaa"), os.ModePerm)
			os.Exit(0)
		}
		return
	}
	pid, err := Start([]string{"serve", time.Now().Format("20060102150405")})
	assert.NoError(t, err)
	assert.NotEqual(t, 0, pid)
	doneCh := make(chan struct{})
	timer := time.NewTimer(1 * time.Second)
	go func() {
		<-timer.C
		errCh <- errors.New("daemon process did not start in time")
	}()
	go func() {
		Wait(pid)
		timer.Stop()
		doneCh <- struct{}{}
	}()
	_, err = os.FindProcess(pid)
	assert.NoError(t, err)
	err = Stop(pid)
	assert.NoError(t, err)
	Wait(pid)
	<-doneCh
}

func TestDaemonStartKill(t *testing.T) {
	errCh := make(chan error)
	defer func() {
		select {
		case err := <-errCh:
			t.Fatal(err)
		default:
		}
	}()
	pid, err := Start([]string{"serve"})
	assert.NoError(t, err)
	assert.NotEqual(t, 0, pid)
	doneCh := make(chan struct{})
	timer := time.NewTimer(1 * time.Second)
	go func() {
		<-timer.C
		errCh <- errors.New("daemon process did not start in time")
	}()
	go func() {
		Wait(pid)
		timer.Stop()
		doneCh <- struct{}{}
	}()
	_, err = os.FindProcess(pid)
	assert.NoError(t, err)
	err = Kill(pid)
	assert.NoError(t, err)
	Wait(pid)
	<-doneCh
}
