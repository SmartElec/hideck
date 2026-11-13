package modemvoice

import (
	"context"
	"errors"
)

// Watch observes one generation. Observers may read/control calls, but must
// return rather than calling Close synchronously: Close joins this observer.
// The owner publishes Updates returned by controls alongside these changes.
func (s *Session) Watch(ctx context.Context, options WatchOptions) error {
	if ctx == nil {
		return errors.New("modem voice: context is required")
	}
	s.watchMu.Lock()
	if s.watchDone != nil || s.ctx.Err() != nil {
		s.watchMu.Unlock()
		return errors.New("modem voice: session watcher already started or closed")
	}
	s.watchDone = make(chan struct{})
	done := s.watchDone
	s.watchMu.Unlock()
	defer close(done)
	defer s.cancel()
	defer s.unsubOnce.Do(s.unsubscribe)
	watchCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	options.Wakeups = s.wakeups
	return watchSnapshots(watchCtx, options, watchSource{wake: s.wake, refresh: func(ctx context.Context) ([]Change, error) {
		update, err := s.refreshVerifiedLocked(ctx)
		return update.Changes, err
	}})
}

// refreshVerifiedLocked 与 Refresh 同一套会话闸门，只是快照多走一次 CPAS 复核。
func (s *Session) refreshVerifiedLocked(ctx context.Context) (Update, error) {
	opCtx, release, err := s.enter(ctx)
	if err != nil {
		return Update{}, err
	}
	defer release()
	return s.refreshVerified(opCtx)
}
