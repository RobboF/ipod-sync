package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---- sanitizeName ----

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"normal", "normal"},
		{"file:name", "file - name"},
		{"Artist: Name", "Artist -  Name"}, // colon → " - ", original space preserved
		{"a*b", "a_b"},
		{"a?b", "a_b"},
		{`a"b`, "a_b"},
		{"a<b", "a_b"},
		{"a>b", "a_b"},
		{"a|b", "a_b"},
		{`a\b`, "a_b"},
		{"a:b*c?d", "a - b_c_d"},
		{"no special", "no special"},
	}
	for _, tt := range tests {
		if got := sanitizeName(tt.in); got != tt.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---- sanitizeRelPath ----

func TestSanitizeRelPath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Artist/Album/track.mp3", "Artist/Album/track.mp3"},
		{"Artist: Name/Album/track.mp3", "Artist -  Name/Album/track.mp3"},
		{"Artist/Album:2024/track.mp3", "Artist/Album - 2024/track.mp3"},
		{"A*B/C?D/E.mp3", "A_B/C_D/E.mp3"},
	}
	for _, tt := range tests {
		if got := sanitizeRelPath(tt.in); got != tt.want {
			t.Errorf("sanitizeRelPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---- walkFollowSymlinks ----

func TestWalkFollowSymlinks_FollowsSymlinks(t *testing.T) {
	target := t.TempDir()
	os.WriteFile(filepath.Join(target, "file.txt"), []byte("hi"), 0o644)

	root := t.TempDir()
	os.Symlink(target, filepath.Join(root, "link"))

	var found bool
	walkFollowSymlinks(root, func(path string, info os.FileInfo) error {
		if !info.IsDir() && filepath.Base(path) == "file.txt" {
			found = true
		}
		return nil
	})
	if !found {
		t.Error("file.txt not reached through symlink")
	}
}

func TestWalkFollowSymlinks_NoCycles(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	os.Mkdir(sub, 0o755)
	// symlink back to root — would loop without cycle detection
	os.Symlink(root, filepath.Join(sub, "cycle"))

	calls := 0
	walkFollowSymlinks(root, func(_ string, _ os.FileInfo) error {
		calls++
		if calls > 100 {
			t.Fatal("walk did not terminate — likely infinite loop")
		}
		return nil
	})
}

// ---- copyFile ----

func TestCopyFile(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	srcPath := filepath.Join(src, "song.mp3")
	dstPath := filepath.Join(dst, "nested", "song.mp3") // intermediate dir must be created

	content := []byte("audio content here")
	mtime := time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)

	os.WriteFile(srcPath, content, 0o644)
	os.Chtimes(srcPath, mtime, mtime)
	srcInfo, _ := os.Stat(srcPath)

	if err := copyFile(srcPath, dstPath, srcInfo); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("reading dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}

	dstInfo, _ := os.Stat(dstPath)
	diff := dstInfo.ModTime().Sub(mtime)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Second {
		t.Errorf("mtime not preserved: got %v, want %v", dstInfo.ModTime(), mtime)
	}
}

// ---- findIPod ----

func TestFindIPod(t *testing.T) {
	base := t.TempDir()
	dev := filepath.Join(base, "1-2")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "idVendor"), []byte("05ac\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "idProduct"), []byte("1261\n"), 0o644)

	got, err := findIPod(base)
	if err != nil {
		t.Fatalf("findIPod: %v", err)
	}
	if got != dev {
		t.Errorf("got %q, want %q", got, dev)
	}
}

func TestFindIPod_WrongVendor(t *testing.T) {
	base := t.TempDir()
	dev := filepath.Join(base, "1-2")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "idVendor"), []byte("1234\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "idProduct"), []byte("1261\n"), 0o644)

	_, err := findIPod(base)
	if err == nil {
		t.Error("expected error for wrong vendor ID")
	}
}

func TestFindIPod_WrongProduct(t *testing.T) {
	base := t.TempDir()
	dev := filepath.Join(base, "1-2")
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "idVendor"), []byte("05ac\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "idProduct"), []byte("9999\n"), 0o644)

	_, err := findIPod(base)
	if err == nil {
		t.Error("expected error for wrong product ID")
	}
}

func TestFindIPod_MultipleDevices(t *testing.T) {
	base := t.TempDir()

	// Non-Apple device
	other := filepath.Join(base, "1-1")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "idVendor"), []byte("1234\n"), 0o644)

	// iPod
	ipod := filepath.Join(base, "1-2")
	os.MkdirAll(ipod, 0o755)
	os.WriteFile(filepath.Join(ipod, "idVendor"), []byte("05ac\n"), 0o644)
	os.WriteFile(filepath.Join(ipod, "idProduct"), []byte("1261\n"), 0o644)

	got, err := findIPod(base)
	if err != nil {
		t.Fatalf("findIPod: %v", err)
	}
	if got != ipod {
		t.Errorf("got %q, want %q", got, ipod)
	}
}

// ---- findBlockDevice ----

func TestFindBlockDevice(t *testing.T) {
	root := t.TempDir()
	blockDir := filepath.Join(root, "host0", "target0", "block")
	os.MkdirAll(filepath.Join(blockDir, "sda"), 0o755)

	disk, err := findBlockDevice(root)
	if err != nil {
		t.Fatalf("findBlockDevice: %v", err)
	}
	if disk != "sda" {
		t.Errorf("disk = %q, want sda", disk)
	}
}

func TestFindBlockDevice_ViaSymlink(t *testing.T) {
	real := t.TempDir()
	blockDir := filepath.Join(real, "host0", "block")
	os.MkdirAll(filepath.Join(blockDir, "sda"), 0o755)

	root := t.TempDir()
	// Simulate a sysfs symlink: root/device -> real
	os.Symlink(real, filepath.Join(root, "device"))

	disk, err := findBlockDevice(filepath.Join(root, "device"))
	if err != nil {
		t.Fatalf("findBlockDevice: %v", err)
	}
	if disk != "sda" {
		t.Errorf("disk = %q, want sda", disk)
	}
}

func TestFindBlockDevice_NotFound(t *testing.T) {
	root := t.TempDir()
	_, err := findBlockDevice(root)
	if err == nil {
		t.Error("expected error when no block dir exists")
	}
}

// ---- syncRenamed ----

func mustWriteFile(t *testing.T, path string, content []byte, mtime time.Time) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, mtime, mtime)
}

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestSyncRenamed_CopiesNew(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	mustWriteFile(t, filepath.Join(src, "song.mp3"), []byte("audio"), epoch)
	mustWriteFile(t, filepath.Join(src, "Artist", "Album", "track.flac"), []byte("flac"), epoch)

	stats, err := syncRenamed(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	if stats.Copied != 2 {
		t.Errorf("Copied = %d, want 2", stats.Copied)
	}
	if stats.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0", stats.Deleted)
	}
	if stats.Total != 2 {
		t.Errorf("Total = %d, want 2", stats.Total)
	}

	if _, err := os.Stat(filepath.Join(dst, "song.mp3")); err != nil {
		t.Error("song.mp3 not in dst")
	}
	if _, err := os.Stat(filepath.Join(dst, "Artist", "Album", "track.flac")); err != nil {
		t.Error("nested track.flac not in dst")
	}
}

