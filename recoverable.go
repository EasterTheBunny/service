package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrAllServicesTerminated = errors.New("all services were terminated")
)

type Runnable interface {
	// Start should start a service and return an error if startup failed.
	Start() error
	// Shutdown should gracefully close the running service and always return an error.
	Shutdown(context.Context) error
	// Close should immediately close the running service and always return an error.
	Close() error
}

type RecoverableServiceManagerOpt func(*RecoverableServiceManager)

func WithLogger(logger *slog.Logger) func(*RecoverableServiceManager) {
	return func(m *RecoverableServiceManager) {
		m.log = logger
	}
}

func RecoverOnError(m *RecoverableServiceManager) {
	m.recoverOnError = true
}

func WithRecoverWait(wait time.Duration) func(*RecoverableServiceManager) {
	return func(m *RecoverableServiceManager) {
		m.recoverWaitTime = wait
	}
}

func NewRecoverableServiceManager(opts ...RecoverableServiceManagerOpt) *RecoverableServiceManager {
	manager := &RecoverableServiceManager{
		recoverWaitTime: defaultRecoverWaitTime,
		notStarted:      make(map[int]Runnable),
		running:         make(map[int]Runnable),
		closed:          make(chan serviceTerm, closeQueueLimit),
		chClose:         make(chan struct{}),
	}

	for _, opt := range opts {
		opt(manager)
	}

	return manager
}

type RecoverableServiceManager struct {
	// provided options
	log             *slog.Logger
	recoverOnError  bool
	recoverWaitTime time.Duration

	// internal state
	mu         sync.Mutex
	notStarted map[int]Runnable
	running    map[int]Runnable
	active     int
	serviceID  int
	closed     chan serviceTerm

	// service life-cycle properties
	chClose  chan struct{}
	starting atomic.Bool
	started  atomic.Bool
	closing  atomic.Bool
}

// Add will add a Runnable service to the service manager ONLY if the service manager has not yet started.
func (m *RecoverableServiceManager) Add(svc Runnable) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := m.serviceID
	m.serviceID++
	m.notStarted[id] = svc

	return nil
}

func (m *RecoverableServiceManager) Close() error {
	m.closing.Store(true)
	closeService(m.chClose)

	m.closing.Store(false)

	return ErrAllServicesTerminated
}

// Start starts the service manager and all managed services. Start will always return an error. This function will
// block until Close or Shutdown are called. By default, Start will terminate all services if one returns an error.
func (m *RecoverableServiceManager) Start() error {
	if m.started.Load() || m.starting.Load() || m.closing.Load() {
		return nil
	}

	m.starting.Store(true)

	for id := range m.notStarted {
		go m.startService(id, time.Duration(0)) // immediately start all services
	}

	m.started.Store(true)
	m.starting.Store(false)

	return m.run()
}

func (m *RecoverableServiceManager) run() error {
	for {
		if m.closing.Load() && m.active == 0 {
			return ErrAllServicesTerminated
		}

		select {
		case svc := <-m.closed:
			if svc.Err != nil {
				if errors.Is(svc.Err, errServicePanic) || m.recoverOnError {
					// restart the service as a recover
					go m.startService(svc.ID, m.recoverWaitTime)

					continue
				}
			}

			if !m.closing.Load() {
				m.closing.Store(true)
				closeService(m.chClose)
			}
		case <-m.chClose:
			if !m.closing.Load() {
				m.closing.Store(true)
				closeService(m.chClose)
			}

			continue
		}
	}
}

func (m *RecoverableServiceManager) startService(serviceID int, after time.Duration) {
	m.mu.Lock()
	svc := m.notStarted[serviceID]
	m.mu.Unlock()

	defer func() {
		if err := recover(); err != nil {
			if m.log != nil {
				log.Println(err)
				// log the error for better clarity on why the service errored.
				m.log.Error(fmt.Sprintf("%s", err))
				// print the stack trace for debugging purposes.
				m.log.Debug(string(debug.Stack()))
			}

			m.setInactive(serviceID)

			// indicate that the service terminated as a panic.
			m.closed <- serviceTerm{
				ID:  serviceID,
				Err: errServicePanic,
			}
		}
	}()

	m.setActive(serviceID)
	defer m.setInactive(serviceID)

	// exit if the service manager has closed; wait otherwise
	select {
	case <-m.chClose:
		return
	case <-time.After(after):
	}

	err := svc.Start()

	if m.log != nil && err != nil {
		m.log.Error(err.Error())
	}

	m.closed <- serviceTerm{
		ID:  serviceID,
		Err: err,
	}
}

func (m *RecoverableServiceManager) setActive(serviceID int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	svc, exists := m.notStarted[serviceID]
	if !exists {
		return
	}

	m.running[serviceID] = svc
	delete(m.notStarted, serviceID)
	m.active++
}

func (m *RecoverableServiceManager) setInactive(serviceID int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	svc, exists := m.running[serviceID]
	if !exists {
		return
	}

	m.notStarted[serviceID] = svc
	delete(m.running, serviceID)
	m.active--
}

func closeService(chClose chan struct{}) {
	select {
	case chClose <- struct{}{}:
	default:
	}
}

type serviceTerm struct {
	ID  int
	Err error
}

const (
	closeQueueLimit        = 1
	defaultRecoverWaitTime = 10 * time.Second
)

var errServicePanic = errors.New("service paniced")
