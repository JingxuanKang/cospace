package container

import (
	"errors"
	"testing"
)

func TestParsePullLine(t *testing.T) {
	cases := []struct {
		line string
		want PullProgress
		ok   bool
	}{
		{"[1/2] Fetching image [0s]", PullProgress{Phase: "fetching", Percent: -1}, true},
		{"[1/2] Fetching image 49% (36 of 52 blobs, 8.2/16.7 MB, 3.2 MB/s) [5s]", PullProgress{Phase: "fetching", Percent: 49, DoneMB: 8.2, TotalMB: 16.7}, true},
		{"[1/2] Fetching image 12% (3 of 9 blobs, 0.3/0.6 GB, 9 MB/s) [5s]", PullProgress{Phase: "fetching", Percent: 12, DoneMB: 0.3 * 1024, TotalMB: 0.6 * 1024}, true},
		{"[2/2] Unpacking image for platform linux/arm64 100% (448 of 448 entries, 4.0/4.0 MB, 98 KB/s) [11s]", PullProgress{Phase: "unpacking", Percent: 100, DoneMB: 4, TotalMB: 4}, true},
		{"Error: unauthorized", PullProgress{}, false},
	}
	for _, c := range cases {
		got, ok := ParsePullLine(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("ParsePullLine(%q) = %+v, %v; want %+v, %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestImageExists(t *testing.T) {
	c := Client{R: &fakeRunner{}}
	if ok, err := c.ImageExists("x"); !ok || err != nil {
		t.Fatalf("present image: %v %v", ok, err)
	}
	c = Client{R: &fakeRunner{err: errors.New("container image inspect x: exit status 1: Error: image not found: x")}}
	if ok, err := c.ImageExists("x"); ok || err != nil {
		t.Fatalf("missing image: %v %v", ok, err)
	}
	c = Client{R: &fakeRunner{err: errors.New("XPC connection error")}}
	if _, err := c.ImageExists("x"); err == nil {
		t.Fatal("runtime failure should surface as an error")
	}
}
