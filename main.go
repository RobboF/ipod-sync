package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	appleVendorID = "05ac"
	iPodProductID = "1261"
	mountPoint    = "/mnt/ipod"
	ntfyURL       = "http://ntfy.ntfy.svc.cluster.local/ipod"
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
		destPath := filepath.Join(dst, sanitizeRelPath(rel))
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
				return nil
			}
			stats.Copied++
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
			if err := os.RemoveAll(path); err != nil {
				fmt.Fprintf(os.Stderr, "delete %s: %v\n", path, err)
			} else {
				stats.Deleted++
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

	elapsed := time.Since(start)
	var dur string
	if elapsed >= time.Minute {
		dur = fmt.Sprintf("%dm %ds", int(elapsed.Minutes()), int(elapsed.Seconds())%60)
	} else {
		dur = fmt.Sprintf("%ds", int(elapsed.Seconds()))
	}

	body := fmt.Sprintf(
		"Audiobooks: %d updated, %d removed (%d files)\nMusic: %d updated, %d removed (%d files)\nCompleted in %s",
		abStats.Copied, abStats.Deleted, abStats.Total,
		muStats.Copied, muStats.Deleted, muStats.Total,
		dur,
	)

	if err := notify(ntfyURL, "iPod sync complete", body); err != nil {
		fmt.Fprintf(os.Stderr, "notify: %v\n", err)
	}
}
