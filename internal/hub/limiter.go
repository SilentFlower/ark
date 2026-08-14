package hub

import (
	"sync"
	"time"
)

const (
	loginFailureLimit      = 5
	loginWindow            = 5 * time.Minute
	maxLoginLimiterEntries = 4096
)

type loginWindowState struct {
	startedAt time.Time
	failures  int
	inFlight  int
}

type loginLimiter struct {
	mutex   sync.Mutex
	windows map[string]loginWindowState
	now     func() time.Time
}

type loginAttempt struct {
	limiter   *loginLimiter
	key       string
	startedAt time.Time
	completed bool
}

func newLoginLimiter(now func() time.Time) *loginLimiter {
	return &loginLimiter{
		windows: make(map[string]loginWindowState),
		now:     now,
	}
}

func (limiter *loginLimiter) begin(key string) (*loginAttempt, time.Duration, bool) {
	now := limiter.now().UTC()
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	limiter.removeExpiredLocked(now)

	state, exists := limiter.windows[key]
	if !exists {
		if len(limiter.windows) >= maxLoginLimiterEntries {
			return nil, loginWindow, false
		}
		state = loginWindowState{startedAt: now}
	}
	if state.failures+state.inFlight >= loginFailureLimit {
		return nil, state.startedAt.Add(loginWindow).Sub(now), false
	}
	state.inFlight++
	limiter.windows[key] = state
	return &loginAttempt{limiter: limiter, key: key, startedAt: state.startedAt}, 0, true
}

func (attempt *loginAttempt) failure() {
	attempt.finish(true, false)
}

func (attempt *loginAttempt) success() {
	attempt.finish(false, true)
}

func (attempt *loginAttempt) cancel() {
	attempt.finish(false, false)
}

func (attempt *loginAttempt) finish(failed, clear bool) {
	limiter := attempt.limiter
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	if attempt.completed {
		return
	}
	attempt.completed = true
	state, exists := limiter.windows[attempt.key]
	if !exists || !state.startedAt.Equal(attempt.startedAt) {
		return
	}
	if state.inFlight > 0 {
		state.inFlight--
	}
	if clear {
		delete(limiter.windows, attempt.key)
		return
	}
	if failed {
		state.failures++
	}
	if state.failures == 0 && state.inFlight == 0 {
		delete(limiter.windows, attempt.key)
		return
	}
	limiter.windows[attempt.key] = state
}

func (limiter *loginLimiter) removeExpiredLocked(now time.Time) {
	for key, state := range limiter.windows {
		if !now.Before(state.startedAt.Add(loginWindow)) {
			delete(limiter.windows, key)
		}
	}
}
