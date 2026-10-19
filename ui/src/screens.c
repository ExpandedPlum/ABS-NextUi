/* Browsing screens: login, book lists, details, downloads, settings. */
#include "common.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* ── Playback entry point ────────────────────────────────────────────────── */

void play_item(const char *item_id, double position, bool use_position) {
    cJSON *body = cJSON_CreateObject();
    cJSON_AddStringToObject(body, "id", item_id);
    if (use_position) cJSON_AddNumberToObject(body, "position", position);
    char *s = cJSON_PrintUnformatted(body);
    cJSON_Delete(body);
    char err[256];
    cJSON *r = call_with_spinner("Starting playback...", "/play", s, err, sizeof(err));
    free(s);
    if (!r) {
        show_message(err);
        return;
    }
    cJSON_Delete(r);
    now_playing_screen();
}

/* ── Login ───────────────────────────────────────────────────────────────── */

bool login_screen(void) {
    char server[1024] = "", user[1024] = "", pass[1024] = "";
    cJSON *st = absd_status();
    if (st) {
        snprintf(server, sizeof(server), "%s", jstr(st, "server"));
        snprintf(user, sizeof(user), "%s", jstr(st, "username"));
        cJSON_Delete(st);
    }
    int cursor = server[0] ? (user[0] ? 2 : 1) : 0;

    for (;;) {
        char masked[64];
        size_t pl = strlen(pass);
        if (pl > sizeof(masked) - 1) pl = sizeof(masked) - 1;
        memset(masked, '*', pl);
        masked[pl] = '\0';
        ap_list_item items[] = {
            { .label = "Server", .trailing_text = server[0] ? server : "not set" },
            { .label = "Username", .trailing_text = user[0] ? user : "not set" },
            { .label = "Password", .trailing_text = pass[0] ? masked : "not set" },
            { .label = "Log In" },
        };
        ap_footer_item footer[] = {
            { .button = AP_BTN_B, .label = "QUIT" },
            { .button = AP_BTN_A, .label = "EDIT", .is_confirm = true },
        };
        ap_list_opts o = ap_list_default_opts("Connect to Audiobookshelf", items, 4);
        o.footer = footer;
        o.footer_count = 2;
        o.status_bar = &g_status_bar;
        o.initial_index = cursor;
        o.help_text = "Enter your Audiobookshelf server address, e.g.\n"
                      "192.168.1.20:13378 or https://abs.example.com\n\n"
                      "Then your username and password.";
        ap_list_result r;
        if (ap_list(&o, &r) != AP_OK) return false;
        cursor = r.selected_index;

        ap_keyboard_result kb;
        switch (r.selected_index) {
            case 0:
                if (ap_url_keyboard(server, NULL, NULL, &kb) == AP_OK) snprintf(server, sizeof(server), "%s", kb.text);
                if (server[0]) cursor = 1;
                break;
            case 1:
                if (ap_keyboard(user, NULL, AP_KB_GENERAL, &kb) == AP_OK) snprintf(user, sizeof(user), "%s", kb.text);
                if (user[0]) cursor = 2;
                break;
            case 2:
                if (ap_keyboard("", NULL, AP_KB_GENERAL, &kb) == AP_OK) snprintf(pass, sizeof(pass), "%s", kb.text);
                cursor = 3;
                break;
            case 3: {
                if (!server[0] || !user[0]) {
                    show_message("Enter the server address and username first.");
                    break;
                }
                cJSON *b = cJSON_CreateObject();
                cJSON_AddStringToObject(b, "server", server);
                cJSON_AddStringToObject(b, "username", user);
                cJSON_AddStringToObject(b, "password", pass);
                char *s = cJSON_PrintUnformatted(b);
                cJSON_Delete(b);
                char err[256];
                cJSON *res = call_with_spinner("Logging in...", "/login", s, err, sizeof(err));
                free(s);
                if (res) {
                    cJSON_Delete(res);
                    return true;
                }
                char msg[400];
                snprintf(msg, sizeof(msg), "Login failed:\n%s", err);
                show_message(msg);
                break;
            }
        }
    }
}

/* ── Book lists ──────────────────────────────────────────────────────────── */

typedef struct {
    ap_list_item *items;
    char (*trail)[32];
    int count;
} book_rows;

