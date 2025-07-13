package service_test

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/easterthebunny/service"
	"github.com/easterthebunny/service/internal/mocks"
)

func TestRecoverableServiceManager_RecoverOnPanic(t *testing.T) {
	t.Parallel()

	mr := new(mocks.MockRunnable)

	chPanic := make(chan struct{}, 1)
	chErr := make(chan struct{}, 1)

	mr.EXPECT().Start().RunAndReturn(func() error {
		select {
		case <-chPanic:
			chErr <- struct{}{}
			panic("panic")
		case <-chErr:
			return errors.New("test")
		case <-t.Context().Done():
			return errors.New("context done")
		}
	}).Times(2)

	manager := service.NewRecoverableServiceManager(
		service.WithRecoveryStrategy(service.NewExponentialRecoveryStrategy(100 * time.Millisecond)),
	)

	require.NoError(t, manager.Add(mr))
	t.Cleanup(func() {
		_ = manager.Close()
	})

	chPanic <- struct{}{}

	require.ErrorIs(t, manager.Start(), service.ErrAllServicesTerminated)
	mr.AssertExpectations(t)
}

func TestRecoverableServiceManager_RecoverOnError(t *testing.T) {
	t.Parallel()

	mr := new(mocks.MockRunnable)

	chPanic := make(chan struct{}, 1)
	chErr := make(chan struct{}, 1)
	chClose := make(chan struct{}, 1)

	manager := service.NewRecoverableServiceManager(
		service.RecoverOnError,
		service.WithRecoveryStrategy(service.NewExponentialRecoveryStrategy(100*time.Millisecond)),
	)

	mr.EXPECT().Start().RunAndReturn(func() error {
		t.Log("run and return")
		select {
		case <-chPanic:
			chErr <- struct{}{}
			panic("panic")
		case <-chErr:
			chClose <- struct{}{}
			return errors.New("service error")
		case <-chClose:
			t.Log("closing")
			_ = manager.Close()
			return nil
		case <-t.Context().Done():
			return errors.New("context done")
		}
	}).Times(3)

	require.NoError(t, manager.Add(mr))

	chPanic <- struct{}{}

	t.Log("start")
	require.ErrorIs(t, manager.Start(), service.ErrAllServicesTerminated)
	mr.AssertExpectations(t)
}

func TestRecoverableServiceManager_ServiceErrorsLogged(t *testing.T) {
	t.Parallel()

	writer := bytes.NewBuffer([]byte{})
	mr := new(mocks.MockRunnable)
	logger := slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))

	chErr := make(chan struct{}, 1)
	chClose := make(chan struct{}, 1)

	manager := service.NewRecoverableServiceManager(
		service.RecoverOnError,
		service.WithLogger(logger),
		service.WithRecoveryStrategy(service.NewExponentialRecoveryStrategy(100*time.Millisecond)),
	)

	mr.EXPECT().Start().RunAndReturn(func() error {
		select {
		case <-chErr:
			chClose <- struct{}{}
			return errors.New("service error")
		case <-chClose:
			manager.Close()
			return errors.New("closing")
		case <-t.Context().Done():
			manager.Close()
			return errors.New("context done")
		}
	}).Times(2)

	mr.EXPECT().Close().Return(nil)

	require.NoError(t, manager.Add(mr))

	chErr <- struct{}{}

	require.ErrorIs(t, manager.Start(), service.ErrAllServicesTerminated)
	mr.AssertExpectations(t)
	assert.Contains(t, writer.String(), "service error")
}

func TestRecoverableServiceManager_Close(t *testing.T) {
	t.Parallel()

	mr := new(mocks.MockRunnable)

	chStarted := make(chan struct{})
	chRunnable := make(chan struct{})

	mr.EXPECT().Start().RunAndReturn(func() error {
		close(chStarted)

		select {
		case <-chRunnable:
			// simulate a slow close
			time.Sleep(time.Second)

			return errors.New("closed")
		case <-t.Context().Done():
			return errors.New("context done")
		}
	}).Once()

	mr.EXPECT().Close().RunAndReturn(func() error {
		close(chRunnable)

		return errors.New("closed")
	}).Once()

	manager := service.NewRecoverableServiceManager(
		service.RecoverOnError,
		service.WithRecoveryStrategy(service.NewExponentialRecoveryStrategy(100*time.Millisecond)),
	)

	require.NoError(t, manager.Add(mr))

	go func() {
		require.ErrorIs(t, manager.Start(), service.ErrAllServicesTerminated)
	}()

	<-chStarted
	time.Sleep(100 * time.Millisecond)
	require.ErrorIs(t, manager.Close(), service.ErrAllServicesTerminated)

	mr.AssertExpectations(t)
}