func TestSyncRenamed_SkipsUnchanged(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	data := []byte("audio")
	mustWriteFile(t, filepath.Join(src, "song.mp3"), data, epoch)
	mustWriteFile(t, filepath.Join(dst, "song.mp3"), data, epoch) // same size + same mtime

	stats, err := syncRenamed(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 {
		t.Errorf("Copied = %d, want 0 (file unchanged)", stats.Copied)
	}
}

func TestSyncRenamed_CopiesOnSizeDiff(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	mustWriteFile(t, filepath.Join(src, "song.mp3"), []byte("new longer audio"), epoch)
	mustWriteFile(t, filepath.Join(dst, "song.mp3"), []byte("old"), epoch) // same mtime, different size

	stats, err := syncRenamed(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 1 {
		t.Errorf("Copied = %d, want 1 (size changed)", stats.Copied)
	}
}

func TestSyncRenamed_CopiesOnMtimeDiff(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	data := []byte("audio")
	mustWriteFile(t, filepath.Join(src, "song.mp3"), data, epoch.Add(5*time.Second))
	mustWriteFile(t, filepath.Join(dst, "song.mp3"), data, epoch) // same size, mtime diff = 5s > 2s

	stats, err := syncRenamed(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 1 {
		t.Errorf("Copied = %d, want 1 (mtime diff > 2s)", stats.Copied)
	}
}

func TestSyncRenamed_SkipsWithinMtimeTolerance(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	data := []byte("audio")
	mustWriteFile(t, filepath.Join(src, "song.mp3"), data, epoch.Add(time.Second))
	mustWriteFile(t, filepath.Join(dst, "song.mp3"), data, epoch) // same size, mtime diff = 1s ≤ 2s

	stats, err := syncRenamed(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 {
		t.Errorf("Copied = %d, want 0 (mtime diff within tolerance)", stats.Copied)
	}
}

func TestSyncRenamed_DeletesExtra(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	data := []byte("audio")
	mustWriteFile(t, filepath.Join(src, "keep.mp3"), data, epoch)
	mustWriteFile(t, filepath.Join(dst, "keep.mp3"), data, epoch)
	mustWriteFile(t, filepath.Join(dst, "old.mp3"), []byte("stale"), epoch)

	stats, err := syncRenamed(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", stats.Deleted)
	}
	if stats.Total != 1 {
		t.Errorf("Total = %d, want 1", stats.Total)
	}
	if _, err := os.Stat(filepath.Join(dst, "old.mp3")); !os.IsNotExist(err) {
		t.Error("old.mp3 should have been deleted")
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.mp3")); err != nil {
		t.Error("keep.mp3 should still exist")
	}
}

func TestSyncRenamed_DeletesExtraDirectory(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	// src is empty; dst has a directory with a file inside
	os.MkdirAll(filepath.Join(dst, "OldArtist", "Album"), 0o755)
	mustWriteFile(t, filepath.Join(dst, "OldArtist", "Album", "track.mp3"), []byte("x"), epoch)

	stats, err := syncRenamed(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	// 3 entries deleted: track.mp3, Album/, OldArtist/
	if stats.Deleted != 3 {
		t.Errorf("Deleted = %d, want 3", stats.Deleted)
	}
	if _, err := os.Stat(filepath.Join(dst, "OldArtist")); !os.IsNotExist(err) {
		t.Error("OldArtist dir should have been deleted")
	}
}

func TestSyncRenamed_PreservesBmark(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	// .bmark is in dst but not src — should survive deletion pass
	mustWriteFile(t, filepath.Join(dst, "bookmark.bmark"), []byte("pos"), epoch)

	stats, err := syncRenamed(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0 (.bmark must be preserved)", stats.Deleted)
	}
	if _, err := os.Stat(filepath.Join(dst, "bookmark.bmark")); err != nil {
		t.Error("bookmark.bmark was deleted")
	}
}

func TestSyncRenamed_SanitizesColonsInFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	mustWriteFile(t, filepath.Join(src, "Book: Part 1.mp3"), []byte("data"), epoch)

	if _, err := syncRenamed(src, dst); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dst, "Book -  Part 1.mp3")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("sanitized file not found at %q", want)
	}
}

func TestSyncRenamed_SanitizesColonsInDir(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	mustWriteFile(t, filepath.Join(src, "Artist: Name", "track.mp3"), []byte("data"), epoch)

	if _, err := syncRenamed(src, dst); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dst, "Artist -  Name", "track.mp3")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file not found at sanitized path %q", want)
	}
}

