#!/bin/sh
set -e

USB_SYS=""
for f in /sys/bus/usb/devices/*/idVendor; do
  [ "$(cat "$f" 2>/dev/null)" = "05ac" ] || continue
  d=$(dirname "$f")
  [ "$(cat "$d/idProduct" 2>/dev/null)" = "1261" ] || continue
  USB_SYS="$d"
  break
done
[ -z "$USB_SYS" ] && { echo "iPod not found in sysfs"; exit 1; }

BLOCK_DIR=$(find -L "$USB_SYS" -name "block" -type d 2>/dev/null | head -1)
[ -z "$BLOCK_DIR" ] && { echo "No block device found under $USB_SYS"; exit 1; }

DISK=$(ls "$BLOCK_DIR" | head -1)
echo "Mounting /dev/${DISK}1"
mkdir -p /mnt/ipod
mount "/dev/${DISK}1" /mnt/ipod

sync_renamed() {
  src="${1%/}"; dst="${2%/}"
  mkdir -p "$dst"
  src_list=$(mktemp); expected=$(mktemp)
  find "$src" -mindepth 1 > "$src_list"
  _copied=0; _deleted=0
  while IFS= read -r f; do
    rel="${f#$src/}"
    dest="$dst/$(printf '%s' "$rel" | sed 's/:/ - /g; s/[*?"<>|\\]/_/g')"
    printf '%s\n' "$dest" >> "$expected"
    if [ -d "$f" ]; then
      mkdir -p "$dest"
    else
      mkdir -p "$(dirname "$dest")"
      _src_stat=$(stat -c '%s %Y' "$f" 2>/dev/null)
      _dst_stat=$(stat -c '%s %Y' "$dest" 2>/dev/null)
      _src_sz=${_src_stat% *}; _src_mt=${_src_stat#* }
      _dst_sz=${_dst_stat% *}; _dst_mt=${_dst_stat#* }
      _mt_diff=$(( ${_src_mt:-0} - ${_dst_mt:-0} ))
      [ "$_mt_diff" -lt 0 ] && _mt_diff=$(( -_mt_diff ))
      if [ "$_src_sz" != "$_dst_sz" ] || [ "$_mt_diff" -gt 2 ]; then
        rsync -a "$f" "$dest" >/dev/null 2>&1
        _copied=$((_copied + 1))
      fi
    fi
  done < "$src_list"
  sorted_expected=$(mktemp)
  sort "$expected" > "$sorted_expected"
  dst_list=$(mktemp)
  find "$dst" -mindepth 1 | sort > "$dst_list"
  to_delete=$(mktemp)
  comm -23 "$dst_list" "$sorted_expected" > "$to_delete"
  while IFS= read -r f; do
    case "$f" in *.bmark) continue ;; esac
    rm -rf "$f"
    _deleted=$((_deleted + 1))
  done < "$to_delete"
  _total=$(find "$dst" -type f | wc -l)
  rm -f "$src_list" "$expected" "$sorted_expected" "$dst_list" "$to_delete"
  printf '%d %d %d' "$_copied" "$_deleted" "$_total"
}

start=$(date +%s)

echo "Syncing audiobooks..."
ab=$(sync_renamed /nas/audiobookshelf/audiobooks /mnt/ipod/Audiobooks)
ab_up=$(printf '%s' "$ab" | cut -d' ' -f1)
ab_del=$(printf '%s' "$ab" | cut -d' ' -f2)
ab_tot=$(printf '%s' "$ab" | cut -d' ' -f3)

echo "Syncing music..."
mu=$(sync_renamed /nas/music/library /mnt/ipod/Music)
mu_up=$(printf '%s' "$mu" | cut -d' ' -f1)
mu_del=$(printf '%s' "$mu" | cut -d' ' -f2)
mu_tot=$(printf '%s' "$mu" | cut -d' ' -f3)

umount /mnt/ipod

elapsed=$(( $(date +%s) - start ))
[ "$elapsed" -ge 60 ] && dur="$((elapsed / 60))m $((elapsed % 60))s" || dur="${elapsed}s"
body=$(printf 'Audiobooks: %d updated, %d removed (%d files)\nMusic: %d updated, %d removed (%d files)\nCompleted in %s' \
  "$ab_up" "$ab_del" "$ab_tot" \
  "$mu_up" "$mu_del" "$mu_tot" \
  "$dur")

curl --silent --fail -X POST \
  -H "Title: iPod sync complete" \
  -H "Priority: default" \
  -H "Tags: white_check_mark" \
  -d "$body" \
  http://ntfy.ntfy.svc.cluster.local/ipod
