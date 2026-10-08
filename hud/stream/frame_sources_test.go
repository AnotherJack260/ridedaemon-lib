package stream

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func TestLiveSourceWaitsForIDR(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 3)
	pFrame := annexBAU(1)
	idrFrame := annexBAU(5)

	source.PushFrame(pFrame)
	if frame, _ := source.NextFrame(time.Now()); frame != nil {
		t.Fatal("predictive frame was emitted before the first IDR")
	}

	source.PushFrame(idrFrame)
	frame, err := source.NextFrame(time.Now())
	if err != nil {
		t.Fatalf("read IDR: %v", err)
	}
	if !hasAnnexBNALType(frame, 5) {
		t.Fatal("first emitted frame is not an IDR")
	}
}

func TestPrepareForConsumerDropsQueuedFramesAndWaitsForFreshIDR(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 3)
	source.PushFrame(annexBAU(5))
	source.PushFrame(annexBAU(1))
	source.PrepareForConsumer()

	if frame, _ := source.NextFrame(time.Now()); frame != nil {
		t.Fatal("stale frame remained queued after consumer reset")
	}
	source.PushFrame(annexBAU(1))
	if frame, _ := source.NextFrame(time.Now()); frame != nil {
		t.Fatal("predictive frame was emitted while waiting for a fresh IDR")
	}
	source.PushFrame(annexBAU(5))
	if frame, _ := source.NextFrame(time.Now()); !hasAnnexBNALType(frame, 5) {
		t.Fatal("fresh IDR was not emitted after consumer reset")
	}
}

// A JPEG carries no NAL of any type, so the IDR gate the constructor arms would drop every
// still ever pushed - in silence, since a dropped frame is not an error anywhere. This is the
// single failure mode that would make the whole experiment report "JPEG does not work" without
// one JPEG having reached the dash.
func TestOpaqueSourceAcceptsPayloadsThatAreNotAccessUnits(t *testing.T) {
	source := NewLiveStreamSource(10, 3*time.Second, 3)
	source.SetOpaquePayloads(true)

	still := jpegStill(0xAA)
	source.PushFrame(still)

	frame, err := source.NextFrame(time.Now())
	if err != nil {
		t.Fatalf("read still: %v", err)
	}
	if len(frame) != len(still) || frame[0] != 0xFF || frame[len(frame)-1] != 0xAA {
		t.Fatalf("still was not forwarded byte-for-byte: %v", frame)
	}
}

// STREAM_START calls PrepareLiveConsumer, which re-arms the IDR wait. On an opaque source that
// would kill the stream at the exact moment the dash starts pulling - the one place where a
// silent drop looks identical to the black screen the experiment is trying to explain.
func TestOpaqueSourceStaysOpaqueAcrossAConsumerReset(t *testing.T) {
	source := NewLiveStreamSource(10, 3*time.Second, 3)
	source.SetOpaquePayloads(true)
	source.PushFrame(jpegStill(0x01))
	source.PrepareForConsumer()

	source.PushFrame(jpegStill(0x02))
	frame, _ := source.NextFrame(time.Now())
	if frame == nil {
		t.Fatal("a still pushed after the consumer reset was dropped")
	}
	if frame[len(frame)-1] != 0x02 {
		t.Fatalf("the stale still survived the reset: %v", frame)
	}
}

// The dash polls at ~30 Hz while the still source produces ten frames a second, so two polls
// in three find the queue empty. Repeating the last still is what keeps the panel fed; the
// H.264 rule that only an IDR may be repeated has no meaning for a whole picture.
func TestOpaqueSourceRepeatsTheLastStillWhenTheQueueIsEmpty(t *testing.T) {
	source := NewLiveStreamSource(10, 3*time.Second, 3)
	source.SetOpaquePayloads(true)
	source.PushFrame(jpegStill(0x07))

	now := time.Now()
	if frame, _ := source.NextFrame(now); frame == nil {
		t.Fatal("the first still was not emitted")
	}
	repeat, _ := source.NextFrame(now)
	if repeat == nil || repeat[len(repeat)-1] != 0x07 {
		t.Fatalf("an empty queue did not repeat the last still: %v", repeat)
	}
}

