package danmaku

import (
	"time"

	"bilibridge/internal/potplayer"
)

type playbackClock struct {
	started      time.Time
	lastSample   sample
	lastProgress time.Time
	origin       time.Duration
	speed        float64
}

func (c *playbackClock) position(now time.Time) time.Duration {
	if c.lastSample.status != potplayer.Playing {
		return max(0, c.origin)
	}
	// A delayed poll should not stop animation between otherwise valid samples.
	end := c.lastSample.when.Add(1500 * time.Millisecond)
	if now.After(end) {
		now = end
	}
	elapsed := max(0, now.Sub(c.started))
	return max(0, c.origin+time.Duration(c.speed*float64(elapsed)))
}

// observe corrects drift without moving already visible comments backwards.
// Detect seeks from changes in the reported position, independent of drift.
func (c *playbackClock) observe(s sample) bool {
	previous := c.lastSample
	visible := c.position(s.when)
	delta := s.at - visible
	step := s.at - previous.at
	advanced := step != 0
	reset := previous.when.IsZero() || step < -250*time.Millisecond ||
		step > s.when.Sub(c.lastProgress)+650*time.Millisecond
	c.lastSample = s
	c.started = s.when
	if reset || advanced || s.status != previous.status {
		c.lastProgress = s.when
	}
	switch {
	case reset, s.status != previous.status:
		c.origin, c.speed = s.at, 1
	case s.status != potplayer.Playing:
		c.origin, c.speed = s.at, 0
	default:
		c.origin = visible
		if advanced {
			// Duplicate samples reflect PotPlayer's position quantization.
			// Ignore normal phase jitter and ease toward a correction so each
			// new timestamp cannot visibly accelerate or slow the text.
			const phaseTolerance = 200 * time.Millisecond
			drift := delta - max(-phaseTolerance, min(phaseTolerance, delta))
			target := 1 + max(-0.08, min(0.08, float64(drift)/float64(3*time.Second)))
			if c.speed == 0 {
				c.speed = 1
			}
			c.speed += (target - c.speed) * 0.15
		} else if s.when.Sub(c.lastProgress) > 1200*time.Millisecond {
			c.speed = 0
		}
	}
	return reset
}
