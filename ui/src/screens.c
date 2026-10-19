/* Browsing screens: login, lists, details, downloads, search. */
#include "common.h"

#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Builds a JSON object from key/value string pairs, NULL-terminated. Empty
 * values are left out. Caller frees. */
char *jbody(const char *key, ...) {
    cJSON *o = cJSON_CreateObject();
    va_list ap;
    va_start(ap, key);
    for (const char *k = key; k; k = va_arg(ap, const char *)) {
        const char *v = va_arg(ap, const char *);
        if (v && v[0]) cJSON_AddStringToObject(o, k, v);
    }
    va_end(ap);
    char *s = cJSON_PrintUnformatted(o);
    cJSON_Delete(o);
    return s;
}

/* ── Playback entry point ────────────────────────────────────────────────── */

void play_item(const char *item_id, const char *episode_id, double position, bool use_position) {
    cJSON *body = cJSON_CreateObject();
    cJSON_AddStringToObject(body, "id", item_id);
    if (episode_id && episode_id[0]) cJSON_AddStringToObject(body, "episodeId", episode_id);
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

/* Shows a QR code for the daemon's one-time login page and waits until the
 * phone has signed in. */
static bool phone_login(void) {
    char err[256];
    cJSON *p = call_with_spinner("Preparing...", "/login/phone", "{}", err, sizeof(err));
    if (!p) {
        char msg[320];
        snprintf(msg, sizeof(msg), "Phone sign-in isn't available:\n%s", err);
        show_message(msg);
        return false;
    }
    char url[256];
    snprintf(url, sizeof(url), "%s", jstr(p, "url"));
    SDL_Texture *qr = ap_load_image(jstr(p, "qr"));
    cJSON_Delete(p);

    ap_theme *t = ap_get_theme();
    ap_footer_item footer[] = { { .button = AP_BTN_B, .label = "CANCEL" } };
    bool logged_in = false, running = true;
    Uint32 last_poll = 0;
    char message[128] = "";
    while (running) {
        ap_input_event ev;
        while (ap_poll_input(&ev)) {
            if (ev.pressed && ev.button == AP_BTN_B) running = false;
        }
        if (!running) break;
        if (SDL_GetTicks() - last_poll > 1000) {
            last_poll = SDL_GetTicks();
            cJSON *st = absd_status();
            if (st) {
                logged_in = jbool(st, "loggedIn");
                cJSON *ph = cJSON_GetObjectItem(st, "phoneLogin");
                snprintf(message, sizeof(message), "%s", ph ? jstr(ph, "message") : "");
                if (!ph && !logged_in) running = false; /* timed out */
                cJSON_Delete(st);
            }
            if (logged_in) break;
        }
        if (screen_is_off()) {
            SDL_WaitEventTimeout(NULL, 500);
            continue;
        }
        ap_clear_screen();
        ap_draw_screen_title("Sign in from your phone", &g_status_bar);
        ap_draw_status_bar(&g_status_bar);
        SDL_Rect cr = ap_get_content_rect(true, true, true);
        int pad = UI_DS(8);
        int qs = cr.h - pad * 2;
        if (qs > cr.w / 2) qs = cr.w / 2;
        int qx = cr.x + pad, qy = cr.y + (cr.h - qs) / 2;
        ap_draw_rect(qx, qy, qs, qs, (ap_color){ 255, 255, 255, 255 });
        if (qr) ap_draw_image(qr, qx, qy, qs, qs);
        int x = qx + qs + pad * 3, w = cr.x + cr.w - x - pad;
        TTF_Font *sm = ap_get_font(AP_FONT_SMALL), *ti = ap_get_font(AP_FONT_TINY);
        int y = qy;
        y += ap_draw_text_wrapped(sm, "Scan the code with your phone (same Wi-Fi), or open:", x, y, w, t->text, AP_ALIGN_LEFT);
        y += pad;
        /* URLs have no spaces to wrap at; split after the host if needed. */
        const char *shown = strncmp(url, "http://", 7) == 0 ? url + 7 : url;
        const char *slash = strrchr(shown, '/');
        if (ap_measure_text(sm, shown) <= w || !slash) {
            ap_draw_text_ellipsized(sm, shown, x, y, t->text, w);
            y += TTF_FontHeight(sm);
        } else {
            char host[128];
            snprintf(host, sizeof(host), "%.*s", (int)(slash - shown + 1), shown);
            ap_draw_text_ellipsized(sm, host, x, y, t->text, w);
            y += TTF_FontHeight(sm);
            ap_draw_text_ellipsized(sm, slash + 1, x, y, t->text, w);
            y += TTF_FontHeight(sm);
        }
        y += pad * 2;
        y += ap_draw_text_wrapped(ti, "Then enter your server and either your password or an API key.", x, y, w, t->hint, AP_ALIGN_LEFT);
        if (message[0]) {
            y += pad * 2;
            ap_draw_text_wrapped(sm, message, x, y, w, t->text, AP_ALIGN_LEFT);
        }
        ap_draw_footer(footer, 1);
        ap_present();
        SDL_WaitEventTimeout(NULL, 250);
    }
    if (qr) SDL_DestroyTexture(qr);
    absd_cmd("/login/phone/stop", NULL);
    return logged_in;
}

static bool submit_login(const char *server, const char *user, const char *pass, const char *apikey) {
    cJSON *b = cJSON_CreateObject();
    cJSON_AddStringToObject(b, "server", server);
    if (apikey) {
        cJSON_AddStringToObject(b, "apiKey", apikey);
    } else {
        cJSON_AddStringToObject(b, "username", user);
        cJSON_AddStringToObject(b, "password", pass);
    }
    char *s = cJSON_PrintUnformatted(b);
    cJSON_Delete(b);
    char err[256];
    cJSON *res = call_with_spinner("Signing in...", "/login", s, err, sizeof(err));
    free(s);
    if (res) {
        cJSON_Delete(res);
        return true;
    }
    char msg[400];
    snprintf(msg, sizeof(msg), "Sign-in failed:\n%s", err);
    show_message(msg);
    return false;
}

bool login_screen(void) {
    char server[1024] = "", user[1024] = "", pass[1024] = "";
    cJSON *st = absd_status();
    if (st) {
        snprintf(server, sizeof(server), "%s", jstr(st, "server"));
        snprintf(user, sizeof(user), "%s", jstr(st, "username"));
        cJSON_Delete(st);
    }
    enum { R_PHONE, R_SERVER, R_USER, R_PASS, R_LOGIN, R_APIKEY };
    int cursor = 0;

    for (;;) {
        char masked[64];
        size_t pl = strlen(pass);
        if (pl > sizeof(masked) - 1) pl = sizeof(masked) - 1;
        memset(masked, '*', pl);
        masked[pl] = '\0';
        ap_list_item items[] = {
            { .label = "Sign in from your phone", .trailing_text = "QR code" },
            { .label = "Server", .trailing_text = server[0] ? server : "not set" },
            { .label = "Username", .trailing_text = user[0] ? user : "not set" },
            { .label = "Password", .trailing_text = pass[0] ? masked : "not set" },
            { .label = "Sign In" },
            { .label = "Use an API key instead", .trailing_text = "SSO accounts" },
        };
        ap_footer_item footer[] = {
            { .button = AP_BTN_B, .label = "QUIT" },
            { .button = AP_BTN_A, .label = "SELECT", .is_confirm = true },
        };
        ap_list_opts o = ap_list_default_opts("Connect to Audiobookshelf", items, 6);
        o.footer = footer;
        o.footer_count = 2;
        o.status_bar = &g_status_bar;
        o.initial_index = cursor;
        o.help_text = "Easiest: \"Sign in from your phone\" shows a QR code.\n"
                      "Open it on a phone on the same Wi-Fi and type\n"
                      "your details there.\n\n"
                      "Accounts that sign in with SSO/OpenID can use an\n"
                      "API key (server Settings > API Keys).";
        ap_list_result r;
        if (ap_list(&o, &r) != AP_OK) return false;
        cursor = r.selected_index;

        ap_keyboard_result kb;
        switch (r.selected_index) {
            case R_PHONE:
                if (phone_login()) return true;
                break;
            case R_SERVER:
                if (ap_url_keyboard(server, NULL, NULL, &kb) == AP_OK) snprintf(server, sizeof(server), "%s", kb.text);
                if (server[0]) cursor = R_USER;
                break;
            case R_USER:
                if (ap_keyboard(user, NULL, AP_KB_GENERAL, &kb) == AP_OK) snprintf(user, sizeof(user), "%s", kb.text);
                if (user[0]) cursor = R_PASS;
                break;
            case R_PASS:
                if (ap_keyboard("", NULL, AP_KB_GENERAL, &kb) == AP_OK) snprintf(pass, sizeof(pass), "%s", kb.text);
                cursor = R_LOGIN;
                break;
            case R_LOGIN:
                if (!server[0] || !user[0]) {
                    show_message("Enter the server address and username first.");
                    break;
                }
                if (submit_login(server, user, pass, NULL)) return true;
                break;
            case R_APIKEY:
                if (!server[0]) {
                    show_message("Enter the server address first.");
                    cursor = R_SERVER;
                    break;
                }
                if (ap_keyboard("", NULL, AP_KB_GENERAL, &kb) == AP_OK && kb.text[0] &&
                    submit_login(server, NULL, NULL, kb.text))
                    return true;
                break;
        }
    }
}

/* ── Entry lists ─────────────────────────────────────────────────────────── */

static bool is_playable(cJSON *e) {
    const char *k = jstr(e, "kind");
    return strcmp(k, "book") == 0 || strcmp(k, "episode") == 0;
}

static void entry_trailing(cJSON *e, bool mixed, char *t, size_t n) {
    const char *k = jstr(e, "kind");
    int c = (int)jnum(e, "count");
    t[0] = '\0';
    if (is_playable(e)) {
        double prog = jnum(e, "progress");
        if (jbool(e, "finished")) snprintf(t, n, "Done");
        else if (prog > 0.005) snprintf(t, n, "%d%%", (int)(prog * 100));
        if (jbool(e, "downloaded")) {
            size_t l = strlen(t);
            snprintf(t + l, n - l, "%sSD", l ? "  " : "");
        }
    } else if (strcmp(k, "podcast") == 0) {
        snprintf(t, n, "%d episode%s", c, c == 1 ? "" : "s");
    } else if (strcmp(k, "playlist") == 0) {
        snprintf(t, n, "%d item%s", c, c == 1 ? "" : "s");
    } else if (mixed && strcmp(k, "author") == 0) {
        snprintf(t, n, "Author");
    } else if (mixed && strcmp(k, "series") == 0) {
        snprintf(t, n, "Series");
    } else {
        snprintf(t, n, "%d book%s", c, c == 1 ? "" : "s");
    }
}

/* Opens an entry: playable ones show their details (or play when play is
 * set); containers open their own list. */
static void open_entry(cJSON *e, bool play) {
    const char *k = jstr(e, "kind");
    const char *id = jstr(e, "id");
    char *b = NULL;
    if (is_playable(e)) {
        if (play) play_item(id, jstr(e, "episodeId"), 0, false);
        else item_detail_screen(id, jstr(e, "episodeId"));
        return;
    }
    if (strcmp(k, "podcast") == 0) {
        b = jbody("id", id, NULL);
        entry_list_screen(jstr(e, "title"), "/podcast", b, 0);
    } else if (strcmp(k, "series") == 0) {
        b = jbody("library", jstr(e, "library"), "id", id, NULL);
        entry_list_screen(jstr(e, "title"), "/series/items", b, 0);
    } else if (strcmp(k, "author") == 0) {
        b = jbody("id", id, NULL);
        entry_list_screen(jstr(e, "title"), "/author/items", b, 0);
    } else if (strcmp(k, "collection") == 0) {
        b = jbody("id", id, NULL);
        entry_list_screen(jstr(e, "title"), "/collection/items", b, 0);
    } else if (strcmp(k, "playlist") == 0) {
        b = jbody("id", id, NULL);
        entry_list_screen(jstr(e, "title"), "/playlist/items", b, 0);
    }
    free(b);
}

static const char *SORTS[] = { "title", "author", "added", "length" };
static const char *SORT_LABELS[] = { "Title", "Author", "Recently added", "Length" };

static void save_setting_string(const char *key, const char *value) {
    cJSON *b = cJSON_CreateObject();
    cJSON *s = cJSON_AddObjectToObject(b, "settings");
    cJSON_AddStringToObject(s, key, value);
    char *str = cJSON_PrintUnformatted(b);
    absd_cmd("/settings", str);
    free(str);
    cJSON_Delete(b);
}

/* Shows entries from a daemon endpoint. The response is either an array or
 * an object with "items" (and optionally "cached"). */
void entry_list_screen(const char *title, const char *path, const char *body, int flags) {
    bool sortable = flags & LIST_SORTABLE, mixed = flags & LIST_MIXED;
    int sort = 0;
    char req_body[1024];
    snprintf(req_body, sizeof(req_body), "%s", body ? body : "{}");
    if (sortable) {
        cJSON *st = absd_status();
        const char *cur = jstr(cJSON_GetObjectItem(st, "settings"), "librarySort");
        for (int i = 0; i < 4; i++) if (strcmp(cur, SORTS[i]) == 0) sort = i;
        cJSON_Delete(st);
    }
    char title_copy[160];
    snprintf(title_copy, sizeof(title_copy), "%s", title);

    bool reload = true;
    cJSON *resp = NULL, *arr = NULL;
    ap_list_item *items = NULL;
    char (*trail)[40] = NULL;
    int count = 0, cursor = 0, top = 0;
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
            free(items);
            free(trail);
            resp = nr;
            arr = cJSON_IsArray(resp) ? resp : cJSON_GetObjectItem(resp, "items");
            cached = jbool(resp, "cached");
            count = cJSON_GetArraySize(arr);
            items = calloc((size_t)(count > 0 ? count : 1), sizeof(*items));
            trail = calloc((size_t)(count > 0 ? count : 1), sizeof(*trail));
            for (int i = 0; i < count; i++) {
                cJSON *e = cJSON_GetArrayItem(arr, i);
                items[i].label = jstr(e, "title");
                entry_trailing(e, mixed, trail[i], sizeof(trail[i]));
                items[i].trailing_text = trail[i];
            }
            reload = false;
        }
        if (count == 0) {
            show_message("Nothing here yet.");
            break;
        }

        char full_title[200];
        snprintf(full_title, sizeof(full_title), "%s%s", title_copy, cached ? " (offline)" : "");
        char sort_label[48];
        snprintf(sort_label, sizeof(sort_label), "SORT: %s", SORT_LABELS[sort]);
        ap_footer_item footer[4];
        int fc = 0;
        footer[fc++] = (ap_footer_item){ .button = AP_BTN_B, .label = "BACK" };
        if (sortable) footer[fc++] = (ap_footer_item){ .button = AP_BTN_Y, .label = sort_label };
        bool any_playable = false;
        for (int i = 0; i < count && !any_playable; i++) any_playable = is_playable(cJSON_GetArrayItem(arr, i));
        if (any_playable) footer[fc++] = (ap_footer_item){ .button = AP_BTN_X, .label = "PLAY" };
        footer[fc++] = (ap_footer_item){ .button = AP_BTN_A, .label = "OPEN", .is_confirm = true };

        ap_list_opts o = ap_list_default_opts(full_title, items, count);
        o.footer = footer;
        o.footer_count = fc;
        o.status_bar = &g_status_bar;
        o.action_button = any_playable ? AP_BTN_X : AP_BTN_NONE;
        if (sortable) o.secondary_action_button = AP_BTN_Y;
        o.initial_index = cursor;
        o.visible_start_index = top;
        ap_list_result r;
        if (ap_list(&o, &r) != AP_OK) break;
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
        open_entry(cJSON_GetArrayItem(arr, r.selected_index), r.action == AP_ACTION_TRIGGERED);
    }
    free(items);
    free(trail);
    cJSON_Delete(resp);
}

