package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	appleVendorID = "05ac"
	iPodProductID = "1261"
	mountPoint    = "/mnt/ipod"
	ntfyURL       = "http://ntfy.ntfy.svc.cluster.local/ipod"
	absURL        = "http://audiobookshelf.arr.svc.cluster.local"
)

func findIPod(basePath string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(basePath, "*/idVendor"))
	if err != nil {
		return "", err
	}
	for _, vendorFile := range matches {
		data, err := os.ReadFile(vendorFile)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) != appleVendorID {
			continue
		}
		dir := filepath.Dir(vendorFile)
		productData, err := os.ReadFile(filepath.Join(dir, "idProduct"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(productData)) == iPodProductID {
			return dir, nil
		}
	}
	return "", fmt.Errorf("iPod not found in sysfs")
}

// walkFollowSymlinks walks path following symlinks to directories,
// guarding against cycles via visited inode tracking.
func walkFollowSymlinks(root string, fn func(path string, info os.FileInfo) error) error {
	visited := make(map[uint64]bool)
	var walk func(string) error
	walk = func(path string) error {
		info, err := os.Lstat(path)
		if err != nil {
			return nil
		}
		// Follow symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				return nil
			}
			info, err = os.Stat(target)
			if err != nil {
				return nil
			}
			path = target
		}
		if info.IsDir() {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if ok {
				if visited[stat.Ino] {
					return nil
				}
				visited[stat.Ino] = true
			}
		}
		if err := fn(path, info); err != nil {
			return err
		}
		if info.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil
			}
			for _, e := range entries {
				if err := walk(filepath.Join(path, e.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(root)
}

func findBlockDevice(usbSysPath string) (string, error) {
	var blockDir string
	walkFollowSymlinks(usbSysPath, func(path string, info os.FileInfo) error {
		if info.IsDir() && info.Name() == "block" && blockDir == "" {
			blockDir = path
		}
		return nil
	})
	if blockDir == "" {
		return "", fmt.Errorf("no block device found under %s", usbSysPath)
	}
	entries, err := os.ReadDir(blockDir)
	if err != nil || len(entries) == 0 {
		return "", fmt.Errorf("no entries in block dir %s", blockDir)
	}
	return entries[0].Name(), nil
}

// sanitizeName applies FAT-safe renaming: colon → " - ", forbidden chars → "_".
func sanitizeName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch r {
		case ':':
			sb.WriteString(" - ")
		case '*', '?', '"', '<', '>', '|', '\\':
			sb.WriteRune('_')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func sanitizeRelPath(rel string) string {
	parts := strings.Split(rel, string(os.PathSeparator))
	for i, p := range parts {
		parts[i] = sanitizeName(p)
	}
	return strings.Join(parts, string(os.PathSeparator))
}

type SyncStats struct {
	Copied  int
	Deleted int
	Total   int
}

type ProgressStats struct {
	IPodToABS int
	ABSToIPod int
	Created   int
	InSync    int
}

type bmark struct {
	ByteOffset int64
	TimeMs     int64
	Dir        string
	Filename   string
}

var errNotFound = errors.New("not found")

func parseBmarks(path string) ([]bmark, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var marks []bmark
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		// Format: >3;0;<byte_offset>;0;<time_ms>;0;0;10000;10000;<dir>;<filename>
		parts := strings.SplitN(line, ";", 11)
		if len(parts) < 11 {
			continue
		}
		byteOffset, _ := strconv.ParseInt(parts[2], 10, 64)
		timeMs, _ := strconv.ParseInt(parts[4], 10, 64)
		marks = append(marks, bmark{
			ByteOffset: byteOffset,
			TimeMs:     timeMs,
			Dir:        parts[9],
			Filename:   parts[10],
		})
	}
	return marks, nil
}

func writeBmarks(path string, marks []bmark) error {
	var sb strings.Builder
	for _, m := range marks {
		fmt.Fprintf(&sb, ">3;0;%d;0;%d;0;0;10000;10000;%s;%s\n",
			m.ByteOffset, m.TimeMs, m.Dir, m.Filename)
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func absDoRequest(method, token, url string, body, out interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return errNotFound
	}
	if resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%s %s: status %d", method, url, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func syncProgress(absBase, token, audiobooksDst string) (ProgressStats, error) {
	var stats ProgressStats

	var libResp struct {
		Libraries []struct {
			ID        string `json:"id"`
			MediaType string `json:"mediaType"`
		} `json:"libraries"`
	}
	if err := absDoRequest(http.MethodGet, token, absBase+"/api/libraries", nil, &libResp); err != nil {
		return stats, fmt.Errorf("list libraries: %w", err)
	}
	var libID string
	for _, l := range libResp.Libraries {
		if l.MediaType == "book" {
			libID = l.ID
			break
		}
	}
	if libID == "" {
		return stats, fmt.Errorf("no audiobook library found")
	}

	var itemsResp struct {
		Results []struct {
			ID    string `json:"id"`
			RelPath string `json:"relPath"`
			Media struct {
				Duration float64 `json:"duration"`
			} `json:"media"`
		} `json:"results"`
	}
	if err := absDoRequest(http.MethodGet, token, absBase+"/api/libraries/"+libID+"/items?limit=10000", nil, &itemsResp); err != nil {
		return stats, fmt.Errorf("list items: %w", err)
	}

	type absItem struct {
		id       string
		relPath  string
		duration float64
	}
	itemByPath := make(map[string]absItem)
	itemByID := make(map[string]absItem)
	for _, it := range itemsResp.Results {
		key := sanitizeRelPath(it.RelPath)
		ai := absItem{id: it.ID, relPath: key, duration: it.Media.Duration}
		itemByPath[key] = ai
		itemByID[it.ID] = ai
	}

	const syncThresholdMs = int64(5000)
	hddID := "<HDD0>"
	bmarkPaths := make(map[string]bool)

	if err := filepath.Walk(audiobooksDst, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".bmark") {
			return nil
		}
		rel, _ := filepath.Rel(audiobooksDst, path)
		relNoExt := strings.TrimSuffix(rel, ".bmark")
		bmarkPaths[relNoExt] = true

		ai, ok := itemByPath[relNoExt]
		if !ok {
			fmt.Printf("  no match  %s\n", relNoExt)
			return nil
		}

		marks, err := parseBmarks(path)
		if err != nil || len(marks) == 0 {
			return nil
		}
		iPodMs := marks[0].TimeMs

		// Derive HDD identifier from existing bmarks (e.g. <HDD0>).
		if dir := marks[0].Dir; strings.HasPrefix(dir, "/") {
			if parts := strings.SplitN(dir, "/", 3); len(parts) >= 2 && parts[1] != "" {
				hddID = parts[1]
			}
		}

		var prog struct {
			CurrentTime float64 `json:"currentTime"`
			IsFinished  bool    `json:"isFinished"`
		}
		err = absDoRequest(http.MethodGet, token, absBase+"/api/me/progress/"+ai.id, nil, &prog)
		if err != nil && !errors.Is(err, errNotFound) {
			fmt.Fprintf(os.Stderr, "  progress %s: %v\n", relNoExt, err)
			return nil
		}
		absMs := int64(prog.CurrentTime * 1000)

		diff := iPodMs - absMs
		if diff < 0 {
			diff = -diff
		}
		if diff <= syncThresholdMs {
			stats.InSync++
			fmt.Printf("  in sync   %s\n", relNoExt)
			return nil
		}

		if iPodMs > absMs {
			fmt.Printf("  iPod→ABS  %s (%.0fs→%.0fs)\n", relNoExt, float64(absMs)/1000, float64(iPodMs)/1000)
			if err := absDoRequest(http.MethodPatch, token, absBase+"/api/me/progress/"+ai.id, map[string]interface{}{
				"currentTime": float64(iPodMs) / 1000,
				"isFinished":  false,
			}, nil); err != nil {
				fmt.Fprintf(os.Stderr, "  update ABS %s: %v\n", relNoExt, err)
				return nil
			}
			stats.IPodToABS++
			return nil
		}

		// ABS is ahead: rewrite bmark using existing bytes-per-ms ratio.
		fmt.Printf("  ABS→iPod  %s (%.0fs→%.0fs)\n", relNoExt, float64(iPodMs)/1000, float64(absMs)/1000)
		var bytesPerMs float64
		if marks[0].TimeMs > 0 {
			bytesPerMs = float64(marks[0].ByteOffset) / float64(marks[0].TimeMs)
		}
		updated := marks[0]
		updated.TimeMs = absMs
		updated.ByteOffset = int64(float64(absMs) * bytesPerMs)
		if err := writeBmarks(path, []bmark{updated}); err != nil {
			fmt.Fprintf(os.Stderr, "  write bmark %s: %v\n", relNoExt, err)
			return nil
		}
		stats.ABSToIPod++
		return nil
	}); err != nil {
		return stats, err
	}

	// Create bmarks for ABS in-progress books that have no bmark on the iPod yet.
	var inProg struct {
		LibraryItems []struct {
			ID                string `json:"id"`
			UserMediaProgress struct {
				CurrentTime float64 `json:"currentTime"`
				IsFinished  bool    `json:"isFinished"`
			} `json:"userMediaProgress"`
		} `json:"libraryItems"`
	}
	if err := absDoRequest(http.MethodGet, token, absBase+"/api/me/items-in-progress", nil, &inProg); err != nil {
		fmt.Fprintf(os.Stderr, "  items-in-progress: %v\n", err)
		return stats, nil
	}

	for _, item := range inProg.LibraryItems {
		if item.UserMediaProgress.IsFinished {
			continue
		}
		ai, ok := itemByID[item.ID]
		if !ok || bmarkPaths[ai.relPath] {
			continue
		}
		absMs := int64(item.UserMediaProgress.CurrentTime * 1000)
		if absMs == 0 {
			continue
		}

		bookDir := filepath.Join(audiobooksDst, filepath.FromSlash(ai.relPath))
		if _, err := os.Stat(bookDir); os.IsNotExist(err) {
			continue
		}
		entries, err := os.ReadDir(bookDir)
		if err != nil {
			continue
		}
		var m4bName string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".m4b") {
				m4bName = e.Name()
				break
			}
		}
		if m4bName == "" {
			continue
		}

		var byteOffset int64
		if ai.duration > 0 {
			if fi, err := os.Stat(filepath.Join(bookDir, m4bName)); err == nil {
				byteOffset = int64(item.UserMediaProgress.CurrentTime / ai.duration * float64(fi.Size()))
			}
		}

		iPodDir := "/" + hddID + "/Audiobooks/" + strings.ReplaceAll(ai.relPath, string(os.PathSeparator), "/") + "/"
		bmarkPath := filepath.Join(audiobooksDst, filepath.FromSlash(ai.relPath)+".bmark")

		fmt.Printf("  ABS→iPod  %s (new, %.0fs)\n", ai.relPath, item.UserMediaProgress.CurrentTime)
		if err := writeBmarks(bmarkPath, []bmark{{
			ByteOffset: byteOffset,
			TimeMs:     absMs,
			Dir:        iPodDir,
			Filename:   m4bName,
		}}); err != nil {
			fmt.Fprintf(os.Stderr, "  create bmark %s: %v\n", ai.relPath, err)
			continue
		}
		stats.Created++
	}

	return stats, nil
}

