package ratelimit

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	xrate "golang.org/x/time/rate"
)

type Class string

const (
	ClassRead     Class = "read"
	ClassMutation Class = "mutation"
	ClassUpload   Class = "upload"
)

type Rate struct {
	PerSecond float64
	Burst     int
}

type Options struct {
	Read              Rate
	Mutation          Rate
	Upload            Rate
	UploadConcurrency int
	IdleTTL           time.Duration
	Now               func() time.Time
}

type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
	Release    func()
}

type tokenEntry struct {
	read          *xrate.Limiter
	mutation      *xrate.Limiter
	upload        *xrate.Limiter
	activeUploads int
	lastSeen      time.Time
}

type Limiter struct {
	mu                sync.Mutex
	read              Rate
	mutation          Rate
	upload            Rate
	uploadConcurrency int
	idleTTL           time.Duration
	now               func() time.Time
	lastSweep         time.Time
	entries           map[uuid.UUID]*tokenEntry
}

func New(options Options) (*Limiter, error) {
	if err := validateRate("read", options.Read); err != nil {
		return nil, err
	}
	if err := validateRate("mutation", options.Mutation); err != nil {
		return nil, err
	}
	if err := validateRate("upload", options.Upload); err != nil {
		return nil, err
	}
	if options.UploadConcurrency <= 0 {
		return nil, fmt.Errorf("upload concurrency must be positive")
	}
	if options.IdleTTL <= 0 {
		return nil, fmt.Errorf("idle TTL must be positive")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		read: options.Read, mutation: options.Mutation, upload: options.Upload,
		uploadConcurrency: options.UploadConcurrency, idleTTL: options.IdleTTL,
		now: now, entries: make(map[uuid.UUID]*tokenEntry),
	}, nil
}

func validateRate(name string, value Rate) error {
	if value.PerSecond <= 0 {
		return fmt.Errorf("%s rate must be positive", name)
	}
	if value.Burst <= 0 {
		return fmt.Errorf("%s burst must be positive", name)
	}
	return nil
}

func (l *Limiter) Acquire(tokenID uuid.UUID, class Class) Decision {
	now := l.now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepIdle(now)
	if tokenID == uuid.Nil {
		return Decision{RetryAfter: time.Second}
	}
	entry := l.entries[tokenID]
	if entry == nil {
		entry = &tokenEntry{
			read:     newBucket(l.read),
			mutation: newBucket(l.mutation),
			upload:   newBucket(l.upload),
			lastSeen: now,
		}
		l.entries[tokenID] = entry
	}
	entry.lastSeen = now
	if class == ClassUpload && entry.activeUploads >= l.uploadConcurrency {
		return Decision{RetryAfter: time.Second}
	}
	bucket := entry.bucket(class)
	if bucket == nil {
		return Decision{RetryAfter: time.Second}
	}
	reservation := bucket.ReserveN(now, 1)
	if !reservation.OK() {
		return Decision{RetryAfter: time.Second}
	}
	delay := reservation.DelayFrom(now)
	if delay > 0 {
		reservation.CancelAt(now)
		return Decision{RetryAfter: delay}
	}
	decision := Decision{Allowed: true}
	if class == ClassUpload {
		entry.activeUploads++
		var once sync.Once
		decision.Release = func() {
			once.Do(func() {
				l.mu.Lock()
				defer l.mu.Unlock()
				if entry.activeUploads > 0 {
					entry.activeUploads--
				}
				entry.lastSeen = l.now().UTC()
			})
		}
	}
	return decision
}

func newBucket(limit Rate) *xrate.Limiter {
	return xrate.NewLimiter(xrate.Limit(limit.PerSecond), limit.Burst)
}

func (entry *tokenEntry) bucket(class Class) *xrate.Limiter {
	switch class {
	case ClassRead:
		return entry.read
	case ClassMutation:
		return entry.mutation
	case ClassUpload:
		return entry.upload
	default:
		return nil
	}
}

func (l *Limiter) sweepIdle(now time.Time) {
	interval := l.idleTTL / 2
	if interval <= 0 {
		interval = l.idleTTL
	}
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < interval {
		return
	}
	for tokenID, entry := range l.entries {
		if entry.activeUploads == 0 && now.Sub(entry.lastSeen) > l.idleTTL {
			delete(l.entries, tokenID)
		}
	}
	l.lastSweep = now
}
