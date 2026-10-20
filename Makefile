# Audiobookshelf.pak for NextUI (TrimUI Brick / Smart Pro, platform tg5040)
#
#   make build      build absd, abs-ui and ffmpeg for the device
#   make release    build and package dist/Audiobookshelf.pak.zip
#                   (RELEASE_VERSION=x.y.z stamps that version into pak.json)
#   make push       release, then install on a Brick connected over adb
#   make lint test  go vet + unit tests (test-integration needs Docker)
#   make desktop    build UI + daemon for this machine, for testing
#   make clean
#
# Requires Go, Docker, jq and zip.

PAK_NAME   := $(shell jq -r .name pak.json)
PAK_TYPE   := $(shell jq -r .type pak.json)
PAK_FOLDER := $(shell echo $(PAK_TYPE) | cut -c1)$(shell echo $(PAK_TYPE) | tr '[:upper:]' '[:lower:]' | cut -c2-)s
RELEASE_VERSION ?= $(shell jq -r .version pak.json)

PLATFORM  := tg5040
TOOLCHAIN ?= ghcr.io/loveretro/tg5040-toolchain:latest
FFMPEG_VERSION ?= 7.1.2

PUSH_SDCARD_PATH ?= /mnt/SDCARD
PUSH_PLATFORM ?= tg5040

BUILD := build
DIST  := dist
DEV   := $(BUILD)/$(PLATFORM)
PAK   := $(DIST)/$(PAK_NAME).pak

DOCKER_RUN := docker run --rm -u $(shell id -u):$(shell id -g) -v "$(CURDIR)":/src -w /src

.PHONY: all build daemon ui ffmpeg release bump-version push lint test test-integration desktop clean

all: release

build: daemon ui ffmpeg

# Always rebuilt so the embedded version matches RELEASE_VERSION.
daemon:
	@mkdir -p $(DEV)
	cd daemon && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags "-s -w -X github.com/ExpandedPlum/ABS-NextUi/daemon/internal/service.Version=$(RELEASE_VERSION)" \
		-o ../$(DEV)/absd .

ui: $(DEV)/abs-ui
$(DEV)/abs-ui: $(wildcard ui/src/*) ui/Makefile
	@mkdir -p $(DEV)
	$(DOCKER_RUN) $(TOOLCHAIN) make -C ui device OUT=/src/$(BUILD)

ffmpeg: $(DEV)/ffmpeg
$(DEV)/ffmpeg: scripts/build-ffmpeg.sh
	@mkdir -p $(DEV) $(BUILD)/ffmpeg-work
	test -f $(BUILD)/ffmpeg-work/ffmpeg-$(FFMPEG_VERSION).tar.xz || \
		curl -fsSL -o $(BUILD)/ffmpeg-work/ffmpeg-$(FFMPEG_VERSION).tar.xz \
		https://ffmpeg.org/releases/ffmpeg-$(FFMPEG_VERSION).tar.xz
	$(DOCKER_RUN) -e WORK=/src/$(BUILD)/ffmpeg-work -e FFMPEG_VERSION=$(FFMPEG_VERSION) \
		$(TOOLCHAIN) sh scripts/build-ffmpeg.sh /src/$(DEV)

# The pak's files go at the zip's root: the Pak Store unzips the release
# into Tools/tg5040/$(PAK_NAME).pak/.
release: build bump-version
	rm -rf $(PAK) $(DIST)/$(PAK_NAME).pak.zip
	mkdir -p $(PAK)/bin
	cp pak/launch.sh pak.json $(PAK)/
	cp $(DEV)/absd $(DEV)/abs-ui $(DEV)/ffmpeg $(PAK)/bin/
	cp -r licenses $(PAK)/
	chmod +x $(PAK)/launch.sh $(PAK)/bin/*
	cd $(PAK) && zip -qr "../$(PAK_NAME).pak.zip" .
	ls -lah $(DIST)

bump-version:
	jq '.version = "$(RELEASE_VERSION)"' pak.json > pak.json.tmp
	mv pak.json.tmp pak.json

# Install on a Brick connected over USB (adb).
push: release
	adb push "$(PAK)/." "$(PUSH_SDCARD_PATH)/$(PAK_FOLDER)/$(PUSH_PLATFORM)/$(PAK_NAME).pak"

lint:
	cd daemon && test -z "$$(gofmt -l .)" && go vet ./...
	for f in pak/launch.sh scripts/build-ffmpeg.sh scripts/integration-test.sh; do sh -n "$$f" || exit 1; done

test:
	cd daemon && go test ./...

# Runs the daemon against a real Audiobookshelf server in Docker.
test-integration:
	scripts/integration-test.sh

desktop:
	$(MAKE) -C ui desktop OUT=../$(BUILD)
	cd daemon && go build -o ../$(BUILD)/desktop/absd .

clean:
	rm -rf $(DIST) $(BUILD)/$(PLATFORM) $(BUILD)/desktop
