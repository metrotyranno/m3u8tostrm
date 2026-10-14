package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const playlist = `#EXTM3U
#EXTINF:-1 tvg-id="ex1" tvg-name="Example Channel" group-title="News | Local",Example Channel HD
http://host/user/pass/101.ts
#EXTINF:-1 tvg-name="Example Movie, 1995" group-title="Movies: Action",Example Movie (1995)
http://host/movie/user/pass/202.mkv
#EXTINF:-1 group-title="Drama",Example Show S01 E02
http://host/series/user/pass/303.mkv
#EXTINF:-1 group-title="Drama",Example Show S01 E02
http://host/series/user/pass/304.mkv
`

var allTypes = []string{"movies", "series", "live"}

func TestTypes(t *testing.T) {
	dir := t.TempDir()
	pl := filepath.Join(dir, "list.m3u")
	out := filepath.Join(dir, "out")
	os.WriteFile(pl, []byte(playlist), 0o644)
	c := &Config{Playlists: []string{pl}, Output: out, Workers: 2, NewGroups: "subscribe",
		groupsPath: filepath.Join(dir, "groups.json"), staleAfter: 1000 * time.Hour, Types: allTypes}
	if err := syncOnce(c); err != nil {
		t.Fatal(err)
	}
	// Turning live off removes its files on the next sync, despite the long grace period.
	c.Types = []string{"movies", "series"}
	if err := syncOnce(c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "Live")); !os.IsNotExist(err) {
		t.Error("live files kept after turning live off")
	}
	if _, err := os.Stat(filepath.Join(out, "Movies", "Example Movie (1995)")); err != nil {
		t.Error("movies wrongly removed")
	}
}

func TestSyncAndPurge(t *testing.T) {
	dir := t.TempDir()
	pl := filepath.Join(dir, "list.m3u")
	out := filepath.Join(dir, "out")
	os.WriteFile(pl, []byte(playlist), 0o644)
	c := &Config{Playlists: []string{pl}, Output: out, Workers: 4, Types: allTypes, NewGroups: "subscribe", groupsPath: filepath.Join(dir, "groups.json")}

	if err := syncOnce(c); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Live/Example Channel HD.strm":                                    "http://host/user/pass/101.ts",
		"Movies/Example Movie (1995)/Example Movie (1995).strm":           "http://host/movie/user/pass/202.mkv",
		"Series/Example Show/Season 01/Example Show S01 E02.strm":         "http://host/series/user/pass/303.mkv",
		"Series/Example Show/Season 01/Example Show S01 E02 - Drama.strm": "http://host/series/user/pass/304.mkv",
	}
	for rel, url := range want {
		b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
		if err != nil || string(b) != url {
			t.Errorf("%s: got %q, %v", rel, b, err)
		}
	}

	// Drop the movie; with stale_after 0 it must be purged along with its empty folders.
	os.WriteFile(pl, []byte(playlist[:len("#EXTM3U\n")]+
		"#EXTINF:-1 group-title=\"News | Local\",Example Channel HD\nhttp://host/user/pass/101.ts\n"), 0o644)
	if err := syncOnce(c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "Movies")); !os.IsNotExist(err) {
		t.Error("stale movie folder not purged")
	}
	if _, err := os.Stat(filepath.Join(out, "Live", "Example Channel HD.strm")); err != nil {
		t.Error("live entry wrongly purged")
	}

	// A failed source must not purge anything.
	os.Remove(pl)
	syncOnce(c)
	if _, err := os.Stat(filepath.Join(out, "Live", "Example Channel HD.strm")); err != nil {
		t.Error("purged after failed fetch")
	}
}

