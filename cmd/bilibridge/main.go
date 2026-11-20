// Command bilibridge plays Bilibili videos in PotPlayer by serving their
// DASH video and audio streams over local HTTP.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"bilibridge/internal/bili"
	"bilibridge/internal/potplayer"
	"bilibridge/internal/stream"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "chrome-extension://") {
		if err := runNative(os.Args[1]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "-desktop" || len(os.Args) == 1 && strings.EqualFold(filepath.Base(os.Args[0]), "BiliBridgeDesktop.exe") {
		if err := runDesktop(); err != nil {
			showDesktopError(0, err.Error())
		}
		return
	}
	fs := flag.NewFlagSet("bilibridge", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:0", "address to serve the merged stream on")
	maxQ := fs.Int("q", 0, "highest quality id to use, e.g. 80 for 1080P (0 = best)")
	// Retain the old flag for existing shortcuts, with no effect on selection.
	fs.Int("min-height", 0, "deprecated; any source resolution can play")
	codecs := fs.String("codec", "avc,hevc,av1", "video codecs in order of preference")
	cookie := fs.String("cookie", "", "browser Cookie header, for logged-in qualities (defaults to BILI_COOKIE)")
	launch := fs.Bool("launch", true, "open the stream in PotPlayer")
	hls := fs.Bool("hls", false, "serve H.264/AAC as on-demand HLS using ffmpeg from PATH")
	prefetch := fs.Duration("prefetch", 60*time.Second, "prefetch window (0 disables prefetch)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bilibridge [flags] <BV id or video URL>")
		fs.PrintDefaults()
	}
	fs.Parse(os.Args[1:])
	cookieValue := os.Getenv("BILI_COOKIE")
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "cookie" {
			cookieValue = *cookie
		}
	})
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	if *prefetch < 0 {
		log.Fatal("-prefetch must be non-negative")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ffmpeg := ""
	prefs := bili.Prefs{MaxQuality: *maxQ, Codecs: parseCodecs(*codecs)}
	if *hls {
		prefs.VideoCodec = bili.CodecAVC
		var err error
		ffmpeg, err = exec.LookPath("ffmpeg")
		if err != nil {
			log.Fatalf("-hls requires ffmpeg on PATH: %v", err)
		}
	}
	if err := run(ctx, fs.Arg(0), *addr, cookieValue, ffmpeg, *launch, *prefetch, prefs); err != nil {
		log.Fatal(err)
	}
}

func parseCodecs(s string) []int {
	ids := map[string]int{"avc": bili.CodecAVC, "hevc": bili.CodecHEVC, "av1": bili.CodecAV1}
	var out []int
	for _, name := range strings.Split(s, ",") {
		if id, ok := ids[strings.TrimSpace(strings.ToLower(name))]; ok {
			out = append(out, id)
		}
	}
	return out
}

func run(ctx context.Context, ref, addr, cookie, ffmpeg string, launch bool, prefetch time.Duration, prefs bili.Prefs) error {
	srv, err := stream.Listen(addr)
	if err != nil {
		return err
	}
	m, err := prepare(ctx, srv, bili.NewClient(cookie), ref, ffmpeg, prefetch, prefs, false)
	if err != nil {
		return err
	}
	log.Printf("serving %s", m.URL)
	if launch {
		exe, err := potplayer.FindExe()
		if err != nil {
			return err
		}
		if err := potplayer.Launch(exe, m.URL); err != nil {
			return err
		}
		go watch(ctx)
	}
	<-ctx.Done()
	return nil
}

type preparedMedia struct {
	bili.Media
	URL      string
	AudioURL string
	Video    bili.DashStream
	Duration time.Duration
}

