package tokenbucket_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/slidingwindowlog"
	"github.com/sorenhoang/go-ratelimiter/internal/limiter/tokenbucket"
)

// TestBurst_TokenBucketAdmitsWhatTheLogRefuses pins down the one thing this
// algorithm does that none of the earlier ones can.
//
// Both limiters below sustain the same rate: one request per second. The token
// bucket reaches it by handing out a capacity of ten up front and refilling one
// a second; the log reaches it by allowing one request in any one second window.
// Over a minute they let through about the same amount of traffic. They differ
// entirely on what happens when a caller arrives with ten requests at once.
//
// That is the choice this phase adds: whether a client who has been quiet may
// spend the quiet time, or has to trickle regardless.
func TestBurst_TokenBucketAdmitsWhatTheLogRefuses(t *testing.T) {
	const burst = 10

	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetTime(baseTime)

	rdb := redis.NewClient(&redis.Options{Addr: s.Addr(), MaxRetries: -1})
	ctx := context.Background()

	bucket, err := tokenbucket.New(rdb, tokenbucket.Config{
		Capacity: burst, RefillPerSecond: 1,
	})
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	trickle, err := slidingwindowlog.New(rdb, slidingwindowlog.Config{
		Limit: 1, Window: time.Second,
	})
	if err != nil {
		t.Fatalf("log: %v", err)
	}

	var bucketAdmitted, logAdmitted int
	for i := 0; i < burst; i++ {
		b, err := bucket.AllowN(ctx, "user", 1)
		if err != nil {
			t.Fatalf("bucket, request %d: %v", i, err)
		}
		l, err := trickle.AllowN(ctx, "user", 1)
		if err != nil {
			t.Fatalf("log, request %d: %v", i, err)
		}
		if b.Allowed {
			bucketAdmitted++
		}
		if l.Allowed {
			logAdmitted++
		}
	}

	t.Logf("a burst of %d at a sustained one per second: bucket admitted %d, log admitted %d",
		burst, bucketAdmitted, logAdmitted)

	if bucketAdmitted != burst {
		t.Errorf("bucket admitted %d of %d, want all of them", bucketAdmitted, burst)
	}
	if logAdmitted != 1 {
		t.Errorf("log admitted %d of %d, want exactly 1", logAdmitted, burst)
	}

	// And having spent the burst, the bucket drops to the sustained rate rather
	// than staying open.
	if d, err := bucket.AllowN(ctx, "user", 1); err != nil || d.Allowed {
		t.Errorf("bucket still open after spending the burst: %+v err=%v", d, err)
	}
}
