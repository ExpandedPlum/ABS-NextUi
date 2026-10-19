/* Now Playing: cover, chapter progress, and transport controls. */
#include "common.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static const char *NP_HELP =
    "A  Play / pause\n"
    "LEFT / RIGHT  Skip back / forward\n"
    "L1 / R1  Previous / next chapter\n"
    "X  Chapters\n"
    "Y  Sleep timer\n"
    "SELECT  Playback speed\n"
    "START  Stop playback\n"
    "B  Back (keeps playing)\n"
    "POWER  Screen off / on (keeps playing)\n"
    "Hold POWER  Save and shut down";

typedef struct {
    SDL_Texture *tex;
    char path[512];
    int w, h;
} cover_cache;

static void update_cover(cover_cache *c, const char *path) {
    if (strcmp(c->path, path) == 0) return;
    if (c->tex) SDL_DestroyTexture(c->tex);
    c->tex = NULL;
    snprintf(c->path, sizeof(c->path), "%s", path);
    if (path[0]) {
        c->tex = ap_load_image(path);
        if (c->tex) SDL_QueryTexture(c->tex, NULL, NULL, &c->w, &c->h);
    }
}

static void draw_cover(cover_cache *c, int x, int y, int size) {
    ap_theme *t = ap_get_theme();
    if (!c->tex) {
        ap_draw_rounded_rect(x, y, size, size, UI_DS(8), t->highlight);
        TTF_Font *f = ap_get_font(AP_FONT_EXTRA_LARGE);
        int tw = ap_measure_text(f, "ABS");
        ap_draw_text(f, "ABS", x + (size - tw) / 2, y + size / 2 - TTF_FontHeight(f) / 2, t->highlighted_text);
        return;
    }
    int dw = size, dh = size;
    if (c->w > 0 && c->h > 0) {
        if (c->w >= c->h) dh = size * c->h / c->w;
        else dw = size * c->w / c->h;
    }
    ap_draw_image(c->tex, x + (size - dw) / 2, y + (size - dh) / 2, dw, dh);
}

static const char *state_label(const char *state) {
    if (strcmp(state, "playing") == 0) return "Playing";
    if (strcmp(state, "paused") == 0) return "Paused";
    if (strcmp(state, "loading") == 0) return "Loading...";
    if (strcmp(state, "ended") == 0) return "Finished";
    if (strcmp(state, "error") == 0) return "Error";
    return "Stopped";
}