/* ── Bookmarks ───────────────────────────────────────────────────────────── */

void bookmarks_screen(const char *item_id, bool in_player) {
    char body[200];
    snprintf(body, sizeof(body), "{\"id\":\"%s\"}", item_id);
    int cursor = 0;
    for (;;) {
        char err[256];
        cJSON *bms = absd_call("/bookmarks", body, err, sizeof(err));
        if (!bms) {
            show_message(err);
            return;
        }
        int n = cJSON_GetArraySize(bms);
        if (n == 0) {
            cJSON_Delete(bms);
            show_message("No bookmarks yet.\n\nWhile listening, press START (MORE)\nand choose \"Add bookmark\".");
            return;
        }
        ap_list_item *items = calloc((size_t)n, sizeof(*items));
        char (*tt)[32] = calloc((size_t)n, sizeof(*tt));
        for (int i = 0; i < n; i++) {
            cJSON *b = cJSON_GetArrayItem(bms, i);
            fmt_clock(jnum(b, "time"), tt[i], sizeof(tt[i]));
            if (jbool(b, "pending")) strncat(tt[i], " *", sizeof(tt[i]) - strlen(tt[i]) - 1);
            items[i].label = jstr(b, "title");
            items[i].trailing_text = tt[i];
        }
        ap_footer_item footer[] = {
            { .button = AP_BTN_B, .label = "BACK" },
            { .button = AP_BTN_X, .label = "DELETE" },
            { .button = AP_BTN_A, .label = "PLAY HERE", .is_confirm = true },
        };
        ap_list_opts o = ap_list_default_opts("Bookmarks", items, n);
        o.footer = footer;
        o.footer_count = 3;
        o.status_bar = &g_status_bar;
        o.action_button = AP_BTN_X;
        o.initial_index = cursor < n ? cursor : 0;
        o.help_text = "A plays from the bookmark. X deletes it.\n* = made offline, uploads when back online.";
        ap_list_result r;
        int ret = ap_list(&o, &r);
        double t = r.selected_index >= 0 ? jnum(cJSON_GetArrayItem(bms, r.selected_index), "time") : 0;
        cursor = r.selected_index;
        free(items);
        free(tt);
        cJSON_Delete(bms);
        if (ret != AP_OK || r.selected_index < 0) return;
        if (r.action == AP_ACTION_TRIGGERED) {
            if (confirm("Delete this bookmark?", "DELETE")) {
                char db[256];
                snprintf(db, sizeof(db), "{\"id\":\"%s\",\"time\":%.3f}", item_id, t);
                if (!absd_cmd("/bookmark/delete", db)) show_message("Couldn't delete the bookmark. Try again when online.");
            }
            continue;
        }
        if (in_player) {
            char sb[64];
            snprintf(sb, sizeof(sb), "{\"position\":%.3f}", t);
            absd_cmd("/seek", sb);
        } else {
            play_item(item_id, NULL, t, true);
        }
        return;
    }
}

