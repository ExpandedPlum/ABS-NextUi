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
mkdir -p "$WORK/books/Author A/Chaptered" "$WORK/books/Author B/Multi" "$WORK/books/Author A/The Saga/Vol 1 - Dawn" \
	"$WORK/podcasts/Test Cast"
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

ffmpeg -loglevel error -y -f lavfi -i "sine=f=500:d=20" -metadata title="Dawn" -c:a aac -b:a 32k \
	"$WORK/books/Author A/The Saga/Vol 1 - Dawn/Dawn.m4b"
for i in 1 2; do
	ffmpeg -loglevel error -y -f lavfi -i "sine=f=$((600 + i * 50)):d=20" -metadata title="Episode $i" \
		-metadata album="Test Cast" -metadata date="2026-0$i-01" -c:a libmp3lame -b:a 32k "$WORK/podcasts/Test Cast/Episode $i.mp3"
done

echo "== audiobookshelf server"
docker run -d --name "$NAME" -p "$PORT:80" -v "$WORK/books:/audiobooks:ro" -v "$WORK/podcasts:/podcasts:ro" \
	ghcr.io/advplyr/audiobookshelf:latest >/dev/null
for _ in $(seq 1 60); do curl -fs "$B/status" >/dev/null 2>&1 && break; sleep 1; done
curl -fs -X POST "$B/init" -H 'Content-Type: application/json' -d '{"newRoot":{"username":"root","password":"pw"}}' >/dev/null
TOK=$(curl -fs -X POST "$B/login" -H 'Content-Type: application/json' -H 'x-return-tokens: true' \
	-d '{"username":"root","password":"pw"}' | pjson 'd["user"].get("accessToken") or d["user"]["token"]')
LIB=$(curl -fs -X POST "$B/api/libraries" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
	-d '{"name":"Books","folders":[{"fullPath":"/audiobooks"}],"mediaType":"book"}' | pjson 'd["id"]')
PLIB=$(curl -fs -X POST "$B/api/libraries" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
	-d '{"name":"Podcasts","folders":[{"fullPath":"/podcasts"}],"mediaType":"podcast"}' | pjson 'd["id"]')
curl -fs -X POST "$B/api/libraries/$LIB/scan" -H "Authorization: Bearer $TOK" >/dev/null
curl -fs -X POST "$B/api/libraries/$PLIB/scan" -H "Authorization: Bearer $TOK" >/dev/null
sleep 6

echo "== daemon"
(cd "$ROOT/daemon" && go build -o "$WORK/absd" .)
ABSD_DATA="$WORK/data" SDCARD_PATH="$WORK/sd" ABSD_AUDIO_OUT=null ABSD_FFMPEG="$(command -v ffmpeg)" \
	ABSD_NO_HOTKEYS=1 ABSD_LOG="$WORK/absd.log" ABSD_IDLE_EXIT=0 "$WORK/absd" serve &
sleep 1
"$WORK/absd" ctl login "{\"server\":\"localhost:$PORT\",\"username\":\"root\",\"password\":\"pw\"}" >/dev/null || fail login
ITEMS=$("$WORK/absd" ctl library "{\"library\":\"$LIB\"}")
CH=$(echo "$ITEMS" | pjson '[i["id"] for i in d["items"] if i["title"]=="Chaptered"][0]')
MULTI=$(echo "$ITEMS" | pjson '[i["id"] for i in d["items"] if i["title"] not in ("Chaptered","Dawn")][0]')
DAWN=$(echo "$ITEMS" | pjson '[i["id"] for i in d["items"] if i["title"]=="Dawn"][0]')

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

