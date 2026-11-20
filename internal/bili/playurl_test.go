package bili

import "testing"

func TestPickAcceptsLowerResolutionWithRequestedCodec(t *testing.T) {
	info := PlayInfo{Dash: &Dash{
		Video: []DashStream{
			{ID: 32, CodecID: CodecAVC, Height: 480},
			{ID: 80, CodecID: CodecHEVC, Height: 1080},
		},
		Audio: []DashStream{{ID: 30280, Codecs: "mp4a.40.2"}},
	}}
	prefs := Prefs{VideoCodec: CodecAVC}
	video, _, err := info.Pick(prefs)
	if err != nil {
		t.Fatal(err)
	}
	if video.Height != 480 || video.CodecID != CodecAVC {
		t.Fatalf("selected height %d and codec %d, want 480p AVC", video.Height, video.CodecID)
	}
}
