package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestRecoverableService_Recover(t *testing.T) {
	mr := new(MockRecoverable)
	mr.chPanic = make(chan error)

	logger := log.New(io.Discard, "", 0)
	ctx := t.Context()
	mr.On("RunWithContext", mock.Anything).Return(fmt.Errorf("done"))

	rsm := NewRecoverableServiceManager(logger)
	require.NoError(t, rsm.Add(mr))

	done := make(chan struct{})
	go func() {
		assert.ErrorIs(t, rsm.Start(ctx), ErrAllServicesTerminated)
		done <- struct{}{}
	}()

	// test that a panic doesn't result in the service ending
	mr.chPanic <- ErrMockPanic

	select {
	case <-done:
		assert.Fail(t, "service terminated when function panic encountered")
	case <-time.After(1 * time.Second):
		return
	}
}

func TestRecoverableService_EndOnError(t *testing.T) {
	mr := new(MockRecoverable)
	m1 := new(MockRecoverable)
	m2 := new(MockRecoverable)
	mr.chPanic = make(chan error, 2)

	logger := log.New(io.Discard, "", 0)
	ctx := t.Context()
	mr.On("RunWithContext", mock.Anything).Return(fmt.Errorf("done"))
	m1.On("RunWithContext", mock.Anything).Return(fmt.Errorf("done"))
	m2.On("RunWithContext", mock.Anything).Return(fmt.Errorf("done"))

	rsm := NewRecoverableServiceManager(logger)

	require.NoError(t, rsm.Add(mr))
	require.NoError(t, rsm.Add(m1))
	require.NoError(t, rsm.Add(m2))

	done := make(chan struct{})
	go func() {
		assert.ErrorIs(t, rsm.Start(ctx), ErrAllServicesTerminated)
		done <- struct{}{}
	}()

	<-time.After(500 * time.Millisecond)
	// test that a panic doesn't result in the service panicing
	mr.chPanic <- nil

	select {
	case <-done:
		return
	case <-time.After(1 * time.Second):
		assert.Fail(t, "service should have ended")
	}
}

var ErrMockPanic = fmt.Errorf("panic")

type MockRecoverable struct {
	mock.Mock
	running bool
	chPanic chan error
}

func (_m *MockRecoverable) RunWithContext(ctx context.Context) error {
	ret := _m.Mock.Called(ctx)
	_m.running = true

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-_m.chPanic:
		if err == nil {
			return ret.Error(0)
		}

		if errors.Is(err, ErrMockPanic) {
			panic(err)
		}

		return err
	}
}