/* ── Book / episode detail ───────────────────────────────────────────────── */

static void chapter_play_picker(cJSON *d) {
    cJSON *chs = cJSON_GetObjectItem(d, "chapters");
    int cn = cJSON_GetArraySize(chs);
    if (cn == 0) return;
    ap_list_item *ci = calloc((size_t)cn, sizeof(*ci));
    char (*tt)[24] = calloc((size_t)cn, sizeof(*tt));
    for (int i = 0; i < cn; i++) {
        cJSON *c = cJSON_GetArrayItem(chs, i);
        ci[i].label = jstr(c, "title");
        fmt_clock(jnum(c, "start"), tt[i], sizeof(tt[i]));
        ci[i].trailing_text = tt[i];
    }
    ap_footer_item footer[] = {
        { .button = AP_BTN_B, .label = "BACK" },
        { .button = AP_BTN_A, .label = "PLAY", .is_confirm = true },
    };
    ap_list_opts co = ap_list_default_opts("Play from chapter", ci, cn);
    co.footer = footer;
    co.footer_count = 2;
    co.status_bar = &g_status_bar;
    ap_list_result cr;
    int sel = (ap_list(&co, &cr) == AP_OK) ? cr.selected_index : -1;
    double start = sel >= 0 ? jnum(cJSON_GetArrayItem(chs, sel), "start") : 0;
    free(ci);
    free(tt);
    if (sel >= 0) play_item(jstr(d, "id"), jstr(d, "episodeId"), start, true);
}

