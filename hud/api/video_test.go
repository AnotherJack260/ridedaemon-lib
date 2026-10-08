package api

import "testing"

func TestCarbitEncoderPlan(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   CaptureConfig
		want EncoderPlan
	}{
		{
			// The Zontes 350D's CAPTURE_CONFIG.
			"zontes",
			CaptureConfig{Width: 1024, Height: 464, WantFps: 0, WantEncoder: 2, SupportCodec: 2, BitRate: 8388608},
			EncoderPlan{Codec: 2, Width: 1024, Height: 464, BitRate: 8388608, Fps: 36, IFrameSeconds: 3,
				FrameIntervalMs: 27, JpegMaxBytes: 25000, Landscape: true},
		},
		{
			"jpeg defaults, cbr, portrait",
			CaptureConfig{Width: 480, Height: 800, WantFps: 30, WantEncoder: 0, CapScreenMode: 8, Orientation: 2, MaxQuality: 40000},
			EncoderPlan{Codec: 1, Width: 480, Height: 800, BitRate: 3000000, Fps: 30, IFrameSeconds: 3, Cbr: true,
				FrameIntervalMs: 33, JpegMaxBytes: 40000},
		},
		{
			"unknown encoder, odd size, round",
			CaptureConfig{Width: 730, Height: 473, WantEncoder: 3, Orientation: 3},
			EncoderPlan{Codec: 0, Width: 736, Height: 480, BitRate: 3000000, Fps: 36, IFrameSeconds: 3,
				FrameIntervalMs: 27, JpegMaxBytes: 25000, Landscape: true, RoundScreen: true},
		},
	} {
		if got := CarbitEncoderPlan(&tc.in); *got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, *got, tc.want)
		}
	}
	if CarbitEncoderPlan(nil) != nil {
		t.Error("a plan for no config")
	}
}

func TestCaptureConfigIsNilBeforeTheDashSendsOne(t *testing.T) {
	session, err := NewMobileSession(NewMobileConfig(nil, 30, 10, 5, 10, 3), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.StopSession()
	session.SetCarbitExactMediaControl(true)
	if session.CaptureConfig() != nil {
		t.Fatal("a capture config before the dash sent one")
	}
}