static void build_rows(cJSON *arr, book_rows *rows) {
    int n = cJSON_GetArraySize(arr);
    rows->count = n;
    rows->items = calloc((size_t)(n > 0 ? n : 1), sizeof(*rows->items));
    rows->trail = calloc((size_t)(n > 0 ? n : 1), sizeof(*rows->trail));
    for (int i = 0; i < n; i++) {
        cJSON *b = cJSON_GetArrayItem(arr, i);
        rows->items[i].label = jstr(b, "title");
        rows->items[i].metadata = jstr(b, "id");
        char *t = rows->trail[i];
        double prog = jnum(b, "progress");
        if (jbool(b, "finished")) snprintf(t, 32, "Done");
        else if (prog > 0.005) snprintf(t, 32, "%d%%", (int)(prog * 100));
        else t[0] = '\0';
        if (jbool(b, "downloaded")) {
            size_t l = strlen(t);
            snprintf(t + l, 32 - l, "%sSD", l ? "  " : "");
        }
        rows->items[i].trailing_text = t;
    }
}

static void free_rows(book_rows *rows) {
    free(rows->items);
    free(rows->trail);
    memset(rows, 0, sizeof(*rows));
}

static const char *SORTS[] = { "title", "author", "added", "length" };
static const char *SORT_LABELS[] = { "Title", "Author", "Recently added", "Length" };

static cJSON *settings_from_status(cJSON *st) { return cJSON_GetObjectItem(st, "settings"); }

static void save_setting_string(const char *key, const char *value) {
    cJSON *b = cJSON_CreateObject();
    cJSON *s = cJSON_AddObjectToObject(b, "settings");
    cJSON_AddStringToObject(s, key, value);
    char *str = cJSON_PrintUnformatted(b);
    absd_cmd("/settings", str);
    free(str);
    cJSON_Delete(b);
}

/* Shows books from a daemon endpoint. path/body describe the request; the
 * response is either an array or {"items": [...]} */
void book_list_screen(const char *title, const char *path, const char *body, bool sortable) {
    int sort = 0;
    char req_body[1024];
    snprintf(req_body, sizeof(req_body), "%s", body ? body : "{}");
    if (sortable) {
        cJSON *st = absd_status();
        const char *cur = jstr(settings_from_status(st), "librarySort");
        for (int i = 0; i < 4; i++) if (strcmp(cur, SORTS[i]) == 0) sort = i;
        cJSON_Delete(st);
    }

    bool reload = true;
    cJSON *resp = NULL;
    book_rows rows = {0};
    int cursor = 0, top = 0;
    bool cached = false;

    for (;;) {
        if (reload) {
            if (sortable) {
                cJSON *b = cJSON_Parse(req_body);
                if (!b) b = cJSON_CreateObject();
                cJSON_DeleteItemFromObject(b, "sort");
                cJSON_AddStringToObject(b, "sort", SORTS[sort]);
                char *s = cJSON_PrintUnformatted(b);
                snprintf(req_body, sizeof(req_body), "%s", s);
                free(s);
                cJSON_Delete(b);
            }
            char err[256];
            cJSON *nr = call_with_spinner("Loading...", path, req_body, err, sizeof(err));
            if (!nr) {
                show_message(err);
                break;
            }
            cJSON_Delete(resp);
            free_rows(&rows);
            resp = nr;
            cJSON *arr = cJSON_IsArray(resp) ? resp : cJSON_GetObjectItem(resp, "items");
            cached = jbool(resp, "cached");
            build_rows(arr, &rows);
            reload = false;
        }
        if (rows.count == 0) {
            show_message("Nothing here yet.");
            break;
        }

        char full_title[160];
        if (sortable)
            snprintf(full_title, sizeof(full_title), "%s%s", title, cached ? " (offline)" : "");
        else
            snprintf(full_title, sizeof(full_title), "%s", title);
        char sort_label[48];
        snprintf(sort_label, sizeof(sort_label), "SORT: %s", SORT_LABELS[sort]);
        ap_footer_item footer[4];
        int fc = 0;
        footer[fc++] = (ap_footer_item){ .button = AP_BTN_B, .label = "BACK" };
        if (sortable) footer[fc++] = (ap_footer_item){ .button = AP_BTN_Y, .label = sort_label };
        footer[fc++] = (ap_footer_item){ .button = AP_BTN_X, .label = "PLAY" };
        footer[fc++] = (ap_footer_item){ .button = AP_BTN_A, .label = "OPEN", .is_confirm = true };

        ap_list_opts o = ap_list_default_opts(full_title, rows.items, rows.count);
        o.footer = footer;
        o.footer_count = fc;
        o.status_bar = &g_status_bar;
        o.action_button = AP_BTN_X;
        if (sortable) o.secondary_action_button = AP_BTN_Y;
        o.initial_index = cursor;
        o.visible_start_index = top;
        ap_list_result r;
        int ret = ap_list(&o, &r);
        if (ret != AP_OK) break;
        cursor = r.selected_index >= 0 ? r.selected_index : cursor;
        top = r.visible_start_index;

        if (r.action == AP_ACTION_SECONDARY_TRIGGERED) {
            sort = (sort + 1) % 4;
            save_setting_string("librarySort", SORTS[sort]);
            cursor = top = 0;
            reload = true;
            continue;
        }
        if (r.selected_index < 0) continue;
        const char *id = rows.items[r.selected_index].metadata;
        char idbuf[128];
        snprintf(idbuf, sizeof(idbuf), "%s", id);
        if (r.action == AP_ACTION_TRIGGERED) {
            play_item(idbuf, 0, false);
        } else {
            book_detail_screen(idbuf);
        }
    }
    free_rows(&rows);
    cJSON_Delete(resp);
}

