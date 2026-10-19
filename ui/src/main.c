/* Audiobookshelf for NextUI: the on-screen front end.
 *
 * All playback, networking and syncing happens in the absd daemon, which
 * keeps running after this UI exits so books play on in the background
 * (including behind games). This program is the remote control.
 */
#define AP_IMPLEMENTATION
#include "apostrophe.h"
#define AP_WIDGETS_IMPLEMENTATION
#include "apostrophe_widgets.h"

#include "common.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

ap_status_bar_opts g_status_bar = { .show_battery = true, .show_wifi = true };

static int wait_worker(void *userdata) {
    (void)userdata;
    for (int i = 0; i < 50; i++) {
        cJSON *st = absd_status();
        if (st) {
            cJSON_Delete(st);
            return AP_OK;
        }
        usleep(100 * 1000);
    }
    return AP_ERROR;
}

/* launch.sh starts the daemon just before us; give it a moment. */
static bool wait_for_daemon(void) {
    cJSON *st = absd_status();
    if (st) {
        cJSON_Delete(st);
        return true;
    }
    ap_process_opts o = { .message = "Starting..." };
    return ap_process_message(&o, wait_worker, NULL) == AP_OK;
}

static cJSON *g_libraries; /* cached /libraries response */

static cJSON *libraries(bool refresh) {
    if (g_libraries && !refresh) return g_libraries;
    char err[256];
    cJSON *libs = g_libraries ? absd_call("/libraries", NULL, err, sizeof(err))
                              : call_with_spinner("Loading libraries...", "/libraries", NULL, err, sizeof(err));
    if (libs) {
        cJSON_Delete(g_libraries);
        g_libraries = libs;
    }
    return g_libraries;
}

static void save_library(const char *id) {
    char *b = jbody("libraryId", id, NULL);
    char body[300];
    snprintf(body, sizeof(body), "{\"settings\":%s}", b);
    free(b);
    absd_cmd("/settings", body);
}

/* The library the home screen shows; picks the first one if none is saved
 * (or the saved one disappeared). */
static cJSON *current_library(cJSON *st) {
    cJSON *libs = libraries(false);
    cJSON *l;
    cJSON_ArrayForEach(l, libs) {
        if (strcmp(jstr(l, "id"), jstr(st, "libraryId")) == 0) return l;
    }
    l = cJSON_GetArrayItem(libs, 0);
    if (l) save_library(jstr(l, "id"));
    return l;
}

static void switch_library(void) {
    cJSON *libs = libraries(true);
    int n = cJSON_GetArraySize(libs);
    if (n < 2) return;
    ap_list_item *items = calloc((size_t)n, sizeof(*items));
    for (int i = 0; i < n; i++) {
        cJSON *l = cJSON_GetArrayItem(libs, i);
        items[i] = (ap_list_item){ .label = jstr(l, "name"),
                                   .trailing_text = strcmp(jstr(l, "mediaType"), "podcast") == 0 ? "Podcasts" : "Books" };
    }
    ap_footer_item footer[] = {
        { .button = AP_BTN_B, .label = "BACK" },
        { .button = AP_BTN_A, .label = "SWITCH", .is_confirm = true },
    };
    ap_list_opts o = ap_list_default_opts("Libraries", items, n);
    o.footer = footer;
    o.footer_count = 2;
    o.status_bar = &g_status_bar;
    ap_list_result r;
    if (ap_list(&o, &r) == AP_OK && r.selected_index >= 0)
        save_library(jstr(cJSON_GetArrayItem(libs, r.selected_index), "id"));
    free(items);
}

enum { H_NOW, H_CONTINUE, H_LIBRARY, H_SERIES, H_AUTHORS, H_COLLECTIONS, H_PLAYLISTS, H_RECENT, H_EPISODES,
       H_SEARCH, H_DOWNLOADS, H_SETTINGS };