func copyFile(src, dst string, srcInfo os.FileInfo) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20) // 1 MiB buffer
	if _, err := io.CopyBuffer(out, in, buf); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Preserve mtime (FAT doesn't store permissions/ownership)
	return os.Chtimes(dst, srcInfo.ModTime(), srcInfo.ModTime())
}

func syncRenamed(src, dst string) (SyncStats, error) {
	var stats SyncStats
	src = strings.TrimRight(src, "/")
	dst = strings.TrimRight(dst, "/")

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return stats, err
	}

	// expectedDst tracks sanitized destination paths produced by this sync.
	expectedDst := make(map[string]bool)

	if err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == src {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		sanitizedRel := sanitizeRelPath(rel)
		destPath := filepath.Join(dst, sanitizedRel)
		expectedDst[destPath] = true

		if info.IsDir() {
			return os.MkdirAll(destPath, 0o755)
		}

		dstInfo, statErr := os.Stat(destPath)
		needsCopy := true
		if statErr == nil {
			mtDiff := info.ModTime().Sub(dstInfo.ModTime())
			if mtDiff < 0 {
				mtDiff = -mtDiff
			}
			// FAT has 2-second mtime resolution; mirror the shell's tolerance.
			if info.Size() == dstInfo.Size() && mtDiff <= 2*time.Second {
				needsCopy = false
			}
		}

		if needsCopy {
			if err := copyFile(path, destPath, info); err != nil {
				fmt.Fprintf(os.Stderr, "copy %s: %v\n", path, err)
				fmt.Printf("  error    %s\n", sanitizedRel)
				return nil
			}
			stats.Copied++
			fmt.Printf("  updated  %s\n", sanitizedRel)
		} else {
			fmt.Printf("  skipped  %s\n", sanitizedRel)
		}
		return nil
	}); err != nil {
		return stats, err
	}

	// Collect destination paths and delete anything not in expectedDst.
	var dstPaths []string
	filepath.Walk(dst, func(path string, info os.FileInfo, err error) error {
		if err == nil && path != dst {
			dstPaths = append(dstPaths, path)
		}
		return nil
	})
	// Reverse-sort so children are processed before parents.
	sort.Sort(sort.Reverse(sort.StringSlice(dstPaths)))

	for _, path := range dstPaths {
		if strings.HasSuffix(path, ".bmark") {
			continue
		}
		if !expectedDst[path] {
			rel, _ := filepath.Rel(dst, path)
			if err := os.RemoveAll(path); err != nil {
				fmt.Fprintf(os.Stderr, "delete %s: %v\n", path, err)
			} else {
				stats.Deleted++
				fmt.Printf("  deleted  %s\n", rel)
			}
		}
	}

	filepath.Walk(dst, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			stats.Total++
		}
		return nil
	})

	return stats, nil
}

