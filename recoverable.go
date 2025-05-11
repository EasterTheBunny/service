package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrServicePanic          = errors.New("service paniced")
	ErrAllServicesTerminated = errors.New("all services were terminated")
	ErrCannotAdd             = errors.New("cannot add service")
)

const (
	closeQueueLimit = 50
	recoverWaitTime = 10 * time.Second
)

type Runnable interface {
	RunWithContext(context.Context) error
}

type serviceTerm struct {
	ID  int
	Err error
}

func NewRecoverableServiceManager(logger *log.Logger) *RecoverableServiceManager {
	return &RecoverableServiceManager{
		closed:   make(chan serviceTerm, closeQueueLimit),
		services: make(map[int]Runnable),
		log:      logger,
		chClose:  make(chan struct{}, 1),
	}
}

type RecoverableServiceManager struct {
	mu        sync.Mutex
	closed    chan serviceTerm
	services  map[int]Runnable
	serviceID int
	active    int
	log       *log.Logger

	// service life-cycle properties
	chClose chan struct{}
	running atomic.Bool
	closing atomic.Bool
}

func (m *RecoverableServiceManager) Add(svc Runnable) error {
	if m.running.Load() {
		return fmt.Errorf("%w: service already running", ErrCannotAdd)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	id := m.serviceID
	m.serviceID++
	m.services[id] = svc
	m.active++

	return nil
}

func (m *RecoverableServiceManager) Start(ctx context.Context) error {
	m.running.Store(true)

	ctx, cancel := context.WithCancel(ctx)

	for id := range m.services {
		m.runService(ctx, id, time.Duration(0))
	}

	for {
		if m.active == 0 {
			cancel()

			return ErrAllServicesTerminated
		}

		select {
		case svc := <-m.closed:
			if svc.Err != nil && errors.Is(svc.Err, ErrServicePanic) {
				// restart the service as a recover
				m.runService(ctx, svc.ID, recoverWaitTime)
			} else {
				if !m.closing.Load() {
					m.closing.Store(true)
					closeService(m.chClose)
				}

				m.mu.Lock()
				delete(m.services, svc.ID)

				m.active--
				m.mu.Unlock()
			}
		case <-m.chClose:
			cancel()

			if !m.closing.Load() {
				m.closing.Store(true)
				closeService(m.chClose)
			}

			continue
		case <-ctx.Done():
			if !m.closing.Load() {
				m.closing.Store(true)
				closeService(m.chClose)
			}

			continue
		}
	}
}

func (m *RecoverableServiceManager) runService(ctx context.Context, serviceID int, after time.Duration) {
	svc := m.services[serviceID]

	go func(idx int, runner Runnable, logger *log.Logger, trm chan serviceTerm, ctx context.Context, aft time.Duration) {
		defer func() {
			if err := recover(); err != nil {
				// print the stack trace
				logger.Println(err)
				logger.Println(string(debug.Stack()))

				// indicate that service terminated as a panic
				trm <- serviceTerm{
					ID:  idx,
					Err: ErrServicePanic,
				}
			}
		}()

		// exit if the context cancels; wait otherwise
		select {
		case <-ctx.Done():
			return
		case <-time.After(aft):
		}

		err := runner.RunWithContext(ctx)

		logger.Println(err)

		trm <- serviceTerm{
			ID:  idx,
			Err: err,
		}
	}(serviceID, svc, m.log, m.closed, ctx, after)
}

func closeService(chClose chan struct{}) {
	select {
	case chClose <- struct{}{}:
	default:
	}
}