/* Returns when the user backs out of the app. */
static void home_screen(void) {
    int cursor = 0;
    for (;;) {
        cJSON *st = absd_status();
        if (!st) {
            show_message("The player service stopped unexpectedly.\nCheck .userdata/tg5040/logs for details.");
            return;
        }
        if (!jbool(st, "loggedIn")) {
            cJSON_Delete(st);
            cJSON_Delete(g_libraries);
            g_libraries = NULL;
            if (!login_screen()) return;
            continue;
        }
        cJSON *lib = current_library(st);
        char lib_id[128], lib_name[128];
        snprintf(lib_id, sizeof(lib_id), "%s", lib ? jstr(lib, "id") : "");
        snprintf(lib_name, sizeof(lib_name), "%s", lib ? jstr(lib, "name") : APP_NAME);
        bool podcasts = lib && strcmp(jstr(lib, "mediaType"), "podcast") == 0;
        bool many_libs = cJSON_GetArraySize(g_libraries) > 1;

        cJSON *p = cJSON_GetObjectItem(st, "player");
        bool loaded = jstr(p, "itemId")[0] != '\0';
        bool online = jbool(st, "online");
        int active_dl = cJSON_GetArraySize(cJSON_GetObjectItem(st, "downloads"));

        char now_trail[96], dl_trail[32];
        snprintf(now_trail, sizeof(now_trail), "%s", jstr(p, "title"));
        snprintf(dl_trail, sizeof(dl_trail), active_dl ? "%d active" : "", active_dl);
        const char *offline = online ? NULL : "offline";

        ap_list_item items[14];
        int acts[14], n = 0;
#define ROW(act, ...) do { items[n] = (ap_list_item){ __VA_ARGS__ }; acts[n++] = (act); } while (0)
        if (loaded)
            ROW(H_NOW, .label = strcmp(jstr(p, "state"), "playing") == 0 ? "Now Playing" : "Paused",
                .trailing_text = now_trail);
        ROW(H_CONTINUE, .label = "Continue Listening");
        if (podcasts) {
            ROW(H_LIBRARY, .label = "Podcasts", .trailing_text = offline);
            ROW(H_EPISODES, .label = "Latest Episodes");
            ROW(H_PLAYLISTS, .label = "Playlists");
        } else {
            ROW(H_LIBRARY, .label = "Library", .trailing_text = offline);
            ROW(H_SERIES, .label = "Series");
            ROW(H_AUTHORS, .label = "Authors");
            ROW(H_COLLECTIONS, .label = "Collections");
            ROW(H_PLAYLISTS, .label = "Playlists");
            ROW(H_RECENT, .label = "Recently Added");
        }
        ROW(H_SEARCH, .label = "Search");
        ROW(H_DOWNLOADS, .label = "Downloads", .trailing_text = dl_trail);
        ROW(H_SETTINGS, .label = "Settings");
#undef ROW

        ap_footer_item footer[4];
        int fc = 0;
        footer[fc++] = (ap_footer_item){ .button = AP_BTN_B, .label = "EXIT" };
        if (many_libs) footer[fc++] = (ap_footer_item){ .button = AP_BTN_Y, .label = "LIBRARY" };
        if (loaded) footer[fc++] = (ap_footer_item){ .button = AP_BTN_X, .label = "PLAY/PAUSE" };
        footer[fc++] = (ap_footer_item){ .button = AP_BTN_A, .label = "OPEN", .is_confirm = true };

        ap_list_opts o = ap_list_default_opts(lib_name, items, n);
        o.footer = footer;
        o.footer_count = fc;
        o.status_bar = &g_status_bar;
        o.action_button = loaded ? AP_BTN_X : AP_BTN_NONE;
        o.secondary_action_button = many_libs ? AP_BTN_Y : AP_BTN_NONE;
        o.initial_index = cursor < n ? cursor : 0;
        o.help_text = "POWER (short)  Screen off, audio keeps playing\n"
                      "POWER (hold)   Save progress and shut down\n"
                      "Y              Switch library\n\n"
                      "Exit with B and choose \"Keep listening\" to\n"
                      "play in the background, even during games.";
        char *lib_body = jbody("library", lib_id, NULL);
        char *recent_body = jbody("library", lib_id, "sort", "added", NULL);
        cJSON_Delete(st);

        ap_list_result r;
        int ret = ap_list(&o, &r);
        if (ret == AP_OK) {
            cursor = r.selected_index;
            if (r.action == AP_ACTION_TRIGGERED) {
                absd_cmd("/toggle", NULL);
            } else if (r.action == AP_ACTION_SECONDARY_TRIGGERED) {
                switch_library();
                cursor = 0;
            } else {
                switch (acts[r.selected_index]) {
                    case H_NOW: now_playing_screen(); break;
                    case H_CONTINUE: entry_list_screen("Continue Listening", "/continue", "{}", 0); break;
                    case H_LIBRARY: entry_list_screen(lib_name, "/library", lib_body, LIST_SORTABLE); break;
                    case H_SERIES: entry_list_screen("Series", "/series", lib_body, 0); break;
                    case H_AUTHORS: entry_list_screen("Authors", "/authors", lib_body, 0); break;
                    case H_COLLECTIONS: entry_list_screen("Collections", "/collections", lib_body, 0); break;
                    case H_PLAYLISTS: entry_list_screen("Playlists", "/playlists", lib_body, 0); break;
                    case H_RECENT: entry_list_screen("Recently Added", "/library", recent_body, 0); break;
                    case H_EPISODES: entry_list_screen("Latest Episodes", "/episodes/recent", lib_body, 0); break;
                    case H_SEARCH: search_screen(lib_id); break;
                    case H_DOWNLOADS: downloads_screen(); break;
                    case H_SETTINGS:
                        if (settings_screen()) cursor = 0;
                        libraries(true);
                        break;
                }
            }
        }
        free(lib_body);
        free(recent_body);
        if (ret != AP_OK) return;
    }
}