static void item_actions(cJSON *d) {
    const char *id = jstr(d, "id"), *ep = jstr(d, "episodeId");
    bool episode = ep[0] != '\0';
    cJSON *dl = cJSON_GetObjectItem(d, "download");
    bool downloaded = jbool(d, "downloaded");
    bool downloading = dl && strcmp(jstr(dl, "state"), "error") != 0;
    bool finished = jbool(d, "finished");

    enum { A_START, A_CHAPTER, A_BOOKMARKS, A_DOWNLOAD, A_CANCEL_DL, A_DELETE_DL, A_FINISHED, A_UNFINISHED };
    ap_list_item items[8];
    int acts[8], n = 0;
    items[n] = (ap_list_item){ .label = "Play from beginning" }; acts[n++] = A_START;
    if (cJSON_GetArraySize(cJSON_GetObjectItem(d, "chapters")) > 0) {
        items[n] = (ap_list_item){ .label = "Play from chapter..." }; acts[n++] = A_CHAPTER;
    }
    if (!episode) { items[n] = (ap_list_item){ .label = "Bookmarks..." }; acts[n++] = A_BOOKMARKS; }
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

    char *body = jbody("id", id, "episodeId", ep, NULL);
    char err[256];
    switch (acts[r.selected_index]) {
        case A_START: play_item(id, ep, 0, true); break;
        case A_CHAPTER: chapter_play_picker(d); break;
        case A_BOOKMARKS: bookmarks_screen(id, false); break;
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
        case A_CANCEL_DL: absd_cmd("/download/cancel", body); break;
        case A_DELETE_DL:
            if (confirm("Remove this from the SD card?", "REMOVE")) {
                cJSON *res = absd_call("/download/delete", body, err, sizeof(err));
                if (res) cJSON_Delete(res);
                else show_message(err);
            }
            break;
        case A_FINISHED:
        case A_UNFINISHED: {
            cJSON *b = cJSON_Parse(body);
            cJSON_AddBoolToObject(b, "finished", acts[r.selected_index] == A_FINISHED);
            char *s = cJSON_PrintUnformatted(b);
            if (!absd_cmd("/finished", s)) show_message("Couldn't update the server. Try again when online.");
            free(s);
            cJSON_Delete(b);
            break;
        }
    }
    free(body);
}

