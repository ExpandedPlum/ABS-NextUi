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
DAEMON_LOG="$LOGS_PATH/$PAK_NAME-daemon.txt"
# The UI uses these to restart the daemon if it isn't running.
export ABSD_BIN="$ABSD" ABSD_LOG_FILE="$DAEMON_LOG"

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

# Every NextUI app sets the speaker to full volume for a moment as it starts,
# which is a loud blip with a book playing. Hold the book for a few seconds
# around each app switch; it resumes on its own. This UI doesn't do that
# reset, so opening it doesn't pause the book.
for phase in pre-launch.d post-launch.d; do
	HOOK_DIR="$USERDATA_PATH/.hooks/$phase"
	mkdir -p "$HOOK_DIR"
	cat > "$HOOK_DIR/audiobookshelf.sync.sh" <<EOF
#!/bin/sh
[ -S /tmp/absd.sock ] || exit 0
[ "\$HOOK_PHASE" = pre ] && case "\$HOOK_EMU_PATH" in */$PAK_NAME.pak/*) exit 0 ;; esac
"$ABSD" ctl transition '{"seconds":3}' >/dev/null 2>&1
exit 0
EOF
	chmod +x "$HOOK_DIR/audiobookshelf.sync.sh"
done

# All daemon output (including Go runtime crashes) goes to its log.
if ! "$ABSD" ctl status >/dev/null 2>&1; then
	echo "launch.sh: starting $ABSD ($(uname -m), kernel $(uname -r))" >"$DAEMON_LOG"
	if command -v setsid >/dev/null 2>&1; then
		setsid "$ABSD" serve </dev/null >>"$DAEMON_LOG" 2>&1 &
	else
		nohup "$ABSD" serve </dev/null >>"$DAEMON_LOG" 2>&1 &
	fi
fi

"$PAK_DIR/bin/abs-ui" >"$LOGS_PATH/$PAK_NAME.txt" 2>&1
