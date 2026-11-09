package main

// UDP output in OpenTrack's "UDP over network" format: six little-endian float64s
// (X, Y, Z in cm, then yaw, pitch, roll in degrees), one datagram per pose.
// An OpenTrack instance elsewhere (e.g. a Shadow PC over Tailscale) with input
// "UDP over network" on the same port receives it.

import (
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strconv"
	"sync"
	"time"
)

type udpOutput struct {
	mu       sync.Mutex
	enabled  bool
	host     string
	port     int
	conn     *net.UDPConn
	addr     string
	err      string
	sent     uint64
	resolved time.Time
}

var udpOut = &udpOutput{port: 4242}

// Control messages sent back from a Receiver to the Tracker over the same UDP flow:
// "EDTRK1" + command byte + uint32 sequence (little-endian). OpenTrack ignores them.
const (
	ctlMagic       = "EDTRK1"
	ctlRecentre    = 0x01
	ctlRecentreAck = 0x81
	ctlLen         = 11
)

func encodeCtl(cmd byte, seq uint32) []byte {
	b := make([]byte, ctlLen)
	copy(b, ctlMagic)
	b[6] = cmd
	binary.LittleEndian.PutUint32(b[7:], seq)
	return b
}

func decodeCtl(b []byte) (cmd byte, seq uint32, ok bool) {
	if len(b) != ctlLen || string(b[:6]) != ctlMagic {
		return 0, 0, false
	}
	return b[6], binary.LittleEndian.Uint32(b[7:]), true
}

// Called (Tracker mode) when the receiver asks us to recentre the tracker itself.
var onRemoteRecentre func() error
var onUDPEvent func(kind, msg string)

func (u *udpOutput) Configure(enabled bool, host string, port int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if port <= 0 || port > 65535 {
		port = 4242
	}
	if host != u.host || port != u.port || !enabled {
		u.closeLocked()
	}
	u.enabled, u.host, u.port = enabled, host, port
	if !enabled {
		u.err = ""
	}
}

func (u *udpOutput) closeLocked() {
	if u.conn != nil {
		u.conn.Close()
		u.conn = nil
	}
	u.addr = ""
}

// connectLocked (re)resolves the host. Re-resolving every 30 s lets a Tailscale
// MagicDNS name or a changed address recover without a restart.
func (u *udpOutput) connectLocked() bool {
	if u.conn != nil && time.Since(u.resolved) < 30*time.Second {
		return true
	}
	if u.host == "" {
		u.err = "no destination set"
		return false
	}
	ra, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(u.host, strconv.Itoa(u.port)))
	if err != nil {
		u.err = "can't resolve " + u.host + ": " + err.Error()
		u.resolved = time.Now() // don't hammer DNS
		u.closeLocked()
		return false
	}
	if u.conn != nil && u.conn.RemoteAddr().String() == ra.String() {
		u.resolved = time.Now()
		return true
	}
	u.closeLocked()
	c, err := net.DialUDP("udp4", nil, ra)
	if err != nil {
		u.err = err.Error()
		return false
	}
	u.conn, u.addr, u.err, u.resolved = c, ra.String(), "", time.Now()
	go u.readLoop(c)
	return true
}

// readLoop answers control messages from the receiver. The socket is connected,
// so only packets from the destination we send to are delivered here.
func (u *udpOutput) readLoop(c *net.UDPConn) {
	buf := make([]byte, 64)
	var lastSeq uint32
	var haveSeq, lastOK bool
	for {
		n, err := c.Read(buf)
		if err != nil {
			// Usually ICMP "port unreachable": nothing is listening yet. On Windows the
			// socket stops delivering replies after that, so drop it; the next pose
			// opens a fresh one (after the usual 3 s back-off).
			u.mu.Lock()
			if u.conn == c {
				u.err = "nothing listening at " + u.addr + " — is the receiver running?"
				u.closeLocked()
				u.resolved = time.Now()
			}
			u.mu.Unlock()
			return
		}
		cmd, seq, ok := decodeCtl(buf[:n])
		if !ok || cmd != ctlRecentre {
			continue
		}
		if !haveSeq || seq != lastSeq { // the receiver sends each request a few times
			haveSeq, lastSeq = true, seq
			var rerr error
			if onRemoteRecentre != nil {
				rerr = onRemoteRecentre()
			}
			if onUDPEvent != nil {
				if rerr != nil {
					onUDPEvent("warn", "Receiver asked to recentre, but: "+rerr.Error())
				} else {
					onUDPEvent("hotkey", "Recentred tracker (requested by receiver)")
				}
			}
			lastOK = rerr == nil
		}
		if !lastOK {
			continue // no ack: the receiver falls back to a software recentre
		}
		c.Write(encodeCtl(ctlRecentreAck, seq))
	}
}

func encodeOpenTrackPose(yaw, pitch, roll float64) []byte {
	buf := make([]byte, 48)
	vals := [6]float64{0, 0, 0, yaw, pitch, roll}
	for i, v := range vals {
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
	}
	return buf
}

// Pose sends one datagram (degrees, same convention as the FreeTrack output).
func (u *udpOutput) Pose(yaw, pitch, roll float64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.enabled {
		return
	}
	if u.conn == nil && time.Since(u.resolved) < 3*time.Second && u.err != "" {
		return // back off after a failure
	}
	if !u.connectLocked() {
		return
	}
	if _, err := u.conn.Write(encodeOpenTrackPose(yaw, pitch, roll)); err != nil {
		u.err = err.Error()
		u.closeLocked()
		u.resolved = time.Now()
		return
	}
	u.err = ""
	u.sent++
}

func (u *udpOutput) Status() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return map[string]any{"udp": u.enabled, "udpHost": u.host, "udpPort": u.port,
		"udpAddr": u.addr, "udpErr": u.err, "udpSent": u.sent}
}

func (u *udpOutput) String() string {
	return fmt.Sprintf("%s:%d", u.host, u.port)
}
