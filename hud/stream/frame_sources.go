package stream

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

type FrameSource interface {
	NextFrame(now time.Time) ([]byte, error)
}

type LiveSource interface {
	FrameSource
	IsActive(now time.Time) bool
	PushFrame(au []byte)
	PrepareForConsumer()
	SetOpaquePayloads(enabled bool)
	SetPollMode(enabled bool)
}

var streamHeader = []byte{
	0x02, 0x00, 0x00, 0x00,
	0x20, 0x03, 0x80, 0x01,
}
var audNAL = []byte{0x00, 0x00, 0x00, 0x01, 0x09, 0xF0}

func splitByAudAnnexB(data []byte) [][]byte {
	var starts []int
	i := 0

	isStartCode := func(pos int) int {
		if pos+3 < len(data) &&
			data[pos] == 0x00 &&
			data[pos+1] == 0x00 &&
			data[pos+2] == 0x01 {
			return 3
		}
		if pos+4 < len(data) &&
			data[pos] == 0x00 &&
			data[pos+1] == 0x00 &&
			data[pos+2] == 0x00 &&
			data[pos+3] == 0x01 {
			return 4
		}
		return 0
	}

	for i < len(data) {
		startCode := isStartCode(i)
		if startCode != 0 {
			nalPos := i + startCode
			if nalPos < len(data) {
				nal := int(data[nalPos] & 0x1F)
				if nal == 9 { // AUD
					starts = append(starts, i)
				}
			}
			i += startCode
		} else {
			i++
		}
	}

	if len(starts) == 0 {
		return nil
	}

	out := make([][]byte, 0, len(starts))
	for idx := 0; idx < len(starts); idx++ {
		begin := starts[idx]
		end := len(data)
		if idx+1 < len(starts) {
			end = starts[idx+1]
		}

		// Copy the slice so the array can be garbage collected and reused safely
		chunk := make([]byte, end-begin)
		copy(chunk, data[begin:end])
		out = append(out, chunk)
	}

	return out
}

func startsWithStreamHeader(b, header []byte) bool {
	if len(b) < len(header) {
		return false
	}
	for i := range header {
		if b[i] != header[i] {
			return false
		}
	}
	return true
}

func concatBytes(parts ...[]byte) []byte {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, total)
	pos := 0
	for _, p := range parts {
		copy(out[pos:], p)
		pos += len(p)
	}
	return out
}

type FileFrameSource struct {
	mu sync.Mutex

	aus       [][]byte // AUD-delimited access units
	replayIdx int
	pollCount uint64

	lastAu       []byte
	lastAdvance  time.Time
	interval     time.Duration // 1/target fps
	streamHeader []byte        // optional SPS/PPS/etc to prepend for first poll (warmup)
}

// NewFileFrameSource loads a .h264 file, splits it into AUD-delim AUs
// annex-b and creates a replay source.
func NewFileFrameSource(path string, targetFPS int) (*FileFrameSource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading h264 file %q: %w", path, err)
	}

	aus := splitByAudAnnexB(data)
	if len(aus) == 0 {
		return nil, fmt.Errorf("no AUD-delimited AUs found in %q", path)
	}

	if targetFPS <= 0 {
		targetFPS = 15
	}

	interval := time.Second / time.Duration(targetFPS)
	return &FileFrameSource{
		aus:          aus,
		replayIdx:    0,
		pollCount:    0,
		lastAu:       nil,
		lastAdvance:  time.Time{},
		interval:     interval,
		streamHeader: append([]byte(nil), streamHeader...), // copy bytes
	}, nil
}

func (s *FileFrameSource) NextFrame(now time.Time) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Nothing to send
	if len(s.aus) == 0 {
		return nil, nil
	}

	firstPoll := s.pollCount == 0

	shouldAdvAU := false
	if s.lastAu == nil {
		shouldAdvAU = true
	} else {
		if s.lastAdvance.IsZero() || now.Sub(s.lastAdvance) >= s.interval {
			shouldAdvAU = true
		}
	}

	if shouldAdvAU {
		au := s.aus[s.replayIdx%len(s.aus)]
		s.replayIdx++

		// Prepend stream header on first payload if absent
		if firstPoll && !startsWithStreamHeader(au, s.streamHeader) {
			au = concatBytes(s.streamHeader, au)
		}

		s.lastAu = au
		s.lastAdvance = now
		s.pollCount++

		return au, nil
	}

	// Idle poll - keep cadence, no new frame
	s.pollCount++
	return nil, nil
}

