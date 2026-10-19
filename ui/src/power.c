/* Power button handling.
 *
 * NextUI paks normally suspend the device on a short power press, which would
 * stop the audio. Instead a short press turns the backlight off (and back on);
 * the CPU is clocked down while dark and all button input is ignored so
 * pocket presses do nothing. Volume buttons keep working because NextUI's
 * keymon reads them directly. A long press saves progress and powers off.
 */
#include "common.h"

#include <fcntl.h>
#include <linux/input.h>
#include <poll.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <unistd.h>

#define DISP_LCD_SET_BRIGHTNESS 0x102
#define DISP_LCD_GET_BRIGHTNESS 0x103
#define LONG_PRESS_MS 1500

static atomic_bool g_screen_off;
static atomic_bool g_running;
static pthread_t g_power_thread, g_heartbeat_thread;
static bool g_power_started, g_heartbeat_started;
static Uint32 g_wake_event;

bool screen_is_off(void) { return atomic_load(&g_screen_off); }

#if AP_PLATFORM_IS_DEVICE
static int g_saved_brightness = -1;

static int disp_ioctl(unsigned long cmd, unsigned long value) {
    int fd = open("/dev/disp", O_RDWR);
    if (fd < 0) return -1;
    unsigned long param[4] = { 0, value, 0, 0 };
    int r = ioctl(fd, cmd, &param);
    close(fd);
    return r;
}

static bool is_brick(void) {
    const char *d = getenv("DEVICE");
    return d && (strcmp(d, "brick") == 0 || strcmp(d, "brickpro") == 0);
}
#endif

void screen_set_off(bool off) {
    if (atomic_load(&g_screen_off) == off) return;
#if AP_PLATFORM_IS_DEVICE
    if (off) {
        int cur = disp_ioctl(DISP_LCD_GET_BRIGHTNESS, 0);
        if (cur > 0) g_saved_brightness = cur;
        disp_ioctl(DISP_LCD_SET_BRIGHTNESS, 0);
        ap_set_cpu_speed(AP_CPU_SPEED_MENU);
    } else {
        ap_set_cpu_speed(AP_CPU_SPEED_NORMAL);
        /* Mirrors NextUI's PLAT_enableBacklight: the Brick panel needs a
         * nudge before restoring the real level. */
        if (is_brick()) disp_ioctl(DISP_LCD_SET_BRIGHTNESS, 8);
        disp_ioctl(DISP_LCD_SET_BRIGHTNESS, g_saved_brightness > 0 ? (unsigned long)g_saved_brightness : 128);
    }
#endif
    atomic_store(&g_screen_off, off);
    ap_log("screen %s", off ? "off" : "on");
    if (!off) {
        /* Wake the UI thread so it redraws immediately. */
        SDL_Event ev = {0};
        ev.type = g_wake_event;
        SDL_PushEvent(&ev);
        ap_request_frame();
    }
}

/* Drop all button input while the screen is dark. */
static int event_filter(void *userdata, SDL_Event *e) {
    (void)userdata;
    if (!atomic_load(&g_screen_off)) return 1;
    switch (e->type) {
        case SDL_KEYDOWN: case SDL_KEYUP:
        case SDL_JOYBUTTONDOWN: case SDL_JOYBUTTONUP:
        case SDL_JOYAXISMOTION: case SDL_JOYHATMOTION:
        case SDL_CONTROLLERBUTTONDOWN: case SDL_CONTROLLERBUTTONUP:
        case SDL_CONTROLLERAXISMOTION:
            return 0;
    }
    return 1;
}

#if AP_PLATFORM_IS_DEVICE
static void power_off_now(void) {
    ap_log("long power press: saving progress and powering off");
    if (screen_is_off()) screen_set_off(false);
    absd_cmd("/stop", NULL);
    absd_cmd("/quit", NULL);
    absd_cmd("/ui", "{\"foreground\":false}");
    if (system("rm -f /tmp/nextui_exec && sync") != 0) ap_log("could not clear nextui_exec");
    FILE *f = fopen("/tmp/poweroff", "w");
    if (f) fclose(f);
    sync();
    exit(0);
}

static int open_power_device(void) {
    unsigned char bits[(KEY_MAX + 1) / 8];
    for (int i = 0; i < 16; i++) {
        char path[32];
        snprintf(path, sizeof(path), "/dev/input/event%d", i);
        int fd = open(path, O_RDONLY | O_NONBLOCK);
        if (fd < 0) continue;
        memset(bits, 0, sizeof(bits));
        if (ioctl(fd, EVIOCGBIT(EV_KEY, sizeof(bits)), bits) >= 0 &&
            (bits[KEY_POWER / 8] & (1 << (KEY_POWER % 8)))) {
            ap_log("power: using %s", path);
            return fd;
        }
        close(fd);
    }
    return -1;
}

static void *power_thread(void *arg) {
    (void)arg;
    int fd = open_power_device();
    if (fd < 0) {
        ap_log("power: no KEY_POWER device; screen-off unavailable");
        return NULL;
    }
    Uint32 pressed_at = 0;
    bool long_fired = false;
    while (atomic_load(&g_running)) {
        struct pollfd p = { .fd = fd, .events = POLLIN };
        int pr = poll(&p, 1, 100);
        if (pressed_at && !long_fired && SDL_GetTicks() - pressed_at >= LONG_PRESS_MS) {
            long_fired = true;
            power_off_now();
        }
        if (pr <= 0) continue;
        struct input_event ev;
        while (read(fd, &ev, sizeof(ev)) == (ssize_t)sizeof(ev)) {
            if (ev.type != EV_KEY || ev.code != KEY_POWER) continue;
            if (ev.value == 1) {
                pressed_at = SDL_GetTicks();
                long_fired = false;
            } else if (ev.value == 0 && pressed_at) {
                if (!long_fired) screen_set_off(!screen_is_off());
                pressed_at = 0;
            }
        }
    }
    close(fd);
    return NULL;
}
#endif

/* Tell the daemon the UI is in front so in-game hotkeys stay quiet. */
static void *heartbeat_thread(void *arg) {
    (void)arg;
    while (atomic_load(&g_running)) {
        absd_cmd("/ui", "{\"foreground\":true}");
        for (int i = 0; i < 20 && atomic_load(&g_running); i++) usleep(100 * 1000);
    }
    return NULL;
}

void power_init(void) {
    atomic_store(&g_running, true);
    g_wake_event = SDL_RegisterEvents(1);
    SDL_SetEventFilter(event_filter, NULL);
    /* Replace Apostrophe's suspend-on-power handler with ours. */
    ap_set_power_handler(false);
#if AP_PLATFORM_IS_DEVICE
    g_power_started = pthread_create(&g_power_thread, NULL, power_thread, NULL) == 0;
#endif
    g_heartbeat_started = pthread_create(&g_heartbeat_thread, NULL, heartbeat_thread, NULL) == 0;
}

void power_shutdown(void) {
    if (screen_is_off()) screen_set_off(false);
    atomic_store(&g_running, false);
    if (g_power_started) pthread_join(g_power_thread, NULL);
    if (g_heartbeat_started) pthread_join(g_heartbeat_thread, NULL);
    absd_cmd("/ui", "{\"foreground\":false}");
}
