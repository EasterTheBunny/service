package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"maps"
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

func RecoverOnErrorExcept(errs ...error) func(*RecoverableServiceManager) {
	return func(m *RecoverableServiceManager) {
		m.recoverOnError = true
		m.recoverOnErrorOpts = errs
	}
}

type RecoveryStrategy interface {
	Wait() (time.Duration, bool)
}

type ExponentialRecoveryStrategy struct {
	mu       sync.Mutex
	factor   int64
	nextWait time.Duration
	max      time.Duration
}

func NewExponentialRecoveryStrategy(initial time.Duration) *ExponentialRecoveryStrategy {
	return &ExponentialRecoveryStrategy{
		factor:   2,
		nextWait: time.Second,
		max:      60 * time.Second,
	}
}

func (s *ExponentialRecoveryStrategy) Wait() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.nextWait
	if next > s.max {
		return 0, false
	}

	s.nextWait = s.nextWait * 2

	return next, true
}

func WithRecoveryStrategy(strategy RecoveryStrategy) func(*RecoverableServiceManager) {
	return func(m *RecoverableServiceManager) {
		m.recovery = strategy
	}
}

func NewRecoverableServiceManager(opts ...RecoverableServiceManagerOpt) *RecoverableServiceManager {
	manager := &RecoverableServiceManager{
		recoverOnErrorOpts: make([]error, 0),
		notStarted:         make(map[int]Runnable),
		running:            make(map[int]Runnable),
		exited:             make(chan serviceTerm, closeQueueLimit),
		chClose:            make(chan struct{}),
	}

	for _, opt := range opts {
		opt(manager)
	}

	if manager.recovery == nil {
		manager.recovery = NewExponentialRecoveryStrategy(time.Second)
	}

	return manager
}

type RecoverableServiceManager struct {
	// provided options
	log                *slog.Logger
	recoverOnError     bool
	recoverOnErrorOpts []error
	recovery           RecoveryStrategy

	// internal state
	mu         sync.Mutex
	notStarted map[int]Runnable
	running    map[int]Runnable
	active     atomic.Int32
	serviceID  int
	exited     chan serviceTerm

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
	// 1. lock services from being started
	// 2. lock services from being added
	// 3. set started and starting states to false
	// 4. call Close on all running services
	// 5. set closing state to false
	// 6. return ErrAllServicesTerminated joined with all service close errors

	m.closing.Store(true)
	closeService(m.chClose)

	err := ErrAllServicesTerminated

	m.mu.Lock()
	for _, svc := range m.running {
		err = errors.Join(err, svc.Close())

		m.active.Add(-1)
	}
	m.mu.Unlock()

	m.closing.Store(false)
	m.started.Store(false)
	m.starting.Store(false)

	return err
}

// Start starts the service manager and all managed services. Start will always return an error. This function will
// block until Close or Shutdown are called. By default, Start will terminate all services if one returns an error.
func (m *RecoverableServiceManager) Start() error {
	// 1. return error for conflicting states
	//   a. already started ErrServiceRunning
	//   b. already starting ErrServiceStarting
	//   c. closing ErrServiceClosing
	if m.started.Load() || m.starting.Load() || m.closing.Load() {
		return ErrAllServicesTerminated
	}

	// 2. set state to starting
	m.starting.Store(true)

	// 3. start all registered services
	m.mu.Lock()
	for id := range maps.Keys(m.notStarted) {
		go m.startService(id, time.Duration(0)) // immediately start all services
	}
	m.mu.Unlock()

	// 4. set state to started
	m.started.Store(true)
	m.starting.Store(false)

	return m.run()
}

func (m *RecoverableServiceManager) run() error {
	for {
		if m.closing.Load() {
			if m.active.Load() <= 0 {
				return ErrAllServicesTerminated
			}

			continue
		}

		select {
		// every time a service exits, restart it
		case svc := <-m.exited:
			if svc.Err != nil && (errors.Is(svc.Err, errServicePanic) || m.recoverOnError) {
				if !errors.Is(svc.Err, errServicePanic) && m.recoverOnError {
					// check for errors that can be skipped
					for _, err := range m.recoverOnErrorOpts {
						if errors.Is(svc.Err, err) {
							// should stop the service and return an error
							m.closing.Store(true)
							closeService(m.chClose)

							continue
						}
					}
				}

				tm, ok := m.recovery.Wait()
				if !ok && !m.closing.Load() {
					// should stop the service and return an error
					m.closing.Store(true)
					closeService(m.chClose)
				}

				if !m.closing.Load() {
					// restart the service as a recover
					go m.startService(svc.ID, tm)
				}

				continue
			}

			// if the exit reason is not a panic and recoverOnError is false
			// start closing services
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

			if !m.closing.Load() {
				// indicate that the service terminated as a panic.
				m.exited <- serviceTerm{
					ID:  serviceID,
					Err: errServicePanic,
				}
			}
		}
	}()

	svc := m.setActive(serviceID)
	defer m.setInactive(serviceID)

	// exit if the service manager has closed; wait otherwise
	select {
	case <-m.chClose:
		return
	case <-time.After(after):
	}

	// blocks until error
	err := svc.Start()

	if !m.closing.Load() {
		if m.log != nil && err != nil {
			m.log.Error(err.Error())
		}

		m.exited <- serviceTerm{
			ID:  serviceID,
			Err: err,
		}
	}
}

func (m *RecoverableServiceManager) setActive(serviceID int) Runnable {
	m.mu.Lock()
	defer m.mu.Unlock()

	svc, exists := m.notStarted[serviceID]
	if !exists {
		return nil
	}

	m.running[serviceID] = svc
	delete(m.notStarted, serviceID)

	return svc
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