type RawFrameSource struct {
	FileFrameSource
}

func NewRawFrameSource(src []byte, targetFPS int) (*RawFrameSource, error) {
	aus := splitByAudAnnexB(src)
	if len(aus) == 0 {
		return nil, errors.New("no AUD-delimited AUs found in source")
	}

	if targetFPS <= 0 {
		targetFPS = 15
	}

	interval := time.Second / time.Duration(targetFPS)
	return &RawFrameSource{
		FileFrameSource: FileFrameSource{
			aus:          aus,
			replayIdx:    0,
			pollCount:    0,
			lastAu:       nil,
			lastAdvance:  time.Time{},
			interval:     interval,
			streamHeader: append([]byte(nil), streamHeader...),
		},
	}, nil
}

// A dash that polls for each H.264 frame (wantFps=0) gets a queue of up to pollQueueAUs, about a
// second at 30 fps, and pollQueueBytes, a memory bound that leaves room for a second of 13 Mbps and
// an IDR. Such a dash pauses its polls for a few hundred ms now and then (decoding a large IDR, the
// Zontes 350D), then pulls faster than we produce (35/s against 30), so it drains what piled up on
// its own; the 6-AU queue overflowed on every such pause and froze the picture until an IDR came.
// Drop-to-IDR stays the answer to a queue that overflows this one.
//
// Carbit has no such queue: it feeds its encoder one frame only when none is in flight
// (mInputCacheCountMax 0, base/l0.java:594-598, q0.java:1138-1141, 585-596), so a dash that stops
// polling stops the encoder and nothing piles up. Our encoder runs at its own rate, hence this.
//
// ponytail: the cap is also the latency bound, ~1 s while the dash is behind. No timer drops a
// backlog that lingers: one would fire on a dash draining at 5 AU/s after a 1 s pause, and a dash
// that truly can't keep up fills the cap and overflows anyway. Add one if field lag says so.
const (
	pollQueueAUs   = 32
	pollQueueBytes = 4 << 20
)

type LiveStreamSource struct {
	mu sync.Mutex

	// Ring buffer for queued AUs for efficiency purposes
	aus      [][]byte
	head     int
	tail     int
	count    int
	capacity int // ring slots: the deeper of maxQ and pollQueueAUs
	maxQ     int // the queue's depth outside H.264 poll mode
	bytes    int // what the queued AUs add up to
	polled   bool

	lastAU      []byte // Freeze frame
	lastAdvance time.Time
	interval    time.Duration // 1/fps

	active      bool
	lastInput   time.Time
	liveTimeout time.Duration
	awaitIDR    bool
	// opaque means the payloads pushed here are not H.264 access units and must be
	// forwarded byte-for-byte: no IDR gate on the way in, no NAL inspection before a
	// repeat on the way out. It exists for the dashboards that negotiate JPEG stills,
	// where every payload is self-contained and the IDR rules would drop all of them.
	opaque bool

	// OnKeyframeNeeded, when set before the first push, is told that the queue overflowed
	// with nothing decodable left in it and is now dropping until the next IDR, with how many
	// AUs went. The host answers by asking its encoder for a sync frame. Called from PushFrame,
	// outside the lock, at most once a second, and again each second the IDR is still owed.
	OnKeyframeNeeded func(dropped int)
	lastKeyframeAsk  time.Time
	keyframeOwed     bool // an overflow armed the IDR wait and no IDR has arrived since
	owedDropped      int  // AUs dropped since the last ask
}

func NewLiveStreamSource(targetFPS int, liveTimeout time.Duration, maxQ int) *LiveStreamSource {
	if targetFPS <= 0 {
		targetFPS = 15
	}
	if liveTimeout <= 0 {
		liveTimeout = 3 * time.Second
	}
	if maxQ <= 0 {
		maxQ = 15
	}
	return &LiveStreamSource{
		aus:         make([][]byte, max(maxQ, pollQueueAUs)),
		capacity:    max(maxQ, pollQueueAUs),
		maxQ:        maxQ,
		lastAU:      nil,
		lastAdvance: time.Time{},
		interval:    time.Second / time.Duration(targetFPS),
		active:      false,
		lastInput:   time.Time{},
		liveTimeout: liveTimeout,
		awaitIDR:    true,
	}
}

