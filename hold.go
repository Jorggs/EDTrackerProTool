package main

// recentreHold masks the EDTracker firmware's recentre transient.
//
// On a recentre the firmware stops updating its joystick output while it takes ~100
// samples for the new centre, then moves the output to the new centre *through its
// smoothing filter* (output = output·LPF + new·(1−LPF)). With high smoothing that is a
// slow sweep from the old angle back to zero, seen as the head "spinning round".
// While a hold is active we publish dead-centre instead, and hand back to live data
// once the tracker reports RUNNING and yaw/pitch are near zero and steady.

import (
	"math"
	"sync"
	"time"
)

const (
	holdMinTime    = 250 * time.Millisecond
	holdMaxTime    = 3 * time.Second
	holdNearDeg    = 6.0 // output degrees (after the tracker's own scaling)
	holdStillDeg   = 0.4 // max change between consecutive samples
	holdStableNeed = 4   // consecutive settled samples before releasing
)

type recentreHold struct {
	mu     sync.Mutex
	active bool
	t0     time.Time
	prev   [3]float64
	stable int
}

var trackerHold = &recentreHold{}

func (h *recentreHold) Start() {
	h.mu.Lock()
	h.active, h.t0, h.stable = true, time.Now(), 0
	h.mu.Unlock()
}

func (h *recentreHold) Active() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.active }

// Filter returns what to publish. status is the tracker's device status
// (2 = RUNNING), or -1 if unknown.
func (h *recentreHold) Filter(deg [3]float64, status int, now time.Time) ([3]float64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active {
		h.prev = deg
		return deg, false
	}
	el := now.Sub(h.t0)
	if el > holdMaxTime {
		h.active = false
		return deg, false
	}
	still := math.Abs(deg[0]-h.prev[0]) < holdStillDeg && math.Abs(deg[1]-h.prev[1]) < holdStillDeg
	near := math.Abs(deg[0]) < holdNearDeg && math.Abs(deg[1]) < holdNearDeg // roll is gravity-referenced
	running := status == 2 || status < 0
	h.prev = deg
	if el >= holdMinTime && running && near && still {
		h.stable++
	} else {
		h.stable = 0
	}
	if h.stable >= holdStableNeed {
		h.active = false
		return deg, false
	}
	return [3]float64{}, true
}
