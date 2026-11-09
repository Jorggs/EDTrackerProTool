package main

// Receiver mode: listen for OpenTrack "UDP over network" packets (six little-endian
// float64s: X, Y, Z, yaw, pitch, roll) from our own Tracker mode or from OpenTrack,
// apply a software centre, and hand the pose to the outputs (FreeTrack/TrackIR, vJoy).

import (
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"sync"
	"time"
)

type receiver struct {
	mu      sync.Mutex
	port    int
	conn    *net.UDPConn
	centre  [3]float64
	last    [3]float64
	lastAt  time.Time
	from    string
	count   uint64
	err     string
	running bool
	onPose  func(yaw, pitch, roll float64)
	onEvent func(kind, msg string)

	fromAddr *net.UDPAddr
	seq      uint32
	ackedSeq uint32
	remoteOK bool   // the sender has acknowledged a remote recentre before
	lastMode string // "tracker" or "software"

	// While a remote recentre is in progress the output is held still, so the
	// moment between the tracker recentring and us dropping our offset can't
	// flick the view (the "head spins round" effect).
	holding bool
	holdOut [3]float64
	holdSeq uint32
	lastOut [3]float64
}

// settledYaw: after a remote recentre, the tracker's yaw comes back near zero.
const settledYaw = 12.0

var recv = &receiver{port: 4242}

func wrap180(a float64) float64 {
	a = math.Mod(a+180, 360)
	if a < 0 {
		a += 360
	}
	return a - 180
}

func decodeOpenTrackPose(b []byte) ([3]float64, bool) {
	var out [3]float64
	if len(b) < 48 {
		return out, false
	}
	for i := 0; i < 3; i++ {
		v := math.Float64frombits(binary.LittleEndian.Uint64(b[(3+i)*8:]))
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e4 {
			return out, false
		}
		out[i] = v
	}
	return out, true
}

// Start listens on the given UDP port (all interfaces, so Tailscale traffic arrives).
func (r *receiver) Start(port int) error {
	r.Stop()
	if port <= 0 || port > 65535 {
		port = 4242
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	r.mu.Lock()
	r.port = port
	if err != nil {
		r.err = fmt.Sprintf("can't listen on UDP %d: %v", port, err)
		r.mu.Unlock()
		return fmt.Errorf("%s", r.err)
	}
	r.conn, r.running, r.err = c, true, ""
	r.mu.Unlock()
	go r.loop(c)
	return nil
}

func (r *receiver) Stop() {
	r.mu.Lock()
	c := r.conn
	r.conn, r.running = nil, false
	r.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func (r *receiver) loop(c *net.UDPConn) {
	buf := make([]byte, 256)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			r.mu.Lock()
			still := r.conn == c
			if still {
				r.err = err.Error()
			}
			r.mu.Unlock()
			if !still {
				return // stopped
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if cmd, seq, ok := decodeCtl(buf[:n]); ok {
			if cmd == ctlRecentreAck {
				r.mu.Lock()
				if seq == r.seq {
					r.ackedSeq = seq
				}
				r.mu.Unlock()
			}
			continue
		}
		pose, ok := decodeOpenTrackPose(buf[:n])
		if !ok {
			continue
		}
		r.mu.Lock()
		r.last, r.lastAt, r.from, r.fromAddr = pose, time.Now(), from.IP.String(), from
		r.count++
		var out [3]float64
		released := false
		if r.holding && r.ackedSeq == r.holdSeq && math.Abs(wrap180(pose[0])) < settledYaw {
			// acknowledged and the tracker's data has actually recentred: release
			r.centre, r.holding, r.remoteOK, r.lastMode = [3]float64{}, false, true, "tracker"
			released = true
		}
		if r.holding {
			out = r.holdOut
		} else {
			for i := range out {
				out[i] = wrap180(pose[i] - r.centre[i])
			}
		}
		r.lastOut = out
		cb := r.onPose
		r.mu.Unlock()
		if released {
			r.event("info", "Recentred the tracker on the sending PC")
		}
		if cb != nil {
			cb(out[0], out[1], out[2])
		}
	}
}

// Recentre asks the sending PC to recentre the tracker itself (so offsets never
// accumulate). If the sender doesn't acknowledge within 400 ms — OpenTrack, or an
// older version of this tool — the current received pose becomes the new zero instead.
func (r *receiver) Recentre() {
	r.mu.Lock()
	c, to := r.conn, r.fromAddr
	live := time.Since(r.lastAt) < time.Second
	snapshot := r.last
	if c == nil || to == nil || !live {
		r.centre, r.lastMode = snapshot, "software"
		r.mu.Unlock()
		return
	}
	r.seq++
	seq := r.seq
	r.holding, r.holdOut, r.holdSeq = true, r.lastOut, seq
	r.mu.Unlock()
	msg := encodeCtl(ctlRecentre, seq)
	for i := 0; i < 3; i++ { // UDP can drop a packet; the sender de-duplicates by sequence
		c.WriteToUDP(msg, to)
	}
	go func() {
		// No answer within 400 ms: the sender can't do it (OpenTrack) — recentre here.
		time.Sleep(400 * time.Millisecond)
		r.mu.Lock()
		if r.holding && r.holdSeq == seq && r.ackedSeq != seq {
			r.centre, r.holding, r.lastMode = snapshot, false, "software"
			r.mu.Unlock()
			r.event("info", "Sender didn't answer (OpenTrack or older version?) — recentred here instead")
			return
		}
		r.mu.Unlock()
		// Answered. The sending PC holds its output at centre while the tracker
		// settles, so the data normally comes back near zero within a frame or two.
		// If it doesn't (an older sender, or the head moving a lot), trust the tracker
		// anyway: never take a software centre from a mid-settle reading.
		time.Sleep(3100 * time.Millisecond)
		r.mu.Lock()
		if r.holding && r.holdSeq == seq {
			r.centre, r.holding, r.remoteOK, r.lastMode = [3]float64{}, false, true, "tracker"
			r.mu.Unlock()
			r.event("info", "Recentred the tracker on the sending PC")
			return
		}
		r.mu.Unlock()
	}()
}

func (r *receiver) event(kind, msg string) {
	if r.onEvent != nil {
		r.onEvent(kind, msg)
	}
}

func (r *receiver) Status() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{"rxListening": r.running, "rxPort": r.port, "rxFrom": r.from, "rxCount": r.count,
		"rxErr": r.err, "rxLive": r.running && time.Since(r.lastAt) < time.Second,
		"rxRemoteOK": r.remoteOK, "rxLastRecentre": r.lastMode}
}