func prepare(ctx context.Context, srv *stream.Server, c *bili.Client, ref, ffmpeg string, prefetch time.Duration, prefs bili.Prefs, separateAudio bool) (preparedMedia, error) {
	media, err := c.Resolve(ctx, ref)
	if err != nil {
		return preparedMedia{}, err
	}
	info, err := c.MediaPlayURL(ctx, media)
	if err != nil {
		return preparedMedia{}, err
	}
	video, audio, err := info.Pick(prefs)
	if err != nil {
		return preparedMedia{}, err
	}
	if ffmpeg != "" && (video.CodecID != bili.CodecAVC || !strings.HasPrefix(audio.Codecs, "mp4a.40.")) {
		ffmpeg = ""
	}
	log.Print(media.Title)
	log.Printf("video: %s %s %dx%d @%s fps, %d kbps", bili.QualityNames[video.ID], video.Codecs, video.Width, video.Height, video.FrameRate, video.Bandwidth/1000)
	log.Printf("audio: %d %s, %d kbps", audio.ID, audio.Codecs, audio.Bandwidth/1000)

	// Expired URLs are replaced by asking for the same stream again.
	refresh := func(id, codec int, isAudio bool) func(context.Context) ([]string, error) {
		return func(ctx context.Context) ([]string, error) {
			fresh, err := c.MediaPlayURL(ctx, media)
			if err != nil {
				return nil, err
			}
			list := fresh.Dash.Video
			if isAudio {
				list = fresh.Dash.Audio
				if fresh.Dash.Flac != nil && fresh.Dash.Flac.Audio != nil {
					list = append(list, *fresh.Dash.Flac.Audio)
				}
			}
			for _, s := range list {
				if s.ID == id && (isAudio || s.CodecID == codec) {
					return s.URLs(), nil
				}
			}
			return nil, fmt.Errorf("stream %d is no longer offered", id)
		}
	}
	vEnd, err := video.IndexEnd()
	if err != nil {
		return preparedMedia{}, err
	}
	aEnd, err := audio.IndexEnd()
	if err != nil {
		return preparedMedia{}, err
	}
	header := bili.MediaHeader()
	vu := stream.NewUpstream("video", video.URLs(), header, refresh(video.ID, video.CodecID, false))
	au := stream.NewUpstream("audio", audio.URLs(), header, refresh(audio.ID, 0, true))

	t0 := time.Now()
	f, err := stream.NewFile(ctx, vu, au, vEnd, aEnd, separateAudio)
	if err != nil {
		return preparedMedia{}, err
	}
	l := f.Layout
	log.Printf("prepared %d segments, %.1f MB, %.0fs, header %d bytes (%v)", len(l.Segments), float64(l.Size)/(1<<20), l.Duration, len(l.Header), time.Since(t0).Round(time.Millisecond))

	var u string
	if ffmpeg == "" {
		u = srv.AddMP4(ctx, f, media.Title, prefetch)
	} else {
		u, err = srv.AddHLS(ctx, f, media.Title, ffmpeg, prefetch)
		if err != nil {
			return preparedMedia{}, err
		}
	}
	m := preparedMedia{Media: media, URL: u, Video: *video, Duration: time.Duration(l.Duration * float64(time.Second))}
	if separateAudio {
		m.AudioURL = srv.AudioURL(u)
	}
	return m, nil
}

// watch logs PotPlayer's state once a second.
func watch(ctx context.Context) {
	w, err := potplayer.WaitWindow(10 * time.Second)
	if err != nil {
		log.Print(err)
		return
	}
	log.Printf("PotPlayer window %#x, pid %d", uintptr(w), w.PID())
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		st, err1 := w.Status()
		pos, err2 := w.Position()
		dur, err3 := w.Duration()
		if err1 != nil || err2 != nil || err3 != nil {
			log.Printf("PotPlayer: %v %v %v", err1, err2, err3)
			if w = potplayer.FindWindow(); w == 0 {
				return
			}
			continue
		}
		log.Printf("PotPlayer: %s %v / %v", st, pos.Round(100*time.Millisecond), dur.Round(time.Second))
	}
}
