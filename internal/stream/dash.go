package stream

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"bilibridge/internal/mp4"
)

func (s *Server) serveDASH(w http.ResponseWriter, r *http.Request, f *File, id, name string) {
	var manifest bytes.Buffer
	manifest.WriteString(xml.Header)
	fmt.Fprintf(&manifest, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-on-demand:2011" type="static" minBufferTime="PT1.5S" mediaPresentationDuration="PT%.3fS"><Period>`,
		max(f.Layout.Duration, f.audio.Layout.Duration))
	writeDASHTrack(&manifest, f.Layout, "video", s.base+"/v/"+id+"/"+url.PathEscape(name)+".mp4")
	writeDASHTrack(&manifest, f.audio.Layout, "audio", s.base+"/audio/"+id+"/"+url.PathEscape(name)+".m4a")
	manifest.WriteString("</Period></MPD>")
	w.Header().Set("Content-Type", "application/dash+xml")
	http.ServeContent(w, r, name+".mpd", time.Time{}, bytes.NewReader(manifest.Bytes()))
}

func writeDASHTrack(manifest *bytes.Buffer, layout *mp4.Layout, kind, mediaURL string) {
	bandwidth := max(1, int64(float64(layout.Size)*8/layout.Duration))
	fmt.Fprintf(manifest, `<AdaptationSet contentType="%s" mimeType="%s/mp4"><Representation id="%s" bandwidth="%d"><BaseURL>`,
		kind, kind, kind, bandwidth)
	xml.EscapeText(manifest, []byte(mediaURL))
	fmt.Fprintf(manifest, `</BaseURL><SegmentBase indexRange="%d-%d" indexRangeExact="true"><Initialization range="0-%d"/></SegmentBase></Representation></AdaptationSet>`,
		layout.InitSize, len(layout.Header)-1, layout.InitSize-1)
}