// The default must not move: an H.264 session that never asked for stills has to keep dropping
// predictive frames until the first IDR, or every dashboard in the fleet changes behaviour.
func TestSourceKeepsTheIDRGateWhenOpaqueIsOff(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 3)
	source.SetOpaquePayloads(false)

	source.PushFrame(annexBAU(1))
	if frame, _ := source.NextFrame(time.Now()); frame != nil {
		t.Fatal("predictive frame was emitted before the first IDR")
	}
}

// A JPEG: SOI, a byte of payload, EOI, then a marker byte the assertions can follow.
func jpegStill(marker byte) []byte {
	return []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0xFF, 0xD9, marker}
}

func annexBAU(nalType byte) []byte {
	return []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 0, 1, nalType, 1, 2, 3}
}

// taggedAU is an access unit of the given NAL type whose last byte names it.
func taggedAU(nalType, tag byte) []byte {
	return []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 0, 1, nalType, 1, 2, tag}
}

const idr, pFrame = 5, 1

// When the queue must drop, it drops up to the next IDR so whatever goes out next decodes;
// dropping one P-frame smeared the picture until the next IDR. JPEG stills still drop the oldest.
func TestAnOverflowingQueueDropsUpToAnIDR(t *testing.T) {
	type push struct{ nal, tag byte }
	for _, tc := range []struct {
		name       string
		capacity   int
		opaque     bool
		pushes     []push
		wantTags   []byte
		wantAskFor []int
	}{
		{
			name:     "an IDR is queued: drop the frames ahead of it",
			capacity: 4,
			pushes:   []push{{idr, 1}, {pFrame, 2}, {pFrame, 3}, {idr, 4}, {pFrame, 5}},
			wantTags: []byte{4, 5},
		},
		{
			name:       "no IDR queued: wait for one and ask for it",
			capacity:   3,
			pushes:     []push{{idr, 1}, {pFrame, 2}, {pFrame, 3}, {pFrame, 4}, {pFrame, 5}, {idr, 6}, {pFrame, 7}},
			wantTags:   []byte{6, 7},
			wantAskFor: []int{4},
		},
		{
			name:     "the frame arriving is the IDR: nothing to ask for",
			capacity: 3,
			pushes:   []push{{idr, 1}, {pFrame, 2}, {pFrame, 3}, {idr, 4}},
			wantTags: []byte{4},
		},
		{
			name:     "JPEG stills drop the oldest",
			capacity: 2,
			opaque:   true,
			pushes:   []push{{0, 1}, {0, 2}, {0, 3}},
			wantTags: []byte{2, 3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := NewLiveStreamSource(30, 3*time.Second, tc.capacity)
			source.SetOpaquePayloads(tc.opaque)
			var asked []int
			source.OnKeyframeNeeded = func(dropped int) { asked = append(asked, dropped) }
			for _, p := range tc.pushes {
				if tc.opaque {
					source.PushFrame(jpegStill(p.tag))
				} else {
					source.PushFrame(taggedAU(p.nal, p.tag))
				}
			}
			var tags []byte
			for i := 0; i < tc.capacity; i++ {
				frame, _ := source.NextFrame(time.Now())
				if frame == nil || (len(tags) > 0 && frame[len(frame)-1] == tags[len(tags)-1]) {
					break // empty, or an IDR repeated for cadence
				}
				tags = append(tags, frame[len(frame)-1])
			}
			if string(tags) != string(tc.wantTags) {
				t.Fatalf("sent %v, want %v", tags, tc.wantTags)
			}
			if fmt.Sprint(asked) != fmt.Sprint(tc.wantAskFor) {
				t.Fatalf("asked for a keyframe %v, want %v", asked, tc.wantAskFor)
			}
		})
	}
}

// One ask a second: a dash that stops polling overflows the queue every few frames, and an IDR
// requested every 200 ms would be most of the bitrate.
func TestKeyframeAsksAreRateLimited(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 2)
	asks := 0
	source.OnKeyframeNeeded = func(int) { asks++ }
	for round := 0; round < 3; round++ {
		source.PushFrame(annexBAU(idr))
		source.PushFrame(annexBAU(pFrame))
		source.PushFrame(annexBAU(pFrame)) // overflow, nothing decodable left
	}
	if asks != 1 {
		t.Fatalf("asked %d times in a burst, want 1", asks)
	}
}