echo "== series, authors, collections, search"
SER=$("$WORK/absd" ctl series "{\"library\":\"$LIB\"}")
[ "$(echo "$SER" | pjson 'd[0]["title"]')" = "The Saga" ] || fail "series list: $SER"
SID=$(echo "$SER" | pjson 'd[0]["id"]')
[ "$("$WORK/absd" ctl series/items "{\"library\":\"$LIB\",\"id\":\"$SID\"}" | pjson 'd[0]["id"]')" = "$DAWN" ] || fail "series items"
AUTH=$("$WORK/absd" ctl authors "{\"library\":\"$LIB\"}" | pjson '[a["id"] for a in d if a["title"]=="Author A"][0]')
[ "$("$WORK/absd" ctl author/items "{\"id\":\"$AUTH\"}" | pjson 'len(d)')" = "2" ] || fail "author items"
curl -fs -X POST "$B/api/collections" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
	-d "{\"libraryId\":\"$LIB\",\"name\":\"Faves\",\"books\":[\"$DAWN\"]}" >/dev/null
COL=$("$WORK/absd" ctl collections "{\"library\":\"$LIB\"}" | pjson 'd[0]["id"]')
[ "$("$WORK/absd" ctl collection/items "{\"id\":\"$COL\"}" | pjson 'd[0]["title"]')" = "Dawn" ] || fail "collection items"
[ "$("$WORK/absd" ctl search "{\"library\":\"$LIB\",\"q\":\"saga\"}" | pjson 'd[0]["kind"]')" = "series" ] || fail "search"

echo "== bookmarks"
"$WORK/absd" ctl play "{\"id\":\"$DAWN\",\"position\":5}" >/dev/null || fail "play dawn"
sleep 2
BT=$("$WORK/absd" ctl bookmark/add '{"title":"Here"}' | pjson 'd["time"]')
[ "$("$WORK/absd" ctl bookmarks "{\"id\":\"$DAWN\"}" | pjson 'd[0]["title"]')" = "Here" ] || fail "bookmark not listed"
"$WORK/absd" ctl bookmark/delete "{\"id\":\"$DAWN\",\"time\":$BT}" >/dev/null || fail "bookmark delete"
[ "$("$WORK/absd" ctl bookmarks "{\"id\":\"$DAWN\"}" | pjson 'len(d)')" = "0" ] || fail "bookmark not deleted"
"$WORK/absd" ctl stop >/dev/null

echo "== podcasts"
POD=$("$WORK/absd" ctl library "{\"library\":\"$PLIB\"}" | pjson 'd["items"][0]["id"]')
EPS=$("$WORK/absd" ctl podcast "{\"id\":\"$POD\"}")
[ "$(echo "$EPS" | pjson 'len(d["items"])')" = "2" ] || fail "podcast episodes: $EPS"
EP=$(echo "$EPS" | pjson 'd["items"][0]["episodeId"]')
"$WORK/absd" ctl play "{\"id\":\"$POD\",\"episodeId\":\"$EP\",\"position\":16}" >/dev/null || fail "play episode"
sleep 7
[ "$(status 'd["player"]["state"]')" = "ended" ] || fail "episode did not end"
sleep 1
FIN=$(curl -fs "$B/api/me/progress/$POD/$EP" -H "Authorization: Bearer $TOK" | pjson 'd["isFinished"]')
[ "$FIN" = "True" ] || fail "episode not marked finished"
[ "$("$WORK/absd" ctl episodes/recent "{\"library\":\"$PLIB\"}" | pjson 'len(d) >= 1')" = "True" ] || fail "recent episodes"

echo "== API key and phone login"
UIDV=$(curl -fs "$B/api/me" -H "Authorization: Bearer $TOK" | pjson 'd["id"]')
KEY=$(curl -fs -X POST "$B/api/api-keys" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
	-d "{\"name\":\"it\",\"userId\":\"$UIDV\",\"isActive\":true}" | pjson 'd["apiKey"]["apiKey"]')
"$WORK/absd" ctl logout >/dev/null
"$WORK/absd" ctl login "{\"server\":\"localhost:$PORT\",\"apiKey\":\"$KEY\"}" >/dev/null || fail "API key login"
[ "$(status 'd["username"]')" = "root" ] || fail "API key user"
"$WORK/absd" ctl logout >/dev/null
URL=$("$WORK/absd" ctl login/phone | pjson 'd["url"]')
curl -fs -X POST "$URL" --data-urlencode "server=localhost:$PORT" -d "method=password&username=root&password=pw" | grep -q "Signed in" \
	|| fail "phone login page"
[ "$(status 'd["loggedIn"]')" = "True" ] || fail "phone login did not sign in"

echo "PASS"