func notify(url, title, body string) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", "default")
	req.Header.Set("Tags", "white_check_mark")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned status %d", resp.StatusCode)
	}
	return nil
}

func main() {
	usbSysPath, err := findIPod("/sys/bus/usb/devices")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	disk, err := findBlockDevice(usbSysPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	device := fmt.Sprintf("/dev/%s1", disk)
	fmt.Printf("Mounting %s\n", device)

	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := syscall.Mount(device, mountPoint, "vfat", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "mount: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := syscall.Unmount(mountPoint, 0); err != nil {
			fmt.Fprintf(os.Stderr, "umount: %v\n", err)
		}
	}()

	start := time.Now()

	fmt.Println("Syncing audiobooks...")
	abStats, err := syncRenamed("/nas/audiobookshelf/audiobooks", filepath.Join(mountPoint, "Audiobooks"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "audiobook sync: %v\n", err)
	}

	fmt.Println("Syncing music...")
	muStats, err := syncRenamed("/nas/music/library", filepath.Join(mountPoint, "Music"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "music sync: %v\n", err)
	}

	var progStats ProgressStats
	token := os.Getenv("ABS_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "ABS_TOKEN not set; skipping progress sync")
	} else {
		base := absURL
		if u := os.Getenv("ABS_URL"); u != "" {
			base = u
		}
		fmt.Println("Syncing progress...")
		progStats, err = syncProgress(base, token, filepath.Join(mountPoint, "Audiobooks"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "progress sync: %v\n", err)
		}
	}

	elapsed := time.Since(start)
	var dur string
	if elapsed >= time.Minute {
		dur = fmt.Sprintf("%dm %ds", int(elapsed.Minutes()), int(elapsed.Seconds())%60)
	} else {
		dur = fmt.Sprintf("%ds", int(elapsed.Seconds()))
	}

	body := fmt.Sprintf(
		"Audiobooks: %d updated, %d removed (%d files)\nMusic: %d updated, %d removed (%d files)\nProgress: %d iPod→ABS, %d ABS→iPod, %d created\nCompleted in %s",
		abStats.Copied, abStats.Deleted, abStats.Total,
		muStats.Copied, muStats.Deleted, muStats.Total,
		progStats.IPodToABS, progStats.ABSToIPod, progStats.Created,
		dur,
	)

	if err := notify(ntfyURL, "iPod sync complete", body); err != nil {
		fmt.Fprintf(os.Stderr, "notify: %v\n", err)
	}
}