// SetOpaquePayloads switches the source to forwarding whole self-contained payloads -
// JPEG stills - instead of H.264 access units, or back. A switch drops everything queued or
// kept for a repeat, none of which the new codec can decode, and sets the IDR wait for the new
// one: off for stills, which carry no NAL of type 5 and would otherwise all be dropped, on for
// H.264, which must start at an IDR. The same setting again changes nothing.
func (s *LiveStreamSource) SetOpaquePayloads(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if enabled != s.opaque {
		s.opaque = enabled
		s.clearLocked()
	}
}

// SetPollMode deepens the H.264 queue to pollQueueAUs for a dash that polls for each frame, or
// returns it to the constructor's depth. JPEG stills keep that depth either way. A queue deeper
// than the new depth drops to an IDR on the next push.
func (s *LiveStreamSource) SetPollMode(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polled = enabled
}

// fullLocked reports whether queueing n more bytes needs room made first. An empty queue always
// takes the AU, however big.
func (s *LiveStreamSource) fullLocked(n int) bool {
	if s.count == 0 {
		return false
	}
	if s.polled && !s.opaque {
		return s.count >= s.capacity || s.bytes+n > pollQueueBytes
	}
	return s.count >= s.maxQ
}

// PushFrame is called by a live encoder, expected H.264 AU in Annex B aud + sps/pps + idr,
// or a whole self-contained payload when SetOpaquePayloads is on.
func (s *LiveStreamSource) PushFrame(au []byte) {
	if len(au) == 0 {
		return
	}

	s.mu.Lock()
	dropped := s.pushLocked(au)
	notify := s.OnKeyframeNeeded
	s.mu.Unlock()

	if dropped > 0 && notify != nil {
		notify(dropped)
	}
}

// pushLocked queues au and returns how many AUs it dropped to wait for an IDR, or 0.
func (s *LiveStreamSource) pushLocked(au []byte) int {
	now := time.Now()
	s.active = true
	s.lastInput = now
	if s.awaitIDR && !s.opaque {
		if !hasAnnexBNALType(au, 5) {
			if s.keyframeOwed {
				s.owedDropped++
				return s.askForKeyframeLocked(now)
			}
			return 0
		}
		s.awaitIDR = false
		s.keyframeOwed, s.owedDropped = false, 0
	}

	dropped := 0
	for s.fullLocked(len(au)) {
		if s.opaque {
			// Every still is a whole picture: dropping the oldest loses nothing else.
			s.dropHeadLocked()
		} else {
			dropped += s.dropToIDRLocked()
		}
	}
	if dropped > 0 && s.count == 0 && !hasAnnexBNALType(au, 5) {
		// Nothing left decodes without what was just dropped, and neither does this AU.
		// The official app never gets here - one frame in flight per poll
		// (base/q0.java:585-596, 713-721) - so the queue waits for an IDR and asks for one.
		s.awaitIDR = true
		s.keyframeOwed = true
		s.owedDropped += dropped + 1
		logging.Printf("LiveStreamSource: queue overflowed with no IDR in it; dropped %d AUs, waiting for an IDR", dropped+1)
		return s.askForKeyframeLocked(now)
	}

	buf := append([]byte(nil), au...) // copy buffer so memory can be reused

	s.aus[s.tail] = buf
	s.tail = (s.tail + 1) % s.capacity
	s.count++
	s.bytes += len(buf)
	return 0
}

// askForKeyframeLocked returns how many AUs to report with an ask, or 0 when the last ask was
// under a second ago. ponytail: one ask a second - a dash that stops polling overflows the
// queue every few frames, and an IDR every 200 ms would be most of the bitrate - but asked
// again each second the IDR is still owed, so a suppressed ask is never the last one.
func (s *LiveStreamSource) askForKeyframeLocked(now time.Time) int {
	if now.Sub(s.lastKeyframeAsk) < time.Second {
		return 0
	}
	s.lastKeyframeAsk = now
	n := s.owedDropped
	s.owedDropped = 0
	return n
}

