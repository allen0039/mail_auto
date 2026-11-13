// Package tglimit coordinates Telegram send rates between task notifications
// and interactive replies, so one cannot push the other into rate limiting.
// Both switches and queues stay independent; only pacing is shared.
package tglimit

import (
	"context"
	"sync"
	"time"
)

var (
	mu        sync.Mutex
	lastChat  = map[string]time.Time{}
	lastAny   time.Time
	blockedTo time.Time
)

// PerChat and Global are the minimum spacing between sends.
var (
	PerChat = time.Second
	Global  = 50 * time.Millisecond
)

// Wait blocks until a message to chat may be sent, honoring per-chat and
// global spacing and any recorded 429 block, or until ctx ends. Interactive
// replies use it.
func Wait(ctx context.Context, chat string) error { return wait(ctx, chat, true) }

// Pace applies only per-chat and global spacing. Task notifications use it:
// they already reschedule themselves from their own 429 retry_after, and
// they publish that block to interactive replies through Block.
func Pace(ctx context.Context, chat string) error { return wait(ctx, chat, false) }

func wait(ctx context.Context, chat string, honorBlock bool) error {
	for {
		mu.Lock()
		now := time.Now()
		next := lastChat[chat].Add(PerChat)
		if g := lastAny.Add(Global); g.After(next) {
			next = g
		}
		if honorBlock && blockedTo.After(next) {
			next = blockedTo
		}
		if !next.After(now) {
			lastChat[chat] = now
			lastAny = now
			if len(lastChat) > 1024 {
				for k, v := range lastChat {
					if now.Sub(v) > time.Minute {
						delete(lastChat, k)
					}
				}
			}
			mu.Unlock()
			return nil
		}
		mu.Unlock()
		t := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Block records a 429 retry_after for the bot.
func Block(d time.Duration) {
	if d <= 0 {
		return
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	mu.Lock()
	defer mu.Unlock()
	if until := time.Now().Add(d); until.After(blockedTo) {
		blockedTo = until
	}
}
