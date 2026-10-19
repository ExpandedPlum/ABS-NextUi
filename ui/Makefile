# Builds the UI. `make` builds for the desktop (testing); `make device` is run
# inside the tg5040 toolchain container by the top-level Makefile.

SRC := src/main.c src/ipc.c src/power.c src/util.c src/screens.c src/nowplaying.c third_party/cjson/cJSON.c
HDR := $(wildcard src/*.h) third_party/apostrophe/apostrophe.h third_party/apostrophe/apostrophe_widgets.h
INC := -Isrc -Ithird_party/apostrophe -Ithird_party/cjson
WARN := -Wall -Wextra -Wno-unused-parameter -Wno-missing-field-initializers -Wno-sign-compare

OUT ?= build

.PHONY: all desktop device clean

all: desktop

desktop: $(OUT)/desktop/abs-ui

$(OUT)/desktop/abs-ui: $(SRC) $(HDR)
	@mkdir -p $(dir $@)
	$(CC) -std=gnu11 -O1 -g $(WARN) -DPLATFORM_LINUX $(INC) $(shell pkg-config --cflags sdl2) \
		-o $@ $(SRC) $(shell pkg-config --libs sdl2) -lSDL2_ttf -lSDL2_image -lpthread -lm

# Device build: uses the toolchain sysroot's SDL2 (same libraries NextUI ships).
device: $(OUT)/tg5040/abs-ui

$(OUT)/tg5040/abs-ui: $(SRC) $(HDR)
	@mkdir -p $(dir $@)
	$(CROSS_COMPILE)gcc -std=gnu11 -O2 $(WARN) -mcpu=cortex-a53 -DPLATFORM_TG5040 $(INC) \
		$(shell pkg-config --cflags sdl2) -o $@ $(SRC) \
		$(filter-out -Wl$(comma)-rpath$(comma)%,$(shell pkg-config --libs sdl2)) \
		-lSDL2_ttf -lSDL2_image -lpthread -ldl -lm
	$(CROSS_COMPILE)strip $@

comma := ,

clean:
	rm -rf $(OUT)