static void render(cJSON *st, cover_cache *cover, ap_footer_item *footer, int footer_count) {
    ap_theme *t = ap_get_theme();
    cJSON *p = cJSON_GetObjectItem(st, "player");
    const char *state = jstr(p, "state");

    ap_clear_screen();
    ap_draw_screen_title("Now Playing", &g_status_bar);
    ap_draw_status_bar(&g_status_bar);
    SDL_Rect cr = ap_get_content_rect(true, true, true);
    int pad = UI_DS(8);

    TTF_Font *lg = ap_get_font(AP_FONT_LARGE);
    TTF_Font *sm = ap_get_font(AP_FONT_SMALL);
    TTF_Font *ti = ap_get_font(AP_FONT_TINY);
    int bar_h = UI_DS(4);

    /* Text block height, so it can be centred beside the cover. */
    int block_h = TTF_FontHeight(lg) + TTF_FontHeight(sm) + pad * 2 + TTF_FontHeight(sm) + pad / 2 + bar_h +
                  pad / 2 + TTF_FontHeight(ti) + pad * 2 + TTF_FontHeight(ti) * 2 + pad;

    int cover_size = cr.h - pad * 2;
    if (cover_size > cr.w * 42 / 100) cover_size = cr.w * 42 / 100;
    int cx = cr.x + pad;
    int cy = cr.y + (cr.h - cover_size) / 2;
    update_cover(cover, jstr(p, "cover"));
    draw_cover(cover, cx, cy, cover_size);

    int x = cx + cover_size + pad * 3;
    int w = cr.x + cr.w - x - pad;
    int y = cr.y + (cr.h - block_h) / 2;
    if (y < cr.y) y = cr.y;

    ap_draw_text_ellipsized(lg, jstr(p, "title"), x, y, t->text, w);
    y += TTF_FontHeight(lg);
    ap_draw_text_ellipsized(sm, jstr(p, "author"), x, y, t->hint, w);
    y += TTF_FontHeight(sm) + pad * 2;

    double pos = jnum(p, "position"), dur = jnum(p, "duration");
    double cs = jnum(p, "chapterStart"), ce = jnum(p, "chapterEnd");
    int ci = (int)jnum(p, "chapterIndex"), cc = (int)jnum(p, "chapterCount");
    bool has_chapter = ci >= 0 && ce > cs;
    if (!has_chapter) { cs = 0; ce = dur; }

    char line[256], a[32], b[32];
    if (has_chapter) {
        snprintf(line, sizeof(line), "%s", jstr(p, "chapterTitle"));
        if (!line[0]) snprintf(line, sizeof(line), "Chapter %d", ci + 1);
    } else {
        snprintf(line, sizeof(line), "%s", "Whole book");
    }
    ap_draw_text_ellipsized(sm, line, x, y, t->text, w);
    y += TTF_FontHeight(sm) + pad / 2;

    double frac = ce > cs ? (pos - cs) / (ce - cs) : 0;
    ap_draw_progress_bar(x, y, w, bar_h, (float)frac, t->accent, t->highlight);
    y += bar_h + pad / 2;
    fmt_clock(pos - cs, a, sizeof(a));
    fmt_clock(ce - pos, b, sizeof(b));
    char rem[40];
    snprintf(rem, sizeof(rem), "-%s", b);
    ap_draw_text(ti, a, x, y, t->hint);
    ap_draw_text(ti, rem, x + w - ap_measure_text(ti, rem), y, t->hint);
    if (has_chapter && cc > 0) {
        snprintf(line, sizeof(line), "%d of %d", ci + 1, cc);
        ap_draw_text(ti, line, x + (w - ap_measure_text(ti, line)) / 2, y, t->hint);
    }
    y += TTF_FontHeight(ti) + pad * 2;

    char left[32];
    fmt_duration(dur - pos, left, sizeof(left));
    double speed = jnum(p, "speed");
    snprintf(line, sizeof(line), "Book %d%%  |  %s left  |  %.3gx", dur > 0 ? (int)(pos * 100 / dur) : 0, left,
             speed > 0 ? speed : 1);
    ap_draw_text_ellipsized(ti, line, x, y, t->hint, w);
    y += TTF_FontHeight(ti);

    if (strcmp(state, "error") == 0 && jstr(p, "error")[0]) {
        snprintf(line, sizeof(line), "Error: %s", jstr(p, "error"));
    } else if (jbool(p, "sleepActive")) {
        if (jbool(p, "sleepEndOfChapter")) {
            snprintf(line, sizeof(line), "%s  |  sleep at chapter end", state_label(state));
        } else {
            fmt_clock(jnum(p, "sleepRemaining"), a, sizeof(a));
            snprintf(line, sizeof(line), "%s  |  sleep in %s", state_label(state), a);
        }
    } else if (!jbool(st, "online") && !jbool(p, "offlineFiles")) {
        snprintf(line, sizeof(line), "%s  |  offline, will sync later", state_label(state));
    } else {
        snprintf(line, sizeof(line), "%s%s", state_label(state), jbool(p, "offlineFiles") ? "  |  from SD card" : "");
    }
    ap_draw_text_ellipsized(ti, line, x, y, strcmp(state, "error") == 0 ? t->accent : t->text, w);

    bool playing = strcmp(state, "playing") == 0 || strcmp(state, "loading") == 0;
    footer[footer_count - 1].label = playing ? "PAUSE" : "PLAY";
    ap_draw_footer(footer, footer_count);
    ap_present();
}

