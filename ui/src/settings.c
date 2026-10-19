/* Settings screen. */
#include "common.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* ── Settings ────────────────────────────────────────────────────────────── */

typedef struct {
    const char *label;
    const char *key;
    const int *values;
    int count;
} int_setting;

static const int SKIP_BACK[] = { 5, 10, 15, 30, 45, 60 };
static const int SKIP_FWD[] = { 10, 15, 30, 45, 60, 90 };
static const int REWIND[] = { 0, 1, 2, 3, 5, 10 };

static const char *HOTKEY_HELP =
    "While a game is running (or in the NextUI menu)\n"
    "these buttons control playback:\n\n"
    "FN1 or SELECT+START  Play / pause\n"
    "SELECT+L1  Skip back\n"
    "SELECT+R1  Skip forward\n\n"
    "The game also sees these presses.\n"
    "Edit settings.json in .userdata/shared/Audiobookshelf\n"
    "to change the buttons.";

bool settings_screen(void) {
    cJSON *st = absd_status();
    if (!st) return false;
    cJSON *set = cJSON_GetObjectItem(st, "settings");

    char err[256];
    cJSON *libs = absd_call("/libraries", NULL, err, sizeof(err));
    int nlibs = libs ? cJSON_GetArraySize(libs) : 0;

    int_setting ints[] = {
        { "Skip back", "skipBack", SKIP_BACK, 6 },
        { "Skip forward", "skipForward", SKIP_FWD, 6 },
        { "Rewind on resume", "resumeRewind", REWIND, 6 },
    };
    enum { N_INTS = 3 };

    /* Option storage. Apostrophe frees/replaces values only for keyboard and
     * color items, so static strings are fine here. */
    ap_option lib_opts[32];
    int lib_sel = 0;
    for (int i = 0; i < nlibs && i < 32; i++) {
        cJSON *l = cJSON_GetArrayItem(libs, i);
        lib_opts[i] = (ap_option){ .label = jstr(l, "name"), .value = jstr(l, "id") };
        if (strcmp(jstr(l, "id"), jstr(st, "libraryId")) == 0) lib_sel = i;
    }
    if (nlibs > 32) nlibs = 32;

    char int_labels[N_INTS][6][16];
    ap_option int_opts[N_INTS][6];
    int int_sel[N_INTS];
    for (int k = 0; k < N_INTS; k++) {
        int cur = (int)jnum(set, ints[k].key);
        int_sel[k] = 0;
        for (int i = 0; i < ints[k].count; i++) {
            if (ints[k].values[i] == 0) snprintf(int_labels[k][i], 16, "Off");
            else snprintf(int_labels[k][i], 16, "%ds", ints[k].values[i]);
            int_opts[k][i] = (ap_option){ .label = int_labels[k][i], .value = int_labels[k][i] };
            if (ints[k].values[i] == cur) int_sel[k] = i;
        }
    }
    ap_option onoff[] = { { .label = "On", .value = "1" }, { .label = "Off", .value = "0" } };
    cJSON *hk = cJSON_GetObjectItem(set, "hotkeys");
    char account[300];
    snprintf(account, sizeof(account), "%s @ %s", jstr(st, "username"), jstr(st, "server"));
    ap_option account_opt[] = { { .label = "Log out", .value = "" } };
    ap_option about_opt[] = { { .label = "", .value = "" } };
    ap_option hotkey_help_opt[] = { { .label = "", .value = "" } };

    ap_options_item items[10];
    int n = 0, idx_lib = -1, idx_int = -1, idx_hk, idx_sleep, idx_account, idx_hkhelp, idx_about;
    if (nlibs > 1) {
        idx_lib = n;
        items[n++] = (ap_options_item){ .label = "Library", .type = AP_OPT_STANDARD, .options = lib_opts,
                                        .option_count = nlibs, .selected_option = lib_sel };
    }
    idx_int = n;
    for (int k = 0; k < N_INTS; k++) {
        items[n++] = (ap_options_item){ .label = ints[k].label, .type = AP_OPT_STANDARD, .options = int_opts[k],
                                        .option_count = ints[k].count, .selected_option = int_sel[k] };
    }
    idx_hk = n;
    items[n++] = (ap_options_item){ .label = "In-game controls", .type = AP_OPT_STANDARD, .options = onoff,
                                    .option_count = 2, .selected_option = jbool(hk, "enabled") ? 0 : 1 };
    idx_hkhelp = n;
    items[n++] = (ap_options_item){ .label = "In-game buttons", .type = AP_OPT_CLICKABLE, .options = hotkey_help_opt, .option_count = 1 };
    idx_sleep = n;
    items[n++] = (ap_options_item){ .label = "Pause when device sleeps", .type = AP_OPT_STANDARD, .options = onoff,
                                    .option_count = 2, .selected_option = jbool(set, "pauseOnSleep") ? 0 : 1 };
    idx_account = n;
    items[n++] = (ap_options_item){ .label = account, .type = AP_OPT_CLICKABLE, .options = account_opt, .option_count = 1 };
    idx_about = n;
    items[n++] = (ap_options_item){ .label = "About", .type = AP_OPT_CLICKABLE, .options = about_opt, .option_count = 1 };

    ap_footer_item footer[] = {
        { .button = AP_BTN_B, .label = "SAVE" },
        { .button = AP_BTN_A, .label = "SELECT", .is_confirm = true },
    };
    ap_options_list_opts o = { .title = "Settings", .items = items, .item_count = n, .footer = footer,
                               .footer_count = 2, .status_bar = &g_status_bar };
    bool logged_out = false;
    for (;;) {
        ap_options_list_result r = {0};
        int ret = ap_options_list(&o, &r);
        o.initial_selected_index = r.focused_index;
        o.visible_start_index = r.visible_start_index;
        if (ret != AP_OK) break;
        if (r.action != AP_ACTION_SELECTED) continue;
        if (r.focused_index == idx_account) {
            if (confirm("Log out?\nDownloads stay on the SD card.", "LOG OUT")) {
                absd_cmd("/logout", NULL);
                logged_out = true;
                break;
            }
        } else if (r.focused_index == idx_hkhelp) {
            show_message(HOTKEY_HELP);
        } else if (r.focused_index == idx_about) {
            char msg[512];
            snprintf(msg, sizeof(msg),
                     "Audiobookshelf for NextUI\nversion %s\n\n"
                     "Short press POWER to turn the screen off\n"
                     "while listening. Hold POWER to shut down.\n\n"
                     "Pending offline syncs: %d",
                     jstr(st, "version"), (int)jnum(st, "pendingSyncs"));
            show_message(msg);
        }
    }

    if (!logged_out) {
        cJSON *b = cJSON_CreateObject();
        cJSON *s = cJSON_AddObjectToObject(b, "settings");
        if (idx_lib >= 0) cJSON_AddStringToObject(s, "libraryId", lib_opts[items[idx_lib].selected_option].value);
        for (int k = 0; k < N_INTS; k++) {
            cJSON_AddNumberToObject(s, ints[k].key, ints[k].values[items[idx_int + k].selected_option]);
        }
        cJSON *hko = cJSON_Duplicate(hk, true);
        if (!hko) hko = cJSON_CreateObject();
        cJSON_DeleteItemFromObject(hko, "enabled");
        cJSON_AddBoolToObject(hko, "enabled", items[idx_hk].selected_option == 0);
        cJSON_AddItemToObject(s, "hotkeys", hko);
        cJSON_AddBoolToObject(s, "pauseOnSleep", items[idx_sleep].selected_option == 0);
        char *str = cJSON_PrintUnformatted(b);
        absd_cmd("/settings", str);
        free(str);
        cJSON_Delete(b);
    }
    cJSON_Delete(libs);
    cJSON_Delete(st);
    return logged_out;
}

