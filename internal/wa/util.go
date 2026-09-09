package wa

import (
	"encoding/base64"
	"strings"
	"sync"
	"time"
)

// NormalizePhone strips everything but digits (leading + tolerated).
func NormalizePhone(phone string) string {
	var b strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// tokenBucket is a per-process send limiter (30/min default).
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	rate     float64
	lastTime time.Time
}

func newTokenBucket(perMinute float64) *tokenBucket {
	return &tokenBucket{tokens: perMinute, max: perMinute, rate: perMinute / 60.0, lastTime: time.Now()}
}

func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	tb.tokens += now.Sub(tb.lastTime).Seconds() * tb.rate
	if tb.tokens > tb.max {
		tb.tokens = tb.max
	}
	tb.lastTime = now
	if tb.tokens < 1 {
		return false
	}
	tb.tokens--
	return true
}
