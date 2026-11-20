package clock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestScaledAdvanceFiresTimer(t *testing.T) {
	c := NewScaled(1)
	start := c.Now()
	tm := c.NewTimer(time.Hour)
	c.Advance(time.Hour)
	select {
	case <-tm.C():
	case <-time.After(2 * time.Second):
		t.Fatal("timer did not fire after Advance")
	}
	if c.Now().Sub(start) < time.Hour {
		t.Fatal("Now did not move")
	}
}

func TestScaledRunsFaster(t *testing.T) {
	c := NewScaled(1000)
	tm := c.NewTimer(10 * time.Second)
	select {
	case <-tm.C():
	case <-time.After(2 * time.Second):
		t.Fatal("scaled timer too slow")
	}
}

func TestStopAndTimeout(t *testing.T) {
	c := NewScaled(1)
	tm := c.NewTimer(time.Minute)
	if !tm.Stop() {
		t.Fatal("Stop reported fired")
	}
	c.Advance(2 * time.Minute)
	select {
	case <-tm.C():
		t.Fatal("stopped timer fired")
	case <-time.After(20 * time.Millisecond):
	}
	cause := errors.New("deadline")
	ctx, cancel := WithTimeout(context.Background(), c, time.Minute, cause)
	defer cancel()
	c.Advance(time.Minute)
	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), cause) {
		t.Fatalf("cause = %v", context.Cause(ctx))
	}
	if err := Sleep(ctx, c, time.Second); err == nil {
		t.Fatal("Sleep on a done context returned nil")
	}
}