// A suppressed ask is not the last one: while the IDR is still owed, the next push after the
// second is up asks again, or the picture would hold until the GOP came round.
func TestASuppressedKeyframeAskIsRepeatedWhileTheIDRIsOwed(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 2)
	var asked []int
	source.OnKeyframeNeeded = func(dropped int) { asked = append(asked, dropped) }
	source.PushFrame(annexBAU(idr))
	source.PushFrame(annexBAU(pFrame))
	source.PushFrame(annexBAU(pFrame)) // overflow: ask
	source.PushFrame(annexBAU(idr))    // answered
	source.PushFrame(annexBAU(pFrame))
	source.PushFrame(annexBAU(pFrame)) // overflow again within the second: suppressed
	if len(asked) != 1 {
		t.Fatalf("asks = %v, want one so far", asked)
	}
	source.mu.Lock()
	source.lastKeyframeAsk = time.Now().Add(-2 * time.Second)
	source.mu.Unlock()
	source.PushFrame(annexBAU(pFrame)) // still owed, a second later: ask again
	if len(asked) != 2 || asked[1] != 4 {
		t.Fatalf("asks = %v, want a second ask covering the 4 AUs dropped since", asked)
	}
	source.PushFrame(annexBAU(idr)) // answered: nothing owed any more
	source.mu.Lock()
	source.lastKeyframeAsk = time.Time{}
	source.mu.Unlock()
	source.PushFrame(annexBAU(pFrame))
	if len(asked) != 2 {
		t.Fatalf("asked again after the IDR arrived: %v", asked)
	}
}

// A capture codec change mid-session (core's OnCaptureConfig) drops what the old codec left queued
// or kept for a repeat, and a switch back to H.264 waits for an IDR again; the same codec again
// changes nothing.
func TestACodecChangeDropsTheOldCodecsFrames(t *testing.T) {
	live := NewLiveStreamSource(30, 3*time.Second, 6)
	idr := []byte{0, 0, 0, 1, 0x65, 0x88}
	p := []byte{0, 0, 0, 1, 0x41, 0x9A}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	next := func() []byte {
		frame, _ := live.NextFrame(time.Now())
		return frame
	}

	live.PushFrame(idr)
	live.PushFrame(p)
	live.SetOpaquePayloads(false) // still H.264
	if frame := next(); !bytes.Equal(frame, idr) {
		t.Fatalf("re-stating the codec dropped the queue: %x", frame)
	}

	live.SetOpaquePayloads(true)
	if frame := next(); frame != nil {
		t.Fatalf("an H.264 frame went to a dash now expecting JPEG: %x", frame)
	}
	live.PushFrame(jpeg)
	if frame := next(); !bytes.Equal(frame, jpeg) {
		t.Fatalf("the first still = %x", frame)
	}

	live.SetOpaquePayloads(false)
	if frame := next(); frame != nil {
		t.Fatalf("a still was repeated to a dash now expecting H.264: %x", frame)
	}
	live.PushFrame(p)
	if frame := next(); frame != nil {
		t.Fatalf("H.264 restarted on a P-frame: %x", frame)
	}
	live.PushFrame(idr)
	if frame := next(); !bytes.Equal(frame, idr) {
		t.Fatalf("H.264 did not restart on the IDR: %x", frame)
	}
}

// The Zontes 350D polls at ~35/s for a 30 fps stream and now and then stops for a few hundred ms.
// A 500 ms pause is 15 frames: the 6-AU queue overflowed on it, dropped 7 and froze the picture
// until an IDR. In poll mode every frame now reaches the dash, in order, and nothing is asked for.
func TestAPollingDashThatPausesHalfASecondMissesNothing(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 6)
	source.SetPollMode(true)
	asked := 0
	source.OnKeyframeNeeded = func(int) { asked++ }

	// Virtual time in µs: a frame every 1/30 s (an IDR every 3 s), a poll every 1/35 s, none
	// between 1.0 s and 1.5 s; the producer stops at 3 s and the dash pulls what is left.
	const frames = 90
	var got []byte
	pull := func() {
		frame, _ := source.NextFrame(time.Now())
		if frame != nil && (len(got) == 0 || frame[len(frame)-1] != got[len(got)-1]) { // not an IDR repeat
			got = append(got, frame[len(frame)-1])
		}
	}
	nextPoll := 0
	for k := 0; k < frames; k++ {
		at := k * 1000000 / 30
		for ; nextPoll*1000000/35 < at; nextPoll++ {
			if t := nextPoll * 1000000 / 35; t < 1000000 || t >= 1500000 {
				pull()
			}
		}
		nal := byte(pFrame)
		if k%90 == 0 {
			nal = idr
		}
		source.PushFrame(taggedAU(nal, byte(k)))
	}
	for i := 0; i < pollQueueAUs; i++ {
		pull()
	}

	if len(got) != frames {
		t.Fatalf("the dash got %d of %d frames: %v", len(got), frames, got)
	}
	for i, tag := range got {
		if tag != byte(i) {
			t.Fatalf("frame %d out of order: %v", i, got)
		}
	}
	if asked != 0 {
		t.Fatalf("asked for %d keyframe(s) with nothing dropped", asked)
	}
}