/* If a book is loaded, ask whether to keep it playing after we exit. */
static void exit_choice(void) {
    cJSON *st = absd_status();
    if (!st) return;
    cJSON *p = cJSON_GetObjectItem(st, "player");
    bool loaded = jstr(p, "itemId")[0] != '\0';
    bool playing = strcmp(jstr(p, "state"), "playing") == 0 || strcmp(jstr(p, "state"), "loading") == 0;
    bool downloading = cJSON_GetArraySize(cJSON_GetObjectItem(st, "downloads")) > 0;
    cJSON_Delete(st);
    if (!loaded) {
        if (!downloading) absd_cmd("/quit", NULL);
        return;
    }
    ap_list_item items[] = {
        { .label = playing ? "Keep listening" : "Keep paused (FN1 resumes in games)" },
        { .label = "Stop playback and exit" },
    };
    ap_footer_item footer[] = {
        { .button = AP_BTN_B, .label = "CANCEL" },
        { .button = AP_BTN_A, .label = "EXIT", .is_confirm = true },
    };
    ap_list_opts o = ap_list_default_opts("Exit Audiobookshelf", items, 2);
    o.footer = footer;
    o.footer_count = 2;
    o.status_bar = &g_status_bar;
    o.help_text = "Keep listening: the book keeps playing after you\n"
                  "leave, including while you play a game.\n\n"
                  "In-game: FN1 or SELECT+START pauses/resumes,\n"
                  "SELECT+L1 / SELECT+R1 skip back/forward.";
    ap_list_result r;
    if (ap_list(&o, &r) != AP_OK) {
        home_screen();
        exit_choice();
        return;
    }
    if (r.selected_index == 1) {
        absd_cmd("/stop", NULL);
        if (!downloading) absd_cmd("/quit", NULL);
    }
}

int main(int argc, char *argv[]) {
    (void)argc;
    (void)argv;
    ap_config cfg = {
        .window_title = APP_NAME,
        .log_path = ap_resolve_log_path("Audiobookshelf"),
        .is_nextui = AP_PLATFORM_IS_DEVICE,
        .cpu_speed = AP_CPU_SPEED_NORMAL,
    };
    if (ap_init(&cfg) != AP_OK) {
        fprintf(stderr, "Failed to initialise UI: %s\n", ap_get_error());
        return 1;
    }
    power_init();

    if (!wait_for_daemon()) {
        show_message("Couldn't start the player service.\nCheck .userdata/tg5040/logs/Audiobookshelf-daemon.txt");
    } else {
        home_screen();
        exit_choice();
    }

    power_shutdown();
    ap_quit();
    return 0;
}
