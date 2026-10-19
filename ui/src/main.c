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

/* Pick a library the first time (or if the saved one disappeared). */
static void ensure_library(cJSON *st) {
    if (jstr(st, "libraryId")[0]) return;
    char err[256];
    cJSON *libs = call_with_spinner("Loading libraries...", "/libraries", NULL, err, sizeof(err));
    if (!libs) return;
    cJSON *first = cJSON_GetArrayItem(libs, 0);
    if (first) {
        cJSON *b = cJSON_CreateObject();
        cJSON *s = cJSON_AddObjectToObject(b, "settings");
        cJSON_AddStringToObject(s, "libraryId", jstr(first, "id"));
        char *str = cJSON_PrintUnformatted(b);
        absd_cmd("/settings", str);
        free(str);
        cJSON_Delete(b);
    }
    cJSON_Delete(libs);
}

enum { H_NOW, H_CONTINUE, H_LIBRARY, H_RECENT, H_SEARCH, H_DOWNLOADS, H_SETTINGS };

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
            if (!login_screen()) return;
            continue;
        }
        ensure_library(st);

        cJSON *p = cJSON_GetObjectItem(st, "player");
        bool loaded = jstr(p, "itemId")[0] != '\0';
        bool online = jbool(st, "online");
        int active_dl = cJSON_GetArraySize(cJSON_GetObjectItem(st, "downloads"));

        char now_trail[96], dl_trail[32];
        snprintf(now_trail, sizeof(now_trail), "%s", jstr(p, "title"));
        snprintf(dl_trail, sizeof(dl_trail), active_dl ? "%d active" : "", active_dl);

        ap_list_item items[8];
        int acts[8], n = 0;
        if (loaded) {
            items[n] = (ap_list_item){ .label = strcmp(jstr(p, "state"), "playing") == 0 ? "Now Playing" : "Paused",
                                       .trailing_text = now_trail };
            acts[n++] = H_NOW;
        }
        items[n] = (ap_list_item){ .label = "Continue Listening" }; acts[n++] = H_CONTINUE;
        items[n] = (ap_list_item){ .label = "Library", .trailing_text = online ? NULL : "offline" }; acts[n++] = H_LIBRARY;
        items[n] = (ap_list_item){ .label = "Recently Added" }; acts[n++] = H_RECENT;
        items[n] = (ap_list_item){ .label = "Search" }; acts[n++] = H_SEARCH;
        items[n] = (ap_list_item){ .label = "Downloads", .trailing_text = dl_trail }; acts[n++] = H_DOWNLOADS;
        items[n] = (ap_list_item){ .label = "Settings" }; acts[n++] = H_SETTINGS;

        ap_footer_item footer[3];
        int fc = 0;
        footer[fc++] = (ap_footer_item){ .button = AP_BTN_B, .label = "EXIT" };
        if (loaded) footer[fc++] = (ap_footer_item){ .button = AP_BTN_X, .label = "PLAY/PAUSE" };
        footer[fc++] = (ap_footer_item){ .button = AP_BTN_A, .label = "OPEN", .is_confirm = true };

        ap_list_opts o = ap_list_default_opts(APP_NAME, items, n);
        o.footer = footer;
        o.footer_count = fc;
        o.status_bar = &g_status_bar;
        o.action_button = loaded ? AP_BTN_X : AP_BTN_NONE;
        o.initial_index = cursor < n ? cursor : 0;
        o.help_text = "POWER (short)  Screen off, audio keeps playing\n"
                      "POWER (hold)   Save progress and shut down\n\n"
                      "Exit with B and choose \"Keep listening\" to\n"
                      "play in the background, even during games.";
        char library_body[256];
        snprintf(library_body, sizeof(library_body), "{\"library\":\"%s\"}", jstr(st, "libraryId"));
        char recent_body[300];
        snprintf(recent_body, sizeof(recent_body), "{\"library\":\"%s\",\"sort\":\"added\"}", jstr(st, "libraryId"));
        cJSON_Delete(st);

        ap_list_result r;
        if (ap_list(&o, &r) != AP_OK) return;
        cursor = r.selected_index;
        if (r.action == AP_ACTION_TRIGGERED) {
            absd_cmd("/toggle", NULL);
            continue;
        }
        switch (acts[r.selected_index]) {
            case H_NOW: now_playing_screen(); break;
            case H_CONTINUE: book_list_screen("Continue Listening", "/continue", "{}", false); break;
            case H_LIBRARY: book_list_screen("Library", "/library", library_body, true); break;
            case H_RECENT: book_list_screen("Recently Added", "/library", recent_body, false); break;
            case H_SEARCH: search_screen(); break;
            case H_DOWNLOADS: downloads_screen(); break;
            case H_SETTINGS:
                if (settings_screen()) cursor = 0;
                break;
        }
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
        { .label = playing ? "Keep listening" : "Keep paused (resume in-game with FN1)" },
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