func TestSyncRenamed_PreservesMtime(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	mtime := time.Date(2023, 6, 15, 8, 30, 0, 0, time.UTC)
	mustWriteFile(t, filepath.Join(src, "song.mp3"), []byte("audio"), mtime)

	if _, err := syncRenamed(src, dst); err != nil {
		t.Fatal(err)
	}

	dstInfo, err := os.Stat(filepath.Join(dst, "song.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	diff := dstInfo.ModTime().Sub(mtime)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Second {
		t.Errorf("dst mtime = %v, want ~%v", dstInfo.ModTime(), mtime)
	}
}

// ---- notify ----

func TestNotify(t *testing.T) {
	var gotMethod, gotTitle, gotPriority, gotTags, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotTitle = r.Header.Get("Title")
		gotPriority = r.Header.Get("Priority")
		gotTags = r.Header.Get("Tags")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := notify(srv.URL, "iPod sync complete", "some stats"); err != nil {
		t.Fatalf("notify: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotTitle != "iPod sync complete" {
		t.Errorf("Title = %q, want %q", gotTitle, "iPod sync complete")
	}
	if gotPriority != "default" {
		t.Errorf("Priority = %q, want default", gotPriority)
	}
	if gotTags != "white_check_mark" {
		t.Errorf("Tags = %q, want white_check_mark", gotTags)
	}
	if gotBody != "some stats" {
		t.Errorf("body = %q, want %q", gotBody, "some stats")
	}
}

func TestNotify_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := notify(srv.URL, "title", "body")
	if err == nil {
		t.Error("expected error for 5xx response, got nil")
	}
}