func TestGroupSelection(t *testing.T) {
	dir := t.TempDir()
	pl := filepath.Join(dir, "list.m3u")
	out := filepath.Join(dir, "out")
	gp := filepath.Join(dir, "groups.json")
	os.WriteFile(pl, []byte(playlist), 0o644)
	// Long grace period: unticked groups must still go immediately.
	c := &Config{Playlists: []string{pl}, Output: out, Workers: 4, Types: allTypes, NewGroups: "ignore", groupsPath: gp, staleAfter: 1000 * time.Hour}
	Groups{"movies": {"Movies: Action": true}, "series": {"Drama": true}}.save(gp)

	if err := syncOnce(c); err != nil {
		t.Fatal(err)
	}
	movie := filepath.Join(out, "Movies", "Example Movie (1995)", "Example Movie (1995).strm")
	if _, err := os.Stat(movie); err != nil {
		t.Fatal("ticked group not written")
	}
	if _, err := os.Stat(filepath.Join(out, "Live")); !os.IsNotExist(err) {
		t.Error("new group written despite new_groups=ignore")
	}
	g, _ := loadGroups(gp)
	if v, ok := g["live"]["News | Local"]; !ok || v {
		t.Errorf("new group not recorded as unticked: %v", g)
	}

	g["movies"]["Movies: Action"] = false
	g.save(gp)
	if err := syncOnce(c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(movie); !os.IsNotExist(err) {
		t.Error("unticked group not purged on next sync")
	}
	if _, err := os.Stat(filepath.Join(out, "Series", "Example Show")); err != nil {
		t.Error("ticked series group wrongly purged")
	}
}

func TestVersionsAndExcludes(t *testing.T) {
	dir := t.TempDir()
	pl := filepath.Join(dir, "list.m3u")
	out := filepath.Join(dir, "out")
	os.WriteFile(pl, []byte(`#EXTM3U
#EXTINF:-1 group-title="Family",Sample Film (2019)
http://h/movie/u/p/2001.mkv
#EXTINF:-1 group-title="Kids Dubbed",Sample Film (2019)
http://h/movie/u/p/2002.mkv
#EXTINF:-1 group-title="Recently Added",Sample Film (2019)
http://h/movie/u/p/2001.mkv
#EXTINF:-1 group-title="New Releases 2023",Sample Event 46: Alpha vs. Beta (2023)
http://h/movie/u/p/555.mkv
`), 0o644)
	c := &Config{Playlists: []string{pl}, Output: out, Workers: 2, Types: allTypes, NewGroups: "subscribe",
		groupsPath: filepath.Join(dir, "groups.json"), staleAfter: 1000 * time.Hour}
	if err := syncOnce(c); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(out, "Movies", "*", "*.strm"))
	if len(files) != 3 { // same URL in two groups = one file; dub kept with a label
		t.Fatalf("want 3 movies, got %v", files)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "Movies", "Sample Film (2019)", "Sample Film (2019) - Kids Dubbed.strm")); string(b) != "http://h/movie/u/p/2002.mkv" {
		t.Errorf("dub not labelled: %q", b)
	}

	// Excluding a title word removes it on the next sync despite the long grace period.
	c.ExcludeTitles = []string{"sample event"}
	if err := syncOnce(c); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(out, "Movies", "Sample Event*")); len(files) != 0 {
		t.Errorf("excluded title still there: %v", files)
	}
}

func TestFillConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	url := "http://example.com/get.php?username=a&password=b&type=m3u_plus"
	os.WriteFile(p, []byte(fillConfig(url, `my "media"`)), 0o600)
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Playlists[0] != url || c.Output != filepath.Join(filepath.Dir(p), `my "media"`) {
		t.Errorf("got %v %q", c.Playlists, c.Output)
	}
}

func TestSetConfigKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	url := "http://example.com/get.php?username=a&password=b"
	os.WriteFile(p, []byte(fillConfig(url, "strm")), 0o600)
	if err := setConfigKeys(p, map[string]any{"exclude_titles": []string{"Sample Event"}, "interval": "12h"}); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Playlists[0] != url || c.interval != 12*time.Hour || !c.excluded("sample event 300") || c.excluded("Example Movie") {
		t.Errorf("got %+v", c)
	}
}

func TestMoviePaths(t *testing.T) {
	for name, want := range map[string]string{
		"Example Movie (1995)":           "Movies/Example Movie (1995)/Example Movie (1995).strm",
		"Example Movie (1995) 4K HDR":    "Movies/Example Movie (1995)/Example Movie (1995) - 4K HDR.strm",
		"Example Movie (1995) (Dubbed)":  "Movies/Example Movie (1995)/Example Movie (1995) - (Dubbed).strm",
		"Example (Long) Movie (1995) 4K": "Movies/Example (Long) Movie (1995)/Example (Long) Movie (1995) - 4K.strm",
		"Example Movie (4K)":             "Movies/Example Movie (4K)/Example Movie (4K).strm",
	} {
		if got := filepath.ToSlash(relPath(entry{name: name, url: "http://h/movie/u/p/1.mkv"})); got != want {
			t.Errorf("%q: got %s, want %s", name, got, want)
		}
	}
}

func TestSeriesPaths(t *testing.T) {
	for name, want := range map[string]string{
		"Example Show S01 E02":          "Series/Example Show/Season 01/Example Show S01 E02.strm",
		"Example Yearly Show S2025 E01": "Series/Example Yearly Show/Season 2025/Example Yearly Show S2025 E01.strm",
		"Example Odd Show S20232 E05":   "Series/Example Odd Show/Season 20232/Example Odd Show S20232 E05.strm",
		"No Episode Number Here":        "Series/No Episode Number Here.strm",
	} {
		if got := filepath.ToSlash(relPath(entry{name: name, url: "http://h/series/u/p/1.mkv"})); got != want {
			t.Errorf("%q: got %s, want %s", name, got, want)
		}
	}
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{"a/b:c?": "a_b_c_", " x. ": "x", "CON": "_CON", "": "_"} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}
