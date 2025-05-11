package service

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// ConnectionTracker watches changes in connection state and sets/resets a timer when a connection becomes idle. This service
// can be used to trigger a shutdown when all net.Conn connections become idle.
//
// base on https://stackoverflow.com/questions/56487495/how-can-i-make-gos-http-server-exit-after-being-idle-for-a-period-of-time
type ConnectionTracker struct {
	mu     sync.Mutex
	active map[net.Conn]bool
	idle   time.Duration
	timer  *time.Timer
}

func NewConnectionTracker(idle time.Duration) *ConnectionTracker {
	return &ConnectionTracker{
		active: make(map[net.Conn]bool),
		idle:   idle,
		timer:  time.NewTimer(idle),
	}
}

func (t *ConnectionTracker) ConnState(conn net.Conn, state http.ConnState) {
	t.mu.Lock()
	defer t.mu.Unlock()

	oldActive := len(t.active)

	switch state {
	case http.StateNew, http.StateActive, http.StateHijacked:
		t.active[conn] = true
		// stop the timer if we transitioned to idle
		if oldActive == 0 {
			t.timer.Stop()
		}
	case http.StateIdle, http.StateClosed:
		delete(t.active, conn)
		// Restart the timer if we've become idle
		if oldActive > 0 && len(t.active) == 0 {
			t.timer.Reset(t.idle)
		}
	}
}

func (t *ConnectionTracker) Done() <-chan time.Time {
	return t.timer.C
}
