package service_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/easterthebunny/service"
)

func TestConnectionTracker(t *testing.T) {
	testTimeout := 1 * time.Second
	tracker := service.NewConnectionTracker(testTimeout)
	srv := httptest.NewUnstartedServer(testHandler())

	srv.Config.SetKeepAlivesEnabled(true)
	srv.Config.ConnState = tracker.ConnState
	srv.Config.IdleTimeout = 100 * time.Millisecond
	srv.Start()

	defer srv.Close()

	dialer := &net.Dialer{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     500 * time.Millisecond,
			Interval: 500 * time.Millisecond,
		},
	}

	client := &http.Client{
		Transport: &http.Transport{
			Dial:        dialer.Dial,
			DialContext: dialer.DialContext,
		},
	}

	for range 10 {
		_, err := client.Get(srv.URL)

		require.NoError(t, err)
	}

	select {
	case <-t.Context().Done():
		t.FailNow()
	case <-tracker.Done():
		// passes if all connections are closed after idle
		require.NoError(t, srv.Config.Shutdown(t.Context()))
	}
}

func testHandler() http.HandlerFunc {
	return func(writer http.ResponseWriter, r *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}
}
