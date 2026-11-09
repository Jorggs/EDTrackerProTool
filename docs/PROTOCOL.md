# EDTracker Pro – USB protocol (recovered from EDTrackerPro.exe)

Source: decompiled `EDTrackerUI5.Dongle` and `EDTrackerUI3.Form1` / `magCloud` (.NET 4, HidSharp).
The serial/COM port is **not** used. Everything is **HID feature reports** on the joystick device.
All structs are packed, little-endian. Offsets below exclude the leading report-ID byte.
The original app always pads writes to 44 bytes + ID (45 total).

## USB IDs the old UI searched for
| VID:PID | Notes |
|---|---|
| 16D0:0AEA, 16D0:0AEB | Pro, wired (accel Z sign inverted) |
| 16D0:0AEC | Pro Wireless dongle |
| 1915:9005 | Wireless (Nordic VID) |
| 16D0:0C42, 03EB:2044 | other/wired variants (0C42 also bootloader-related) |

## Report IDs
| ID | Name | Direction |
|---|---|---|
| 1 | Joystick input | input: int16 x(yaw), y(pitch), z(roll); degrees = v × 180 / 32768 |
| 2 | CONFIG | get / set |
| 3 | MAG calibration | get / set |
| 4 | COMMAND | set: `[cmd, option]` |
| 5 | AUX (live sensors + status) | get (polled ~every 10–50 ms) |
| 6 | CHANNEL | get: `[rfChannel]` |
| 7 | CHANNEL SCAN | get: `[seq, 30 × channel data]` |

### CONFIG (ID 2)
| Off | Type | Field | Meaning |
|---|---|---|---|
| 0 | u8 | version | bits 6-4 major, 3-0 minor, bit 7 beta. Write 0. `(v&0x7F)<18` ⇒ old "screws-up" firmware |
| 1 | u8 | rollScale | ×¼ |
| 2 | u8 | expRollScale | ×¼ |
| 3 | u8 | outputOff | 1 = output disabled |
| 4 | u8 | orientation | 0 Top/USB right, 1 Top/USB front, 2 Top/USB left, 3 Top/USB rear, 4 Left/USB down, 5 Right/USB down |
| 5 | u8 | scaleMode | 0 linear, 1 exponential |
| 6 | u8 | yawScale | ×¼ (UI range 1–25) |
| 7 | u8 | pitchScale | ×¼ |
| 8 | u8 | expYawScale | ×¼ |
| 9 | u8 | expPitchScale | ×¼ |
| 10 | u16 | LPF | UI writes `min(32767, (smoothing/99.05)^0.15 × 32767)`, smoothing 0–99 |
| 12 | u8 | channel | RF channel × 3 |

### MAG (ID 3)
| Off | Type | Field |
|---|---|---|
| 0 | u8 | sequenceID (echo back what was read) |
| 1 | int16[3] | hard-iron offsets, raw mag units × 32 |
| 7 | int16[9] | soft-iron matrix C, row-major, Q14 (×16384) |
| 25 | int16[9] | "oriented" matrix = C · Rz(θ) [· F], row-major, Q14 |

Orientation θ and flip (Form1.cbOrientation_SelectedIndexChanged):
0 → π · 1 → +π/2 (−π/2 if screws-up) · 2 → 0 · 3 → −π/2 (+π/2 if screws-up) · 4 → 0 with F · 5 → π with F

`Rz = [[cosθ, sinθ, 0], [−sinθ, cosθ, 0], [0, 0, 1]]`, `F = [[0,0,−1],[0,1,0],[1,0,0]]`.
Changing orientation in the config means you must re-send MAG with the new oriented matrix.

### AUX (ID 5)
| Off | Type | Field |
|---|---|---|
| 0 | int16[3] | gyro |
| 6 | int16[3] | accel (16384 = 1 g) |
| 12 | int16[3] | magRaw (only populated in FULL_DATA_OUT mode) |
| 18 | u16 | voltage — wireless: V = (v − 70) / 750 |
| 20 | u16 | magHeading — degrees = v / 182 (wrap > 180) |
| 22 | u8 | chargeStatus — bit 0: 0 charging / 1 charged; bits 7-1 thermistor |
| 23 | u8 | deviceStatus — 0 BIAS, 1 RECENTRE, 2 RUNNING, 3 SLEEPING, 4 NO SIGNAL, 5 UNPAIRED |
| 24 | u8 | rfCount (messages/s) |
| 25 | u8 | CalibrationDataAvailable → read CONFIG |
| 26 | u8 | RawMagMatrixAvailable |
| 27 | u8 | OrientatedMatrixAvailable → (with 26) read MAG |
| 28 | u8 | ChannelDataAvailable → read CHANNEL |
| 29 | u8 | sessionID |

## Commands (report 4, byte 0 = cmd, byte 1 = option)
| Cmd | Name | Notes |
|---|---|---|
| 1 | RECENTER | |
| 2 | AUTO_BIAS | gyro bias, keep still |
| 3 | GET_CONFIG | ask device to publish CONFIG |
| 6 | SCAN_CHANNELS | after RF channel change |
| 9 | FULL_BIAS | gyro + accel; used by wizard when firmware major ≥ 2 |
| 10 / 11 | CARRIER_ON / OFF | RF test, option = channel |
| 12 | FULL_DATA_OUT | enables raw mag in AUX (needed for calibration) |
| 13 | MIN_DATA_OUT | back to normal |
| 14 / 15 | OUTPUT_OFF / ON | joystick output |
| 16 | DEGAUSS | |
| 17 | TXSTATE | option = TX mode |
| 20 | REBIND | re-pair wireless |
| 21–24 | DONGLE carrier on/off, TX state, channel | |
| 99 | REBOOT to bootloader | **don't** send unless reflashing |

## Calibration sequence (original wizard)
1. Tracker flat, logo up (check accel Z > 0). Send 9 (or 2 on fw < 2), then 12. Bias is done when status passes BIAS/RECENTRE and returns to RUNNING.
2. User chooses mounting orientation.
3. Collect magRaw from AUX while user rotates. Accept a sample if |m| < 2× mean radius and the angle from the previous accepted sample is > 2°. Stop at 900 samples.
4. Fit:
   - **Standard**: offsets = mid-range per axis; C = diag(maxRange / range_i).
   - **Alt.1**: least-squares quadric (x², y², z², 2xy, 2xz, 2yz, 2x, 2y, 2z = 1) → centre = offsets; eigen-decompose the normalised 3×3 → C = V · diag(r_min / r_i) · Vᵀ.
5. Write CONFIG (orientation), write MAG (offsets, C, oriented), RECENTER ×2, then 13.