void now_playing_screen(void) {
    cover_cache cover = {0};
    ap_footer_item footer[] = {
        { .button = AP_BTN_B,      .label = "BACK" },
        { .button = AP_BTN_X,      .label = "CHAPTERS" },
        { .button = AP_BTN_Y,      .label = "SLEEP" },
        { .button = AP_BTN_A,      .label = "PAUSE", .is_confirm = true },
    };
    int footer_count = (int)(sizeof(footer) / sizeof(footer[0]));

    cJSON *st = absd_status();
    Uint32 last_poll = SDL_GetTicks();
    bool running = st != NULL;

    while (running) {
        bool acted = false;
        ap_input_event ev;
        while (ap_poll_input(&ev)) {
            if (!ev.pressed) continue;
            cJSON *set = cJSON_GetObjectItem(st, "settings");
            char body[64];
            switch (ev.button) {
                case AP_BTN_B: running = false; break;
                case AP_BTN_A: absd_cmd("/toggle", NULL); acted = true; break;
                case AP_BTN_LEFT:
                    snprintf(body, sizeof(body), "{\"seconds\":%d}", -(int)jnum(set, "skipBack"));
                    absd_cmd("/skip", body); acted = true; break;
                case AP_BTN_RIGHT:
                    snprintf(body, sizeof(body), "{\"seconds\":%d}", (int)jnum(set, "skipForward"));
                    absd_cmd("/skip", body); acted = true; break;
                case AP_BTN_L1: absd_cmd("/chapter/prev", NULL); acted = true; break;
                case AP_BTN_R1: absd_cmd("/chapter/next", NULL); acted = true; break;
                case AP_BTN_X: chapters_picker(); acted = true; break;
                case AP_BTN_Y: sleep_picker(); acted = true; break;
                case AP_BTN_SELECT: speed_picker(); acted = true; break;
                case AP_BTN_START:
                    if (confirm("Stop playback?\nYour position is saved.", "STOP")) {
                        absd_cmd("/stop", NULL);
                        running = false;
                    }
                    acted = true;
                    break;
                case AP_BTN_MENU: ap_show_help_overlay(NP_HELP); break;
                default: break;
            }
        }
        if (!running) break;

        if (screen_is_off()) {
            /* Dark screen: no rendering, just keep pumping events slowly. */
            SDL_WaitEventTimeout(NULL, 500);
            continue;
        }

        if (acted || SDL_GetTicks() - last_poll > 500) {
            cJSON *ns = absd_status();
            if (!ns) {
                show_message("The player service stopped.");
                break;
            }
            cJSON_Delete(st);
            st = ns;
            last_poll = SDL_GetTicks();
            if (strcmp(jstr(cJSON_GetObjectItem(st, "player"), "state"), "idle") == 0) break;
        }
        render(st, &cover, footer, footer_count);
        SDL_WaitEventTimeout(NULL, 200);
    }
    if (cover.tex) SDL_DestroyTexture(cover.tex);
    cJSON_Delete(st);
}

/* ── Pickers used from Now Playing ───────────────────────────────────────── */

void chapters_picker(void) {
    char err[256];
    cJSON *chs = absd_call("/chapters", NULL, err, sizeof(err));
    cJSON *st = absd_status();
    if (!chs || !st) {
        cJSON_Delete(chs);
        cJSON_Delete(st);
        return;
    }
    int n = cJSON_GetArraySize(chs);
    if (n == 0) {
        show_message("This book has no chapters.");
        cJSON_Delete(chs);
        cJSON_Delete(st);
        return;
    }
    ap_list_item *items = calloc((size_t)n, sizeof(*items));
    char (*times)[24] = calloc((size_t)n, sizeof(*times));
    char (*labels)[24] = calloc((size_t)n, sizeof(*labels));
    for (int i = 0; i < n; i++) {
        cJSON *c = cJSON_GetArrayItem(chs, i);
        fmt_clock(jnum(c, "start"), times[i], sizeof(times[i]));
        items[i].label = jstr(c, "title");
        if (!items[i].label[0]) {
            snprintf(labels[i], sizeof(labels[i]), "Chapter %d", i + 1);
            items[i].label = labels[i];
        }
        items[i].trailing_text = times[i];
    }
    ap_footer_item footer[] = {
        { .button = AP_BTN_B, .label = "BACK" },
        { .button = AP_BTN_A, .label = "JUMP", .is_confirm = true },
    };
    ap_list_opts o = ap_list_default_opts("Chapters", items, n);
    o.footer = footer;
    o.footer_count = 2;
    o.status_bar = &g_status_bar;
    int cur = (int)jnum(cJSON_GetObjectItem(st, "player"), "chapterIndex");
    o.initial_index = cur > 0 ? cur : 0;
    o.visible_start_index = o.initial_index > 3 ? o.initial_index - 3 : 0;
    ap_list_result r;
    if (ap_list(&o, &r) == AP_OK && r.selected_index >= 0) {
        char body[48];
        snprintf(body, sizeof(body), "{\"index\":%d}", r.selected_index);
        absd_cmd("/chapter", body);
    }
    free(items);
    free(times);
    free(labels);
    cJSON_Delete(chs);
    cJSON_Delete(st);
}

