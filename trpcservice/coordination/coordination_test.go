package coordination

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestInMemoryClaimLifecycle(t *testing.T) {
	c := NewInMemory()
	ctx := context.Background()
	lease, err := c.Claim(ctx, "m1", time.Minute)
	if err != nil || lease.State != Claimed || lease.Token == "" {
		t.Fatalf("first claim = %+v, %v", lease, err)
	}
	second, _ := c.Claim(ctx, "m1", time.Minute)
	if second.State != AlreadyProcessing || second.Token != "" {
		t.Fatalf("second claim = %+v", second)
	}
	if err := c.Complete(ctx, "m1", lease.Token, time.Minute); err != nil {
		t.Fatal(err)
	}
	completed, _ := c.Claim(ctx, "m1", time.Minute)
	if completed.State != AlreadyCompleted {
		t.Fatalf("completed claim = %+v", completed)
	}
}

func TestInMemoryStaleOwnerCannotMutateNewClaim(t *testing.T) {
	c := NewInMemory()
	ctx := context.Background()
	old, err := c.Claim(ctx, "m1", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	current, err := c.Claim(ctx, "m1", time.Minute)
	if err != nil || current.State != Claimed {
		t.Fatalf("replacement claim = %+v, %v", current, err)
	}
	if err := c.ReleaseClaim(ctx, "m1", old.Token); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(ctx, "m1", old.Token, time.Minute); !errors.Is(err, ErrClaimOwnershipLost) {
		t.Fatalf("stale complete error = %v", err)
	}
	if err := c.SaveResult(ctx, "m1", old.Token, map[string]string{"owner": "old"}, time.Minute); !errors.Is(err, ErrClaimOwnershipLost) {
		t.Fatalf("stale result write error = %v", err)
	}
	seen, err := c.Claim(ctx, "m1", time.Minute)
	if err != nil || seen.State != AlreadyProcessing {
		t.Fatalf("stale owner changed current claim: %+v, %v", seen, err)
	}
}

func TestRedisStaleOwnerCannotCompleteOrReleaseNewClaim(t *testing.T) {
	server := miniredis.RunT(t)
	c, err := NewRedis("redis://"+server.Addr()+"/0", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	old, err := c.Claim(ctx, "m1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	server.FastForward(2 * time.Second)
	current, err := c.Claim(ctx, "m1", time.Minute)
	if err != nil || current.State != Claimed {
		t.Fatalf("replacement claim = %+v, %v", current, err)
	}
	if err := c.ReleaseClaim(ctx, "m1", old.Token); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(ctx, "m1", old.Token, time.Minute); !errors.Is(err, ErrClaimOwnershipLost) {
		t.Fatalf("stale complete error = %v", err)
	}
	if err := c.SaveResult(ctx, "m1", old.Token, map[string]string{"owner": "old"}, time.Minute); !errors.Is(err, ErrClaimOwnershipLost) {
		t.Fatalf("stale result write error = %v", err)
	}
	seen, err := c.Claim(ctx, "m1", time.Minute)
	if err != nil || seen.State != AlreadyProcessing {
		t.Fatalf("stale owner changed current claim: %+v, %v", seen, err)
	}
	if err := c.Complete(ctx, "m1", current.Token, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestNewRedisDoesNotLeakCredentialsInParseError(t *testing.T) {
	const credential = "canary-password"
	_, err := NewRedis("redis://user:"+credential+"@localhost/%zz", "test")
	if err == nil {
		t.Fatal("expected malformed redis URL to fail")
	}
	if strings.Contains(err.Error(), credential) {
		t.Fatalf("redis parse error leaked credential: %v", err)
	}
}

func TestInMemoryLockSerializesSameSession(t *testing.T) {
	c := NewInMemory()
	ctx := context.Background()
	var mu sync.Mutex
	active, maxActive := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := c.Lock(ctx, "session", time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			defer lease.Release()
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("same-session critical sections overlapped: max=%d", maxActive)
	}
}