// dropToIDRLocked drops the oldest AU and every predicted AU behind it up to the next queued
// IDR. It used to drop the oldest alone, which on a GOP stream smears the picture until the
// next IDR: every frame after a lost P-frame predicts from a reference the dash never got
// (the 2026-10-06 Zontes GOP test). Whatever goes out next now decodes.
func (s *LiveStreamSource) dropToIDRLocked() int {
	dropped := 0
	for s.count > 0 && (dropped == 0 || !hasAnnexBNALType(s.aus[s.head], 5)) {
		s.dropHeadLocked()
		dropped++
	}
	return dropped
}

func (s *LiveStreamSource) dropHeadLocked() {
	s.bytes -= len(s.aus[s.head])
	s.aus[s.head] = nil
	s.head = (s.head + 1) % s.capacity
	s.count--
}

// PrepareForConsumer clears stale predictive frames and waits for a fresh IDR.
func (s *LiveStreamSource) PrepareForConsumer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLocked()
}

func (s *LiveStreamSource) clearLocked() {
	for i := range s.aus {
		s.aus[i] = nil
	}
	s.count = 0
	s.bytes = 0
	s.head = 0
	s.tail = 0
	s.lastAU = nil
	s.lastAdvance = time.Time{}
	s.awaitIDR = !s.opaque
}

func (s *LiveStreamSource) IsActive(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.active {
		return false
	}

	if !s.lastInput.IsZero() && now.Sub(s.lastInput) > s.liveTimeout {
		s.resetLocked()
		return false
	}
	return true
}

func (s *LiveStreamSource) resetLocked() {
	s.active = false
	s.lastAU = nil
	s.lastAdvance = time.Time{}
	s.count = 0
	s.bytes = 0
	s.head = 0
	s.tail = 0
	s.awaitIDR = !s.opaque
}

func hasAnnexBNALType(data []byte, wanted byte) bool {
	for i := 0; i+3 < len(data); {
		startCodeLen := 0
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			startCodeLen = 3
		} else if i+4 < len(data) && data[i] == 0 && data[i+1] == 0 &&
			data[i+2] == 0 && data[i+3] == 1 {
			startCodeLen = 4
		}
		if startCodeLen == 0 {
			i++
			continue
		}
		nalIndex := i + startCodeLen
		if nalIndex < len(data) && data[nalIndex]&0x1f == wanted {
			return true
		}
		i = nalIndex + 1
	}
	return false
}

// NextFrame fits our FrameSource interface's NextFrame signature.
//
// The head unit's own 0x0072 poll cadence drives this call, so that poll IS
// the stream pacing: pop strictly FIFO, one AU per poll, no internal
// re-pacing. Re-sending lastAU to keep cadence is only safe when that AU is
// an IDR (all-intra streams, where a repeat decodes to the identical
// picture) — repeating a predicted frame applies its motion residual twice
// and smears the decoded picture, so predictive streams answer an empty
// ring with idle and let the decoder hold its last picture.
func (s *LiveStreamSource) NextFrame(now time.Time) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Active checks
	if !s.active {
		return nil, nil
	}
	if !s.lastInput.IsZero() && now.Sub(s.lastInput) > s.liveTimeout {
		s.resetLocked()
		return nil, nil
	}

	// Pop the next live AU
	if s.count > 0 {
		au := s.aus[s.head]
		s.dropHeadLocked()

		s.lastAU = au
		s.lastAdvance = now
		return au, nil
	}

	// No queued frames? Repeating is only decode-safe for an IDR - or for an opaque
	// payload, where every frame is a whole picture by definition. That repeat is what
	// keeps a dash polling at 30 Hz fed from a source producing ten stills a second.
	if s.lastAU != nil && (s.opaque || hasAnnexBNALType(s.lastAU, 5)) {
		s.lastAdvance = now
		return s.lastAU, nil
	}
	return nil, nil
}