/* ── Book detail ─────────────────────────────────────────────────────────── */

static void book_actions(cJSON *d) {
    const char *id = jstr(d, "id");
    cJSON *dl = cJSON_GetObjectItem(d, "download");
    bool downloaded = jbool(d, "downloaded");
    bool downloading = dl && strcmp(jstr(dl, "state"), "error") != 0;
    bool finished = jbool(d, "finished");

    enum { A_START, A_CHAPTER, A_DOWNLOAD, A_CANCEL_DL, A_DELETE_DL, A_FINISHED, A_UNFINISHED };
    ap_list_item items[6];
    int acts[6], n = 0;
    items[n] = (ap_list_item){ .label = "Play from beginning" }; acts[n++] = A_START;
    if (cJSON_GetArraySize(cJSON_GetObjectItem(d, "chapters")) > 0) {
        items[n] = (ap_list_item){ .label = "Play from chapter..." }; acts[n++] = A_CHAPTER;
    }
    if (downloaded) { items[n] = (ap_list_item){ .label = "Remove download" }; acts[n++] = A_DELETE_DL; }
    else if (downloading) { items[n] = (ap_list_item){ .label = "Cancel download" }; acts[n++] = A_CANCEL_DL; }
    else { items[n] = (ap_list_item){ .label = "Download to SD card" }; acts[n++] = A_DOWNLOAD; }
    if (finished) { items[n] = (ap_list_item){ .label = "Mark as not finished" }; acts[n++] = A_UNFINISHED; }
    else { items[n] = (ap_list_item){ .label = "Mark as finished" }; acts[n++] = A_FINISHED; }

    ap_footer_item footer[] = {
        { .button = AP_BTN_B, .label = "BACK" },
        { .button = AP_BTN_A, .label = "SELECT", .is_confirm = true },
    };
    ap_list_opts o = ap_list_default_opts(jstr(d, "title"), items, n);
    o.footer = footer;
    o.footer_count = 2;
    o.status_bar = &g_status_bar;
    ap_list_result r;
    if (ap_list(&o, &r) != AP_OK || r.selected_index < 0) return;

    char body[256], err[256];
    snprintf(body, sizeof(body), "{\"id\":\"%s\"}", id);
    switch (acts[r.selected_index]) {
        case A_START:
            play_item(id, 0, true);
            break;
        case A_CHAPTER: {
            cJSON *chs = cJSON_GetObjectItem(d, "chapters");
            int cn = cJSON_GetArraySize(chs);
            ap_list_item *ci = calloc((size_t)cn, sizeof(*ci));
            char (*tt)[24] = calloc((size_t)cn, sizeof(*tt));
            for (int i = 0; i < cn; i++) {
                cJSON *c = cJSON_GetArrayItem(chs, i);
                ci[i].label = jstr(c, "title");
                fmt_clock(jnum(c, "start"), tt[i], sizeof(tt[i]));
                ci[i].trailing_text = tt[i];
            }
            ap_list_opts co = ap_list_default_opts("Play from chapter", ci, cn);
            co.footer = footer;
            co.footer_count = 2;
            co.status_bar = &g_status_bar;
            ap_list_result cr;
            if (ap_list(&co, &cr) == AP_OK && cr.selected_index >= 0) {
                double start = jnum(cJSON_GetArrayItem(chs, cr.selected_index), "start");
                free(ci);
                free(tt);
                play_item(id, start, true);
                return;
            }
            free(ci);
            free(tt);
            break;
        }
        case A_DOWNLOAD: {
            cJSON *res = call_with_spinner("Queueing download...", "/download", body, err, sizeof(err));
            if (res) {
                cJSON_Delete(res);
                show_message("Downloading in the background.\nProgress is shown under Downloads.");
            } else {
                show_message(err);
            }
            break;
        }
        case A_CANCEL_DL:
            absd_cmd("/download/cancel", body);
            break;
        case A_DELETE_DL:
            if (confirm("Remove this book from the SD card?", "REMOVE")) {
                cJSON *res = absd_call("/download/delete", body, err, sizeof(err));
                if (res) cJSON_Delete(res);
                else show_message(err);
            }
            break;
        case A_FINISHED:
        case A_UNFINISHED:
            snprintf(body, sizeof(body), "{\"id\":\"%s\",\"finished\":%s}", id,
                     acts[r.selected_index] == A_FINISHED ? "true" : "false");
            if (!absd_cmd("/finished", body)) show_message("Couldn't update the server. Try again when online.");
            break;
    }
}

