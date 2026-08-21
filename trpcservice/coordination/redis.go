package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const completedValue = "completed"

var compareDelete = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

var compareRenew = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

var compareComplete = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[3])
  return 1
end
return 0
`)

var compareSaveResult = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[3])
  return 1
end
return 0
`)

type Redis struct {
	client *redis.Client
	prefix string
}

func NewRedis(rawURL, prefix string) (*Redis, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("invalid coordination redis URL")
	}
	client := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, errors.New("coordination redis is unavailable")
	}
	return &Redis{client: client, prefix: prefix}, nil
}

func (r *Redis) Claim(ctx context.Context, key string, ttl time.Duration) (ClaimLease, error) {
	redisKey := r.key("dedup", key)
	owner := newOwnerToken()
	processingValue := "processing:" + owner
	ok, err := r.client.SetNX(ctx, redisKey, processingValue, ttl).Result()
	if err != nil {
		return ClaimLease{}, fmt.Errorf("claim message: %w", err)
	}
	if ok {
		return ClaimLease{State: Claimed, Token: owner}, nil
	}
	value, err := r.client.Get(ctx, redisKey).Result()
	if err != nil {
		return ClaimLease{}, fmt.Errorf("read message claim: %w", err)
	}
	if value == completedValue {
		return ClaimLease{State: AlreadyCompleted}, nil
	}
	return ClaimLease{State: AlreadyProcessing}, nil
}

func (r *Redis) Complete(ctx context.Context, key, ownerToken string, ttl time.Duration) error {
	result, err := compareComplete.Run(
		ctx,
		r.client,
		[]string{r.key("dedup", key)},
		"processing:"+ownerToken,
		completedValue,
		ttl.Milliseconds(),
	).Int64()
	if err != nil {
		return fmt.Errorf("complete message claim: %w", err)
	}
	if result != 1 {
		return ErrClaimOwnershipLost
	}
	return nil
}

func (r *Redis) ReleaseClaim(ctx context.Context, key, ownerToken string) error {
	_, err := compareDelete.Run(ctx, r.client, []string{r.key("dedup", key)}, "processing:"+ownerToken).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("release message claim: %w", err)
	}
	return nil
}

func (r *Redis) SaveResult(ctx context.Context, key, ownerToken string, value any, ttl time.Duration) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode pending result: %w", err)
	}
	result, err := compareSaveResult.Run(
		ctx,
		r.client,
		[]string{r.key("dedup", key), r.key("result", key)},
		"processing:"+ownerToken,
		encoded,
		ttl.Milliseconds(),
	).Int64()
	if err != nil {
		return fmt.Errorf("save pending result: %w", err)
	}
	if result != 1 {
		return ErrClaimOwnershipLost
	}
	return nil
}

func (r *Redis) LoadResult(ctx context.Context, key string, value any) (bool, error) {
	encoded, err := r.client.Get(ctx, r.key("result", key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load pending result: %w", err)
	}
	if err := json.Unmarshal(encoded, value); err != nil {
		return false, fmt.Errorf("decode pending result: %w", err)
	}
	return true, nil
}

func (r *Redis) DeleteResult(ctx context.Context, key string) error {
	if err := r.client.Del(ctx, r.key("result", key)).Err(); err != nil {
		return fmt.Errorf("delete pending result: %w", err)
	}
	return nil
}

func (r *Redis) Lock(ctx context.Context, key string, ttl time.Duration) (*LockLease, error) {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	redisKey := r.key("lock", key)
	token := newOwnerToken()
	for {
		ok, err := r.client.SetNX(ctx, redisKey, token, ttl).Result()
		if err != nil {
			return nil, fmt.Errorf("acquire redis lock: %w", err)
		}
		if ok {
			break
		}
		jitter := time.Duration(30+rand.Intn(40)) * time.Millisecond //nolint:gosec
		timer := time.NewTimer(jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("acquire redis lock: %w", ctx.Err())
		case <-timer.C:
		}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	lost := make(chan error, 1)
	go func() {
		defer close(done)
		renewEvery := ttl / 3
		if renewEvery <= 0 {
			renewEvery = time.Nanosecond
		}
		ticker := time.NewTicker(renewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(context.Background(), ttl/4)
				renewed, err := compareRenew.Run(renewCtx, r.client, []string{redisKey}, token, ttl.Milliseconds()).Int64()
				cancel()
				if err != nil {
					lost <- fmt.Errorf("%w: renew redis lock: %v", ErrLockOwnershipLost, err)
					return
				}
				if renewed != 1 {
					lost <- ErrLockOwnershipLost
					return
				}
			}
		}
	}()
	var once sync.Once
	release := func() {
		once.Do(func() {
			close(stop)
			<-done
			releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = compareDelete.Run(releaseCtx, r.client, []string{redisKey}, token).Result()
		})
	}
	return &LockLease{Release: release, Lost: lost}, nil
}

func (r *Redis) key(kind, key string) string {
	return r.prefix + ":" + kind + ":" + key
}

func (r *Redis) Close() error { return r.client.Close() }
