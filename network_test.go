package main

// Tests for the platform-independent parts: OpenTrack UDP format, Receiver mode,
// remote recentre (with OpenTrack fallback) and the recentre hold that masks the
// firmware's smoothing sweep. Run with: go test -v ./...

import (
	"encoding/binary"
	"math"
	"net"
	"sync"
	"testing"
	"time"
)

// fresh resets the shared state between tests.
func fresh(t *testing.T) {
	t.Helper()
	if recv != nil {
		recv.Stop()
	}
	if udpOut != nil {
		udpOut.Configure(false, "", 0)
	}
	recv = &receiver{port: 4242}
	udpOut = &udpOutput{port: 4242}
	trackerHold = &recentreHold{}
	onRemoteRecentre, onUDPEvent = nil, nil
}

func freePort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestOpenTrackPacketFormat(t *testing.T) {
	fresh(t)
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	udpOut.Configure(true, "127.0.0.1", l.LocalAddr().(*net.UDPAddr).Port)
	udpOut.Pose(12.5, -7.25, 3)
	l.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 100)
	n, _, err := l.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 48 {
		t.Fatalf("packet length %d, want 48", n)
	}
	var v [6]float64
	for i := range v {
		v[i] = math.Float64frombits(binary.LittleEndian.Uint64(buf[i*8:]))
	}
	if v != [6]float64{0, 0, 0, 12.5, -7.25, 3} {
		t.Fatalf("got %v", v)
	}
}

func TestReceiverRecentreAndWrap(t *testing.T) {
	fresh(t)
	port := freePort(t)
	got := make(chan [3]float64, 10)
	recv.onPose = func(y, p, r float64) { got <- [3]float64{y, p, r} }
	if err := recv.Start(port); err != nil {
		t.Fatal(err)
	}
	defer recv.Stop()
	c, _ := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	defer c.Close()
	send := func(y, p float64) {
		b := make([]byte, 48)
		for i, v := range []float64{0, 0, 0, y, p, 0} {
			binary.LittleEndian.PutUint64(b[i*8:], math.Float64bits(v))
		}
		c.Write(b)
	}
	send(170, -10)
	<-got
	recv.Recentre() // OpenTrack-style sender (never answers): software recentre after 400 ms
	time.Sleep(450 * time.Millisecond)
	send(-175, -8) // crosses ±180: 15° right of the new centre
	v := <-got
	for len(got) > 0 {
		v = <-got
	}
	if math.Abs(v[0]-15) > 0.1 || math.Abs(v[1]-2) > 0.1 {
		t.Fatalf("after recentre got %v, want ~[15 2 0]", v)
	}
	c.Write([]byte("junk")) // ignored
	select {
	case x := <-got:
		t.Fatalf("junk produced a pose %v", x)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRemoteRecentreReachesTracker(t *testing.T) {
	fresh(t)
	port := freePort(t)
	var mu sync.Mutex
	raw, recentres := 30.0, 0
	onRemoteRecentre = func() error { mu.Lock(); raw = 0; recentres++; mu.Unlock(); return nil }
	if err := recv.Start(port); err != nil {
		t.Fatal(err)
	}
	defer recv.Stop()
	udpOut.Configure(true, "127.0.0.1", port)
	stop := make(chan bool)
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			mu.Lock()
			y := raw
			mu.Unlock()
			udpOut.Pose(y, 0, 0)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	recv.Recentre()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if recentres != 1 {
		t.Fatalf("tracker recentred %d times, want exactly 1 (request is sent 3 times)", recentres)
	}
	if s := recv.Status(); s["rxLastRecentre"] != "tracker" {
		t.Fatalf("status %v", s)
	}
}

// The firmware recentres by pausing ~100 samples then gliding through its smoothing
// filter; the hold must keep that sweep away from the games.
func TestHoldMasksFirmwareSweep(t *testing.T) {
	for _, c := range []struct{ lpf, start float64 }{{0.992, 150}, {0.90, 150}, {0, 20}} {
		h := &recentreHold{}
		now := time.Unix(0, 0)
		h.Start()
		h.t0 = now
		out, fw := c.start, 0
		worst, released := 0.0, false
		for step := 0; step < 300; step++ {
			for k := 0; k < 3; k++ { // 200 Hz firmware, 15 ms tool poll
				fw++
				if fw > 100 {
					out *= c.lpf
				}
			}
			status := 2
			if fw <= 100 {
				status = 1
			}
			pub, held := h.Filter([3]float64{out, 0, 0}, status, now)
			if !released {
				worst = math.Max(worst, math.Abs(pub[0]))
			}
			if !held {
				released = true
			}
			now = now.Add(15 * time.Millisecond)
		}
		if !released || worst > 6.5 {
			t.Fatalf("LPF %.3f: released=%v, largest yaw leaked %.1f°", c.lpf, released, worst)
		}
	}
}

// F6 bound on both PCs: the home PC recentres at once, the receiver's request
// arrives ~30 ms later. The game must never see the sweep, and the tracker must
// only be recentred once.
func TestRecentreFromBothMachinesNoSpin(t *testing.T) {
	fresh(t)
	port := freePort(t)
	var mu sync.Mutex
	fwOut, fwCount, fwRecentres := 150.0, 1000, 0
	var lastLocal time.Time
	local := func() {
		trackerHold.Start()
		mu.Lock()
		lastLocal, fwCount = time.Now(), 0
		fwRecentres++
		mu.Unlock()
	}
	onRemoteRecentre = func() error {
		mu.Lock()
		recent := time.Since(lastLocal) < time.Second
		mu.Unlock()
		if !recent {
			local()
		}
		return nil
	}
	stop := make(chan bool)
	defer close(stop)
	go func() { // firmware: 100-sample pause then smoothing glide, 200 Hz
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
			}
			mu.Lock()
			fwCount++
			if fwCount > 100 {
				fwOut *= 0.992
			}
			mu.Unlock()
		}
	}()
	go func() { // tracker-side tool: hold filter, then UDP
		for {
			select {
			case <-stop:
				return
			default:
			}
			mu.Lock()
			y, status := fwOut, 2
			if fwCount <= 100 {
				status = 1
			}
			mu.Unlock()
			out, _ := trackerHold.Filter([3]float64{y, 0, 0}, status, time.Now())
			udpOut.Pose(out[0], out[1], out[2])
			time.Sleep(15 * time.Millisecond)
		}
	}()
	var game []float64
	recv.onPose = func(y, p, r float64) { mu.Lock(); game = append(game, y); mu.Unlock() }
	if err := recv.Start(port); err != nil {
		t.Fatal(err)
	}
	defer recv.Stop()
	udpOut.Configure(true, "127.0.0.1", port)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	game = nil
	mu.Unlock()
	local()
	time.Sleep(30 * time.Millisecond)
	recv.Recentre()
	time.Sleep(3500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	worst := 0.0
	for _, y := range game {
		worst = math.Max(worst, math.Abs(y))
	}
	if worst > 7 || fwRecentres != 1 {
		t.Fatalf("largest yaw seen by the game %.1f°, firmware recentres %d", worst, fwRecentres)
	}
}