void book_detail_screen(const char *item_id) {
    char body[256];
    snprintf(body, sizeof(body), "{\"id\":\"%s\"}", item_id);
    for (;;) {
        char err[256];
        cJSON *d = call_with_spinner("Loading...", "/item", body, err, sizeof(err));
        if (!d) {
            show_message(err);
            return;
        }

        char length[32], progress[96], chapters[16], dlstate[48];
        double dur = jnum(d, "duration"), cur = jnum(d, "currentTime");
        fmt_duration(dur, length, sizeof(length));
        if (jbool(d, "finished")) {
            snprintf(progress, sizeof(progress), "Finished");
        } else if (cur > 1 && dur > 0) {
            char left[32];
            fmt_duration(dur - cur, left, sizeof(left));
            snprintf(progress, sizeof(progress), "%d%%, %s left", (int)(cur * 100 / dur), left);
        } else {
            snprintf(progress, sizeof(progress), "Not started");
        }
        snprintf(chapters, sizeof(chapters), "%d", cJSON_GetArraySize(cJSON_GetObjectItem(d, "chapters")));
        cJSON *dl = cJSON_GetObjectItem(d, "download");
        if (jbool(d, "downloaded")) snprintf(dlstate, sizeof(dlstate), "On SD card");
        else if (dl && strcmp(jstr(dl, "state"), "error") == 0) snprintf(dlstate, sizeof(dlstate), "Failed: retry from MORE");
        else if (dl) snprintf(dlstate, sizeof(dlstate), "Downloading %d%%", (int)(jnum(dl, "progress") * 100));
        else snprintf(dlstate, sizeof(dlstate), "Stream only");

        ap_detail_info_pair info[8];
        int ni = 0;
        if (jstr(d, "author")[0]) info[ni++] = (ap_detail_info_pair){ "Author", jstr(d, "author") };
        if (jstr(d, "narrator")[0]) info[ni++] = (ap_detail_info_pair){ "Narrator", jstr(d, "narrator") };
        if (jstr(d, "series")[0]) info[ni++] = (ap_detail_info_pair){ "Series", jstr(d, "series") };
        if (jstr(d, "year")[0]) info[ni++] = (ap_detail_info_pair){ "Year", jstr(d, "year") };
        info[ni++] = (ap_detail_info_pair){ "Length", length };
        info[ni++] = (ap_detail_info_pair){ "Progress", progress };
        info[ni++] = (ap_detail_info_pair){ "Chapters", chapters };
        info[ni++] = (ap_detail_info_pair){ "Offline", dlstate };

        char *desc = strip_html(jstr(d, "description"));
        ap_detail_section sections[3];
        int ns = 0;
        if (jstr(d, "cover")[0]) {
            /* Fit the cover in a square box, keeping its aspect ratio. */
            int box = UI_S(300), iw = box, ih = box;
            SDL_Surface *surf = IMG_Load(jstr(d, "cover"));
            if (surf) {
                if (surf->w > surf->h) ih = box * surf->h / surf->w;
                else iw = box * surf->w / surf->h;
                SDL_FreeSurface(surf);
            }
            sections[ns++] = (ap_detail_section){ .type = AP_SECTION_IMAGE, .image_path = jstr(d, "cover"),
                                                  .image_w = iw, .image_h = ih };
        }
        sections[ns++] = (ap_detail_section){ .type = AP_SECTION_INFO, .info_pairs = info, .info_count = ni };
        if (desc && desc[0]) {
            sections[ns++] = (ap_detail_section){ .type = AP_SECTION_DESCRIPTION, .title = "About", .description = desc };
        }

        ap_footer_item footer[] = {
            { .button = AP_BTN_B, .label = "BACK" },
            { .button = AP_BTN_Y, .label = "MORE" },
            { .button = AP_BTN_A, .label = cur > 1 && !jbool(d, "finished") ? "RESUME" : "PLAY", .is_confirm = true },
        };
        ap_detail_opts o = { .title = jstr(d, "title"), .sections = sections, .section_count = ns,
                             .footer = footer, .footer_count = 3, .status_bar = &g_status_bar };
        ap_detail_result r = {0};
        ap_detail_screen(&o, &r);
        free(desc);

        if (r.action == AP_DETAIL_ACTION) {
            char id[128];
            snprintf(id, sizeof(id), "%s", item_id);
            cJSON_Delete(d);
            play_item(id, 0, false);
            continue;
        }
        if (r.action == AP_DETAIL_SECONDARY_ACTION) {
            book_actions(d);
            cJSON_Delete(d);
            continue;
        }
        cJSON_Delete(d);
        return;
    }
}