void item_detail_screen(const char *item_id, const char *episode_id) {
    char id[128], ep[128];
    snprintf(id, sizeof(id), "%s", item_id);
    snprintf(ep, sizeof(ep), "%s", episode_id ? episode_id : "");
    char *body = jbody("id", id, "episodeId", ep, NULL);
    bool episode = ep[0] != '\0';
    for (;;) {
        char err[256];
        cJSON *d = call_with_spinner("Loading...", "/item", body, err, sizeof(err));
        if (!d) {
            show_message(err);
            break;
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

        ap_detail_info_pair info[9];
        int ni = 0;
        if (episode) {
            if (jstr(d, "author")[0]) info[ni++] = (ap_detail_info_pair){ "Podcast", jstr(d, "author") };
            if (jstr(d, "published")[0]) info[ni++] = (ap_detail_info_pair){ "Published", jstr(d, "published") };
        } else {
            if (jstr(d, "author")[0]) info[ni++] = (ap_detail_info_pair){ "Author", jstr(d, "author") };
            if (jstr(d, "narrator")[0]) info[ni++] = (ap_detail_info_pair){ "Narrator", jstr(d, "narrator") };
            if (jstr(d, "series")[0]) info[ni++] = (ap_detail_info_pair){ "Series", jstr(d, "series") };
            if (jstr(d, "year")[0]) info[ni++] = (ap_detail_info_pair){ "Year", jstr(d, "year") };
        }
        info[ni++] = (ap_detail_info_pair){ "Length", length };
        info[ni++] = (ap_detail_info_pair){ "Progress", progress };
        if (!episode || strcmp(chapters, "0") != 0) info[ni++] = (ap_detail_info_pair){ "Chapters", chapters };
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
            cJSON_Delete(d);
            play_item(id, ep, 0, false);
            continue;
        }
        if (r.action == AP_DETAIL_SECONDARY_ACTION) {
            item_actions(d);
            cJSON_Delete(d);
            continue;
        }
        cJSON_Delete(d);
        break;
    }
    free(body);
}

