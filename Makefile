# Audiobookshelf.pak for NextUI (TrimUI Brick / Smart Pro, platform tg5040)
#
#   make            build everything and package dist/Audiobookshelf.pak.zip
#   make daemon     absd (Go, static arm64)
#   make ui         abs-ui (C/SDL2, built in the NextUI toolchain container)
#   make ffmpeg     minimal audio-only ffmpeg (toolchain container)
#   make desktop    build UI + daemon for this machine, for testing
#   make clean
#
# Requires Go and Docker.

PAK_NAME   := Audiobookshelf
PLATFORM   := tg5040
TOOLCHAIN  ?= ghcr.io/loveretro/tg5040-toolchain:latest
FFMPEG_VERSION ?= 7.1.2
VERSION    := $(shell sed -n 's/.*"version": *"v\{0,1\}\([^"]*\)".*/\1/p' pak/pak.json)

BUILD := build
DIST  := dist
DEV   := $(BUILD)/$(PLATFORM)
PAK   := $(DIST)/$(PAK_NAME).pak

DOCKER_RUN := docker run --rm -u $(shell id -u):$(shell id -g) -v "$(CURDIR)":/src -w /src

.PHONY: all daemon ui ffmpeg pak desktop clean

all: pak

daemon: $(DEV)/absd
$(DEV)/absd: $(shell find daemon -name '*.go') daemon/go.mod daemon/go.sum
	@mkdir -p $(DEV)
	cd daemon && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
		-ldflags "-s -w" -o ../$@ .

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

pak: daemon ui ffmpeg
	rm -rf $(PAK) $(DIST)/$(PAK_NAME).pak.zip
	mkdir -p $(PAK)/bin
	cp pak/launch.sh pak/pak.json $(PAK)/
	cp $(DEV)/absd $(DEV)/abs-ui $(DEV)/ffmpeg $(PAK)/bin/
	cp LICENSE $(PAK)/ 2>/dev/null || true
	cp -r licenses $(PAK)/ 2>/dev/null || true
	chmod +x $(PAK)/launch.sh $(PAK)/bin/*
	cd $(DIST) && rm -f $(PAK_NAME).pak.zip && zip -qr $(PAK_NAME).pak.zip $(PAK_NAME).pak
	@echo "Built $(DIST)/$(PAK_NAME).pak.zip (v$(VERSION))"

desktop:
	$(MAKE) -C ui desktop OUT=../$(BUILD)
	cd daemon && go build -o ../$(BUILD)/desktop/absd .

clean:
	rm -rf $(DIST) $(BUILD)/$(PLATFORM) $(BUILD)/desktop
