package tglimit

import (
	"context"
	"testing"
	"time"
)

func TestBlockAppliesToRepliesAndPacingToBoth(t *testing.T) {
	PerChat, Global = 50*time.Millisecond, 0
	defer func() { PerChat, Global = time.Second, 50*time.Millisecond }()
	Block(300 * time.Millisecond)
	start := time.Now()
	if err := Pace(context.Background(), "a"); err != nil || time.Since(start) > 100*time.Millisecond {
		t.Fatal("notification pacing waited for the 429 block", time.Since(start))
	}
	start = time.Now()
	if err := Wait(context.Background(), "b"); err != nil || time.Since(start) < 200*time.Millisecond {
		t.Fatal("interactive reply ignored the 429 block", time.Since(start))
	}
	start = time.Now()
	_ = Pace(context.Background(), "c")
	_ = Pace(context.Background(), "c")
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("per-chat spacing not applied")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	Block(time.Second)
	if Wait(ctx, "d") == nil {
		t.Fatal("cancelled wait returned nil")
	}
}