/* ── Downloads ───────────────────────────────────────────────────────────── */

typedef struct {
    ap_list_item *items;
    char (*trail)[32];
    char (*keys)[260]; /* "itemId|episodeId" */
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

static void dl_key(char *out, const char *id, const char *ep) { snprintf(out, 260, "%s|%s", id, ep); }

/* Live-update download percentages while the list is open. */
static void dl_footer_update(ap_list_opts *opts, int cursor, void *userdata) {
    (void)opts; (void)cursor;
    dl_view *v = userdata;
    if (v->jobs == 0) return;
    if (SDL_GetTicks() - v->last_refresh >= 1000) {
        v->last_refresh = SDL_GetTicks();
        cJSON *r = absd_call("/downloaded", NULL, NULL, 0);
        if (r) {
            cJSON *jobs = cJSON_GetObjectItem(r, "jobs"), *done = cJSON_GetObjectItem(r, "items");
            for (int i = 0; i < v->jobs; i++) {
                char k[260];
                bool found = false;
                cJSON *j;
                cJSON_ArrayForEach(j, jobs) {
                    dl_key(k, jstr(j, "itemId"), jstr(j, "episodeId"));
                    if (strcmp(k, v->keys[i]) == 0) {
                        dl_fill_job_trail(j, v->trail[i]);
                        found = true;
                    }
                }
                if (!found) {
                    cJSON *b;
                    cJSON_ArrayForEach(b, done) {
                        dl_key(k, jstr(b, "id"), jstr(b, "episodeId"));
                        if (strcmp(k, v->keys[i]) == 0) snprintf(v->trail[i], 32, "Done");
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
        cJSON *jobs = cJSON_GetObjectItem(r, "jobs"), *done = cJSON_GetObjectItem(r, "items");
        dl_view v = { .jobs = cJSON_GetArraySize(jobs) };
        v.count = v.jobs + cJSON_GetArraySize(done);
        if (v.count == 0) {
            cJSON_Delete(r);
            show_message("No downloads yet.\n\nOpen a book or episode, press Y (MORE) and\nchoose \"Download to SD card\" to listen offline.");
            return;
        }
        v.items = calloc((size_t)v.count, sizeof(*v.items));
        v.trail = calloc((size_t)v.count, sizeof(*v.trail));
        v.keys = calloc((size_t)v.count, sizeof(*v.keys));
        int i = 0;
        cJSON *it;
        cJSON_ArrayForEach(it, jobs) {
            v.items[i].label = jstr(it, "title");
            dl_key(v.keys[i], jstr(it, "itemId"), jstr(it, "episodeId"));
            dl_fill_job_trail(it, v.trail[i]);
            v.items[i].trailing_text = v.trail[i];
            i++;
        }
        cJSON_ArrayForEach(it, done) {
            v.items[i].label = jstr(it, "title");
            dl_key(v.keys[i], jstr(it, "id"), jstr(it, "episodeId"));
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

        char key[260] = "";
        bool is_job = false;
        if (ret == AP_OK && lr.selected_index >= 0) {
            snprintf(key, sizeof(key), "%s", v.keys[lr.selected_index]);
            is_job = lr.selected_index < v.jobs;
            cursor = lr.selected_index;
        }
        int action = lr.action;
        free(v.items);
        free(v.trail);
        free(v.keys);
        cJSON_Delete(r);
        if (ret != AP_OK) return;
        if (!key[0]) continue;

        char *bar = strchr(key, '|');
        const char *ep = "";
        if (bar) {
            *bar = '\0';
            ep = bar + 1;
        }
        char *body = jbody("id", key, "episodeId", ep, NULL);
        if (action == AP_ACTION_TRIGGERED) {
            if (is_job) {
                absd_cmd("/download/cancel", body);
            } else if (confirm("Remove this from the SD card?", "REMOVE")) {
                cJSON *res = absd_call("/download/delete", body, err, sizeof(err));
                if (res) cJSON_Delete(res);
                else show_message(err);
            }
        } else {
            item_detail_screen(key, ep);
        }
        free(body);
    }
}

/* ── Search ──────────────────────────────────────────────────────────────── */

void search_screen(const char *library_id) {
    ap_keyboard_result kb;
    if (ap_keyboard("", NULL, AP_KB_GENERAL, &kb) != AP_OK || !kb.text[0]) return;
    char *body = jbody("library", library_id, "q", kb.text, NULL);
    char title[160];
    snprintf(title, sizeof(title), "Search: %.140s", kb.text);
    entry_list_screen(title, "/search", body, LIST_MIXED);
    free(body);
}
