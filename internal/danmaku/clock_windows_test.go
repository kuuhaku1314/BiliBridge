package danmaku

import (
	"testing"
	"time"

	"bilibridge/internal/potplayer"
)

func TestPlaybackClockAdvancesBetweenSamples(t *testing.T) {
	start := time.Unix(0, 0)
	var clock playbackClock
	clock.observe(sample{10 * time.Second, start, potplayer.Playing})
	for elapsed := 20 * time.Millisecond; elapsed <= 400*time.Millisecond; elapsed += 20 * time.Millisecond {
		if got, want := clock.position(start.Add(elapsed)), 10*time.Second+elapsed; got != want {
			t.Fatalf("position after %v = %v, want %v", elapsed, got, want)
		}
	}
	next := start.Add(400 * time.Millisecond)
	before := clock.position(next)
	if clock.observe(sample{10250 * time.Millisecond, next, potplayer.Playing}) {
		t.Fatal("a slightly delayed sample was treated as a seek")
	}
	if after := clock.position(next); after != before {
		t.Fatalf("timing correction stepped from %v to %v", before, after)
	}
	if after := clock.position(next.Add(time.Second / 60)); after <= before {
		t.Fatalf("clock stopped after correction at %v", after)
	}
}

func TestPlaybackClockKeepsSpeedAcrossRepeatedPositions(t *testing.T) {
	start := time.Unix(0, 0)
	var clock playbackClock
	clock.observe(sample{10 * time.Second, start, potplayer.Playing})
	for elapsed := 90 * time.Millisecond; elapsed < 500*time.Millisecond; elapsed += 90 * time.Millisecond {
		clock.observe(sample{10 * time.Second, start.Add(elapsed), potplayer.Playing})
	}
	if got := clock.position(start.Add(500 * time.Millisecond)); got != 10500*time.Millisecond {
		t.Fatalf("duplicate position samples changed playback speed, got %v", got)
	}
}

func TestPlaybackClockDoesNotJumpBackWhenVideoResumes(t *testing.T) {
	start := time.Unix(0, 0)
	var clock playbackClock
	clock.observe(sample{10 * time.Second, start, potplayer.Playing})
	when := start.Add(1300 * time.Millisecond)
	before := clock.position(when)
	if clock.observe(sample{10500 * time.Millisecond, when, potplayer.Playing}) {
		t.Fatal("forward progress after a slow frame was misidentified as a seek")
	}
	if after := clock.position(when); after != before {
		t.Fatalf("overlay moved backwards from %v to %v", before, after)
	}
	when = when.Add(140 * time.Millisecond)
	if !clock.observe(sample{9 * time.Second, when, potplayer.Playing}) || clock.position(when) != 9*time.Second {
		t.Fatal("explicit backwards seek did not reset the overlay")
	}
}

func TestPlaybackClockIgnoresJitterOnNewTimestamps(t *testing.T) {
	start := time.Unix(0, 0)
	var clock playbackClock
	clock.observe(sample{time.Second, start, potplayer.Playing})
	for i, elapsed := range []time.Duration{580, 1030, 1600, 2040} {
		at := time.Second + time.Duration(i+1)*500*time.Millisecond
		when := start.Add(elapsed * time.Millisecond)
		if clock.observe(sample{at, when, potplayer.Playing}) || clock.speed != 1 {
			t.Fatalf("sample %d changed the speed to %f", i, clock.speed)
		}
	}
}

func TestPlaybackClockPausesAndResetsAfterSeek(t *testing.T) {
	start := time.Unix(0, 0)
	var clock playbackClock
	clock.observe(sample{time.Second, start, potplayer.Playing})
	clock.observe(sample{1500 * time.Millisecond, start.Add(500 * time.Millisecond), potplayer.Paused})
	if got := clock.position(start.Add(2 * time.Second)); got != 1500*time.Millisecond {
		t.Fatalf("position advanced while paused to %v", got)
	}
	seek := start.Add(3 * time.Second)
	if !clock.observe(sample{time.Minute, seek, potplayer.Paused}) || clock.position(seek) != time.Minute {
		t.Fatal("clock did not move to the seek target while paused")
	}
	clock.observe(sample{time.Minute, seek.Add(time.Second), potplayer.Playing})
	if got := clock.position(seek.Add(2 * time.Second)); got != time.Minute+time.Second {
		t.Fatalf("clock did not resume from the seek target, got %v", got)
	}
}