// A dash that stops polling for good still ends in drop-to-IDR: the 33rd frame it never pulls
// overflows the queue, everything goes, and the host is asked for an IDR.
func TestAPollingDashThatStopsStillDropsToAnIDR(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 6)
	source.SetPollMode(true)
	var asked []int
	source.OnKeyframeNeeded = func(dropped int) { asked = append(asked, dropped) }
	source.PushFrame(taggedAU(idr, 0))
	source.NextFrame(time.Now())

	for k := 1; k <= pollQueueAUs; k++ {
		source.PushFrame(taggedAU(pFrame, byte(k)))
	}
	if len(asked) != 0 {
		t.Fatalf("asked %v before the queue was full", asked)
	}
	source.PushFrame(taggedAU(pFrame, pollQueueAUs+1))
	if fmt.Sprint(asked) != fmt.Sprint([]int{pollQueueAUs + 1}) {
		t.Fatalf("asked %v, want one ask for %d AUs", asked, pollQueueAUs+1)
	}

	source.PushFrame(taggedAU(pFrame, 100)) // still waiting for the IDR
	source.PushFrame(taggedAU(idr, 101))
	if frame, _ := source.NextFrame(time.Now()); frame == nil || frame[len(frame)-1] != 101 {
		t.Fatalf("after the drop the dash got %v, want the new IDR", frame)
	}
}

// In poll mode the queue never holds more than pollQueueBytes, whatever the AU count: a push that
// would pass it drops to an IDR like a full queue. An AU bigger than the cap still goes out alone.
func TestThePollQueueHoldsItsByteCap(t *testing.T) {
	source := NewLiveStreamSource(30, 3*time.Second, 6)
	source.SetPollMode(true)
	asked := 0
	source.OnKeyframeNeeded = func(int) { asked++ }
	big := func(size int, nal, tag byte) []byte {
		return append(bytes.Repeat([]byte{0xAA}, size), taggedAU(nal, tag)...)
	}
	quarter := pollQueueBytes/4 - 100 // four fit, five do not
	queued := func(want string) {
		t.Helper()
		source.mu.Lock()
		defer source.mu.Unlock()
		var tags []byte
		held := 0
		for i := 0; i < source.count; i++ {
			au := source.aus[(source.head+i)%source.capacity]
			held += len(au)
			tags = append(tags, au[len(au)-1])
		}
		if held != source.bytes {
			t.Fatalf("queue holds %d bytes but counts %d", held, source.bytes)
		}
		if held > pollQueueBytes && len(tags) > 1 {
			t.Fatalf("queue holds %d bytes over %d AUs, cap %d", held, len(tags), pollQueueBytes)
		}
		if fmt.Sprint(tags) != want {
			t.Fatalf("queued %v, want %s", tags, want)
		}
	}

	source.PushFrame(big(quarter, idr, 1))
	source.PushFrame(big(quarter, pFrame, 2))
	source.PushFrame(big(quarter, idr, 3))
	source.PushFrame(big(quarter, pFrame, 4))
	queued("[1 2 3 4]")
	source.PushFrame(big(quarter, pFrame, 5)) // over the cap: drop to the queued IDR
	queued("[3 4 5]")
	source.PushFrame(big(quarter, pFrame, 6))
	source.PushFrame(big(quarter, pFrame, 7)) // over again, no IDR behind the head: drop it all
	queued("[]")
	if asked != 1 {
		t.Fatalf("asked %d times, want 1", asked)
	}
	source.PushFrame(big(pollQueueBytes, idr, 8))
	queued("[8]")
	if frame, _ := source.NextFrame(time.Now()); frame == nil || frame[len(frame)-1] != 8 {
		t.Fatal("an IDR bigger than the cap was not sent")
	}
	queued("[]")
}