void speed_picker(void) {
    static const double speeds[] = { 0.5, 0.75, 0.8, 0.9, 1.0, 1.1, 1.2, 1.25, 1.3, 1.4, 1.5, 1.6, 1.75, 2.0, 2.25, 2.5, 3.0 };
    enum { N = sizeof(speeds) / sizeof(speeds[0]) };
    char labels[N][16];
    ap_list_item items[N];
    cJSON *st = absd_status();
    double cur = st ? jnum(cJSON_GetObjectItem(st, "player"), "speed") : 1.0;
    int init = 4;
    for (int i = 0; i < N; i++) {
        snprintf(labels[i], sizeof(labels[i]), "%.3gx", speeds[i]);
        items[i] = (ap_list_item){ .label = labels[i], .trailing_text = speeds[i] == 1.0 ? "normal" : NULL };
        if (cur > speeds[i] - 0.01 && cur < speeds[i] + 0.01) init = i;
    }
    cJSON_Delete(st);
    ap_footer_item footer[] = {
        { .button = AP_BTN_B, .label = "BACK" },
        { .button = AP_BTN_A, .label = "SET", .is_confirm = true },
    };
    ap_list_opts o = ap_list_default_opts("Playback Speed", items, N);
    o.footer = footer;
    o.footer_count = 2;
    o.status_bar = &g_status_bar;
    o.initial_index = init;
    o.visible_start_index = init > 3 ? init - 3 : 0;
    ap_list_result r;
    if (ap_list(&o, &r) == AP_OK && r.selected_index >= 0) {
        char body[48];
        snprintf(body, sizeof(body), "{\"speed\":%.2f}", speeds[r.selected_index]);
        absd_cmd("/speed", body);
    }
}

void sleep_picker(void) {
    static const struct { const char *label; double minutes; } opts[] = {
        { "Off", 0 }, { "5 minutes", 5 }, { "10 minutes", 10 }, { "15 minutes", 15 },
        { "20 minutes", 20 }, { "30 minutes", 30 }, { "45 minutes", 45 }, { "1 hour", 60 },
        { "1.5 hours", 90 }, { "2 hours", 120 }, { "End of chapter", -1 },
    };
    enum { N = sizeof(opts) / sizeof(opts[0]) };
    ap_list_item items[N];
    for (int i = 0; i < N; i++) items[i] = (ap_list_item){ .label = opts[i].label };
    ap_footer_item footer[] = {
        { .button = AP_BTN_B, .label = "BACK" },
        { .button = AP_BTN_A, .label = "SET", .is_confirm = true },
    };
    ap_list_opts o = ap_list_default_opts("Sleep Timer", items, N);
    o.footer = footer;
    o.footer_count = 2;
    o.status_bar = &g_status_bar;
    ap_list_result r;
    if (ap_list(&o, &r) == AP_OK && r.selected_index >= 0) {
        char body[48];
        snprintf(body, sizeof(body), "{\"minutes\":%g}", opts[r.selected_index].minutes);
        absd_cmd("/sleep", body);
    }
}
