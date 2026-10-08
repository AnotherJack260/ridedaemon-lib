package stream

import (
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
