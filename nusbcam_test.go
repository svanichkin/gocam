package gocam

import (
	"fmt"
	"testing"
)

func TestParseNusbFormats(t *testing.T) {
	got := parseNusbFormats("640x480x16-YUY2 30.00,15.00\n" +
		"352x288x16-YUY2 30.00\n" +
		"1280x720x0-MJPG 30.00-5.00\n" +
		"garbage\n\n")
	want := []nusbFormat{
		{640, 480, 16, "YUY2"},
		{352, 288, 16, "YUY2"},
		{1280, 720, 0, "MJPG"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPickNusbFormat(t *testing.T) {
	f := func(w, h int, cc string) nusbFormat { return nusbFormat{w, h, 16, cc} }
	cases := []struct {
		in   []nusbFormat
		want string
		ok   bool
	}{
		{[]nusbFormat{f(1280, 720, "YUY2"), f(640, 480, "YUY2"), f(352, 288, "YUY2"), f(320, 240, "YUY2")}, "352x288x16-YUY2", true},
		{[]nusbFormat{f(160, 120, "YUY2"), f(1280, 720, "YUY2"), f(640, 480, "YUY2")}, "640x480x16-YUY2", true},
		{[]nusbFormat{f(160, 120, "YUY2"), f(320, 240, "YUY2")}, "320x240x16-YUY2", true},
		{[]nusbFormat{f(352, 288, "MJPG"), f(1280, 720, "YUY2")}, "1280x720x16-YUY2", true},
		{[]nusbFormat{f(640, 480, "MJPG")}, "", false},
	}
	for _, c := range cases {
		got, ok := pickNusbFormat(c.in)
		if ok != c.ok || (ok && got.String() != c.want) {
			t.Errorf("pickNusbFormat(%v) = %v, %v; want %s, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseImageHeader(t *testing.T) {
	h := []byte(fmt.Sprintf("%11s %11d %11d %11d %11d ", "b8g8r8", 0, 0, 640, 480))
	w, ht, pf, err := parseImageHeader(h)
	if err != nil || w != 640 || ht != 480 || pf != pixFmtRGB24 {
		t.Fatalf("got %d %d %v %v", w, ht, pf, err)
	}
	h = []byte(fmt.Sprintf("%11s %11d %11d %11d %11d ", "r8g8b8", 10, 20, 30, 40))
	if w, ht, pf, err = parseImageHeader(h); err != nil || w != 20 || ht != 20 || pf != pixFmtBGR24 {
		t.Fatalf("got %d %d %v %v", w, ht, pf, err)
	}
	for _, bad := range [][]byte{
		h[:30],
		[]byte(fmt.Sprintf("%11s %11d %11d %11d %11d ", "k8", 0, 0, 1, 1)),
		[]byte(fmt.Sprintf("%11s %11d %11d %11d %11d ", "b8g8r8", 0, 0, 0, 1)),
	} {
		if _, _, _, err := parseImageHeader(bad); err == nil {
			t.Errorf("parseImageHeader(%q): no error", bad)
		}
	}
}