// extractAusFromStream scans `buf` for AUD-delimited AUs in Annex B format
// and returns (completeAUs, remainingBytes).
//
// It assumes NALs are prefixed by 0x000001 or 0x00000001, and treats
// each AUD (nal_unit_type == 9) as the start of an AU.
// Everything from one AUD start to the next AUD start is an AU.
func extractAusFromStream(buf []byte) ([][]byte, []byte) {
	const nalAud = 9

	var audStarts []int

	// Find all start codes followed by AUD NAL
	i := 0
	for i+3 < len(buf) {
		// 3-byte start code: 0x000001
		if buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 1 {
			nalHeaderIdx := i + 3
			if nalHeaderIdx < len(buf) {
				nalType := buf[nalHeaderIdx] & 0x1F
				if nalType == nalAud {
					audStarts = append(audStarts, i)
				}
			}
			i += 3
			continue
		}

		// 4-byte start code: 0x00000001
		if i+4 < len(buf) &&
			buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 0 && buf[i+3] == 1 {
			nalHeaderIdx := i + 4 // first byte after start code
			if nalHeaderIdx < len(buf) {
				nalType := buf[nalHeaderIdx] & 0x1F
				if nalType == nalAud {
					audStarts = append(audStarts, i)
				}
			}
			i += 4
			continue
		}

		i++
	}

	// No AUD at all: nothing we can form into AUs yet.
	if len(audStarts) == 0 {
		return nil, buf
	}

	// Only one AUD: treat everything from that AUD onward as "maybe incomplete".
	if len(audStarts) == 1 {
		return nil, buf[audStarts[0]:]
	}

	// Build complete AUs between successive AUDs: [AUD_i, AUD_{i+1})
	aus := make([][]byte, 0, len(audStarts)-1)
	for j := 0; j < len(audStarts)-1; j++ {
		start := audStarts[j]
		end := audStarts[j+1]
		if start < end && end <= len(buf) {
			// Copy out so caller can safely reuse original buffer.
			auCopy := append([]byte(nil), buf[start:end]...)
			aus = append(aus, auCopy)
		}
	}

	// Remaining data from last AUD to end (possibly incomplete AU).
	remaining := append([]byte(nil), buf[audStarts[len(audStarts)-1]:]...)

	return aus, remaining
}

// feedStreamToLiveSource reads io buffer data and feeds it to the LiveStreamSource in chunks
func feedStreamToLiveSource(r io.Reader, src *LiveStreamSource) error {
	const chunkSize = 4096
	buf := make([]byte, chunkSize)
	var leftover []byte

	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append(leftover, buf[:n]...)
			aus, rem := extractAusFromStream(chunk)
			leftover = rem

			for _, au := range aus {
				src.PushFrame(au)
			}
		}

		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

type MuxSource struct {
	stopAll  atomic.Bool
	Live     LiveSource
	NoSignal FrameSource
}

func (m *MuxSource) NextFrame(now time.Time) ([]byte, error) {
	if m.stopAll.Load() {
		return nil, nil
	}

	// Prefer live if it's active
	if m.Live != nil && m.Live.IsActive(now) {
		au, err := m.Live.NextFrame(now)
		if err != nil {
			return nil, err
		}
		return au, nil
	}

	// No live available switch to static
	if m.NoSignal != nil {
		return m.NoSignal.NextFrame(now)
	}
	return nil, nil
}

func (m *MuxSource) StopAllFrames() {
	m.stopAll.Store(true)
}

func (m *MuxSource) PrepareLiveConsumer() {
	if m.Live != nil {
		m.Live.PrepareForConsumer()
	}
}

// SetLiveOpaque switches the live queue between H.264 access units and whole JPEG stills, for a
// dash that negotiated JPEG mid-session rather than through the host's JpegStills experiment.
func (m *MuxSource) SetLiveOpaque(enabled bool) {
	if m.Live != nil {
		m.Live.SetOpaquePayloads(enabled)
	}
}

// SetLivePollMode deepens the live H.264 queue for a dash that polls for each frame (wantFps=0,
// no movie mode), or returns it to its own depth for one that is pushed or grants frames.
func (m *MuxSource) SetLivePollMode(enabled bool) {
	if m.Live != nil {
		m.Live.SetPollMode(enabled)
	}
}