/* ── Downloads ───────────────────────────────────────────────────────────── */

typedef struct {
    cJSON *resp;
    ap_list_item *items;
    char (*trail)[32];
    int count;
    int jobs;
    Uint32 last_refresh;
} dl_view;

static void dl_fill_job_trail(cJSON *job, char *t) {
    const char *state = jstr(job, "state");
    if (strcmp(state, "queued") == 0) snprintf(t, 32, "Queued");
    else if (strcmp(state, "error") == 0) snprintf(t, 32, "Failed");
    else snprintf(t, 32, "%d%%", (int)(jnum(job, "progress") * 100));
}

/* Live-update download percentages while the list is open. */
static void dl_footer_update(ap_list_opts *opts, int cursor, void *userdata) {
    (void)opts; (void)cursor;
    dl_view *v = userdata;
    if (v->jobs == 0) return;
    if (SDL_GetTicks() - v->last_refresh >= 1000) {
        v->last_refresh = SDL_GetTicks();
        cJSON *r = absd_call("/downloaded", NULL, NULL, 0);
        if (r) {
            cJSON *jobs = cJSON_GetObjectItem(r, "jobs");
            cJSON *done = cJSON_GetObjectItem(r, "items");
            for (int i = 0; i < v->jobs; i++) {
                const char *id = v->items[i].metadata;
                bool found = false;
                cJSON *j;
                cJSON_ArrayForEach(j, jobs) {
                    if (strcmp(jstr(j, "itemId"), id) == 0) {
                        dl_fill_job_trail(j, v->trail[i]);
                        found = true;
                    }
                }
                if (!found) {
                    cJSON *b;
                    cJSON_ArrayForEach(b, done) {
                        if (strcmp(jstr(b, "id"), id) == 0) snprintf(v->trail[i], 32, "Done");
                    }
                }
            }
            cJSON_Delete(r);
        }
    }
    ap_request_frame_in(1000);
}

