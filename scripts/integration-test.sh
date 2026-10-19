#!/bin/sh
# End-to-end test of the daemon against a real Audiobookshelf server in Docker.
# Audio goes to ffmpeg's null sink, so no sound card is needed.
#
# Needs: docker, go, ffmpeg (with libmp3lame), python3, curl.
# Usage: scripts/integration-test.sh
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
PORT="${ABS_PORT:-13399}"
NAME="abs-nextui-it-$$"
B="http://localhost:$PORT"
export ABSD_SOCKET="$WORK/absd.sock"

cleanup() {
	"$WORK/absd" ctl quit >/dev/null 2>&1 || true
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

fail() { echo "FAIL: $*"; echo "--- daemon log"; tail -20 "$WORK/absd.log" || true; exit 1; }
pjson() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }
status() { "$WORK/absd" ctl status | pjson "$1"; }

echo "== test media"
mkdir -p "$WORK/books/Author A/Chaptered" "$WORK/books/Author B/Multi"
cat >"$WORK/ch.txt" <<'EOF'
;FFMETADATA1
title=Chaptered
artist=Author A
[CHAPTER]
TIMEBASE=1/1000
START=0
END=60000
title=One
[CHAPTER]
TIMEBASE=1/1000
START=60000
END=120000
title=Two
EOF
ffmpeg -loglevel error -y -f lavfi -i "sine=f=440:d=120" -i "$WORK/ch.txt" -map_metadata 1 -map_chapters 1 \
	-c:a aac -b:a 48k "$WORK/books/Author A/Chaptered/Chaptered.m4b"
for i in 1 2; do
	ffmpeg -loglevel error -y -f lavfi -i "sine=f=$((300 + i * 100)):d=30" -metadata track=$i \
		-c:a libmp3lame -b:a 48k "$WORK/books/Author B/Multi/Part $i.mp3"
done

echo "== audiobookshelf server"
docker run -d --name "$NAME" -p "$PORT:80" -v "$WORK/books:/audiobooks:ro" ghcr.io/advplyr/audiobookshelf:latest >/dev/null
for _ in $(seq 1 60); do curl -fs "$B/status" >/dev/null 2>&1 && break; sleep 1; done
curl -fs -X POST "$B/init" -H 'Content-Type: application/json' -d '{"newRoot":{"username":"root","password":"pw"}}' >/dev/null
TOK=$(curl -fs -X POST "$B/login" -H 'Content-Type: application/json' -H 'x-return-tokens: true' \
	-d '{"username":"root","password":"pw"}' | pjson 'd["user"].get("accessToken") or d["user"]["token"]')
LIB=$(curl -fs -X POST "$B/api/libraries" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
	-d '{"name":"Books","folders":[{"fullPath":"/audiobooks"}],"mediaType":"book"}' | pjson 'd["id"]')
curl -fs -X POST "$B/api/libraries/$LIB/scan" -H "Authorization: Bearer $TOK" >/dev/null
sleep 5

echo "== daemon"
(cd "$ROOT/daemon" && go build -o "$WORK/absd" .)
ABSD_DATA="$WORK/data" SDCARD_PATH="$WORK/sd" ABSD_AUDIO_OUT=null ABSD_FFMPEG="$(command -v ffmpeg)" \
	ABSD_NO_HOTKEYS=1 ABSD_LOG="$WORK/absd.log" ABSD_IDLE_EXIT=0 "$WORK/absd" serve &
sleep 1
"$WORK/absd" ctl login "{\"server\":\"localhost:$PORT\",\"username\":\"root\",\"password\":\"pw\"}" >/dev/null || fail login
ITEMS=$("$WORK/absd" ctl library "{\"library\":\"$LIB\"}")
CH=$(echo "$ITEMS" | pjson '[i["id"] for i in d["items"] if i["title"]=="Chaptered"][0]')
MULTI=$(echo "$ITEMS" | pjson '[i["id"] for i in d["items"] if i["title"]!="Chaptered"][0]')

echo "== stream, seek, chapters"
"$WORK/absd" ctl play "{\"id\":\"$CH\",\"position\":55}" >/dev/null || fail play
sleep 7
[ "$(status 'd["player"]["chapterIndex"]')" = "1" ] || fail "expected chapter 2 after crossing 60s"
"$WORK/absd" ctl pause >/dev/null
"$WORK/absd" ctl sync >/dev/null
POS=$(curl -fs "$B/api/me/progress/$CH" -H "Authorization: Bearer $TOK" | pjson 'int(d["currentTime"])')
[ "$POS" -ge 58 ] && [ "$POS" -le 66 ] || fail "server progress $POS, expected ~61"

echo "== multi-file track change and finish"
"$WORK/absd" ctl play "{\"id\":\"$MULTI\",\"position\":27}" >/dev/null || fail play2
sleep 6
P=$(status 'int(d["player"]["position"])')
[ "$P" -ge 31 ] || fail "did not cross into track 2 (pos $P)"
"$WORK/absd" ctl seek '{"position":58}' >/dev/null
sleep 5
[ "$(status 'd["player"]["state"]')" = "ended" ] || fail "book did not end"
sleep 1
FIN=$(curl -fs "$B/api/me/progress/$MULTI" -H "Authorization: Bearer $TOK" | pjson 'd["isFinished"]')
[ "$FIN" = "True" ] || fail "book not marked finished on server"

echo "== download and offline playback"
"$WORK/absd" ctl stop >/dev/null
"$WORK/absd" ctl download "{\"id\":\"$CH\"}" >/dev/null
for _ in $(seq 1 30); do
	[ "$("$WORK/absd" ctl downloaded | pjson 'len(d["items"])')" = "1" ] && break
	sleep 1
done
[ "$("$WORK/absd" ctl downloaded | pjson 'len(d["items"])')" = "1" ] || fail "download did not complete"
docker pause "$NAME" >/dev/null
"$WORK/absd" ctl check >/dev/null || true
"$WORK/absd" ctl play "{\"id\":\"$CH\",\"position\":10}" >/dev/null || fail "offline play"
sleep 3
[ "$(status 'd["player"]["offlineFiles"]')" = "True" ] || fail "not playing from SD"
"$WORK/absd" ctl pause >/dev/null
sleep 11 # let the failed sync time out and queue
docker unpause "$NAME" >/dev/null
"$WORK/absd" ctl check >/dev/null
"$WORK/absd" ctl sync >/dev/null
POS=$(curl -fs "$B/api/me/progress/$CH" -H "Authorization: Bearer $TOK" | pjson 'int(d["currentTime"])')
[ "$POS" -ge 11 ] && [ "$POS" -le 16 ] || fail "offline progress not synced (server has $POS)"

echo "PASS"
