#!/bin/sh
# Audiobookshelf.pak — NextUI launcher entry point.
#
# Starts the absd playback daemon (if it isn't already running) detached from
# this script, so audio keeps playing after the UI exits and while games run,
# then shows the UI.

PAK_DIR="$(cd "$(dirname "$0")" && pwd)"
PAK_NAME="$(basename "$PAK_DIR" .pak)"
cd "$PAK_DIR" || exit 1

: "${SDCARD_PATH:=/mnt/SDCARD}"
: "${PLATFORM:=tg5040}"
: "${USERDATA_PATH:=$SDCARD_PATH/.userdata/$PLATFORM}"
: "${SHARED_USERDATA_PATH:=$SDCARD_PATH/.userdata/shared}"
: "${LOGS_PATH:=$USERDATA_PATH/logs}"
export SDCARD_PATH PLATFORM USERDATA_PATH SHARED_USERDATA_PATH LOGS_PATH
mkdir -p "$LOGS_PATH"

ABSD="$PAK_DIR/bin/absd"
export ABSD_LOG="$LOGS_PATH/$PAK_NAME-daemon.txt"

# Pause and save progress right before the device goes to sleep (NextUI runs
# *.sync.sh hooks synchronously before suspending).
HOOK_DIR="$USERDATA_PATH/.hooks/pre-sleep.d"
mkdir -p "$HOOK_DIR"
cat > "$HOOK_DIR/audiobookshelf.sync.sh" <<EOF
#!/bin/sh
[ -S /tmp/absd.sock ] && "$ABSD" ctl presleep >/dev/null 2>&1
exit 0
EOF
chmod +x "$HOOK_DIR/audiobookshelf.sync.sh"

if ! "$ABSD" ctl status >/dev/null 2>&1; then
	if command -v setsid >/dev/null 2>&1; then
		setsid "$ABSD" serve </dev/null >/dev/null 2>&1 &
	else
		nohup "$ABSD" serve </dev/null >/dev/null 2>&1 &
	fi
fi

"$PAK_DIR/bin/abs-ui" >"$LOGS_PATH/$PAK_NAME.txt" 2>&1