void downloads_screen(void) {
    int cursor = 0;
    for (;;) {
        char err[256];
        cJSON *r = absd_call("/downloaded", NULL, err, sizeof(err));
        if (!r) {
            show_message(err);
            return;
        }
        cJSON *jobs = cJSON_GetObjectItem(r, "jobs");
        cJSON *done = cJSON_GetObjectItem(r, "items");
        dl_view v = { .resp = r, .jobs = cJSON_GetArraySize(jobs) };
        v.count = v.jobs + cJSON_GetArraySize(done);
        if (v.count == 0) {
            cJSON_Delete(r);
            show_message("No downloads yet.\n\nOpen a book, press Y (MORE) and choose\n\"Download to SD card\" to listen offline.");
            return;
        }
        v.items = calloc((size_t)v.count, sizeof(*v.items));
        v.trail = calloc((size_t)v.count, sizeof(*v.trail));
        int i = 0;
        cJSON *it;
        cJSON_ArrayForEach(it, jobs) {
            v.items[i].label = jstr(it, "title");
            v.items[i].metadata = jstr(it, "itemId");
            dl_fill_job_trail(it, v.trail[i]);
            v.items[i].trailing_text = v.trail[i];
            i++;
        }
        cJSON_ArrayForEach(it, done) {
            v.items[i].label = jstr(it, "title");
            v.items[i].metadata = jstr(it, "id");
            double p = jnum(it, "progress");
            if (jbool(it, "finished")) snprintf(v.trail[i], 32, "Done");
            else if (p > 0.005) snprintf(v.trail[i], 32, "%d%%", (int)(p * 100));
            v.items[i].trailing_text = v.trail[i];
            i++;
        }

        ap_footer_item footer[] = {
            { .button = AP_BTN_B, .label = "BACK" },
            { .button = AP_BTN_X, .label = "REMOVE" },
            { .button = AP_BTN_A, .label = "OPEN", .is_confirm = true },
        };
        ap_list_opts o = ap_list_default_opts("Downloads", v.items, v.count);
        o.footer = footer;
        o.footer_count = 3;
        o.status_bar = &g_status_bar;
        o.action_button = AP_BTN_X;
        o.initial_index = cursor < v.count ? cursor : 0;
        o.footer_update = dl_footer_update;
        o.footer_update_userdata = &v;
        ap_list_result lr;
        int ret = ap_list(&o, &lr);

        char id[128] = "";
        bool is_job = false;
        if (ret == AP_OK && lr.selected_index >= 0) {
            snprintf(id, sizeof(id), "%s", v.items[lr.selected_index].metadata);
            is_job = lr.selected_index < v.jobs;
            cursor = lr.selected_index;
        }
        int action = lr.action;
        free(v.items);
        free(v.trail);
        cJSON_Delete(r);
        if (ret != AP_OK) return;
        if (!id[0]) continue;

        char body[200];
        snprintf(body, sizeof(body), "{\"id\":\"%s\"}", id);
        if (action == AP_ACTION_TRIGGERED) {
            if (is_job) {
                absd_cmd("/download/cancel", body);
            } else if (confirm("Remove this book from the SD card?", "REMOVE")) {
                cJSON *res = absd_call("/download/delete", body, err, sizeof(err));
                if (res) cJSON_Delete(res);
                else show_message(err);
            }
        } else {
            book_detail_screen(id);
        }
    }
}

/* ── Search ──────────────────────────────────────────────────────────────── */

void search_screen(void) {
    ap_keyboard_result kb;
    if (ap_keyboard("", NULL, AP_KB_GENERAL, &kb) != AP_OK || !kb.text[0]) return;
    cJSON *st = absd_status();
    cJSON *b = cJSON_CreateObject();
    cJSON_AddStringToObject(b, "library", st ? jstr(st, "libraryId") : "");
    cJSON_AddStringToObject(b, "q", kb.text);
    cJSON_Delete(st);
    char *s = cJSON_PrintUnformatted(b);
    cJSON_Delete(b);
    char title[160];
    snprintf(title, sizeof(title), "Search: %.140s", kb.text);
    book_list_screen(title, "/search", s, false);
    free(s);
}

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
    cJSON *set = settings_from_status(st);

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

