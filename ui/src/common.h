/* Shared declarations for the Audiobookshelf NextUI front end. */
#ifndef ABS_COMMON_H
#define ABS_COMMON_H

#include <stdbool.h>
#include <stddef.h>

#include "apostrophe.h"
#include "apostrophe_widgets.h"
#include "cJSON.h"

#define APP_NAME "Audiobookshelf"

/* AP_S/AP_DS only work inside the Apostrophe implementation unit. */
#define UI_S(x)  ap_scale(x)                    /* scale from 1024px reference */
#define UI_DS(x) ap_font_size_for_resolution(x) /* x * integer device scale */

/* ── ipc.c: JSON calls to the absd daemon over its unix socket ───────────── */

/* Returns parsed JSON (caller frees with cJSON_Delete) or NULL with a
 * human-readable message in err. body may be NULL. */
cJSON *absd_call(const char *path, const char *body, char *err, size_t errlen);
/* Fire-and-forget command; returns true on success. */
bool absd_cmd(const char *path, const char *body);
/* Fetch /status, or NULL when the daemon is unreachable. */
cJSON *absd_status(void);

/* ── power.c: screen-off handling and daemon heartbeat ───────────────────── */

void power_init(void);
void power_shutdown(void);
bool screen_is_off(void);
void screen_set_off(bool off);

/* ── util.c ──────────────────────────────────────────────────────────────── */

const char *jstr(const cJSON *obj, const char *key);   /* "" when missing */
double      jnum(const cJSON *obj, const char *key);   /* 0 when missing */
bool        jbool(const cJSON *obj, const char *key);
void        fmt_clock(double seconds, char *buf, size_t len);    /* 1:02:03 / 4:05 */
void        fmt_duration(double seconds, char *buf, size_t len); /* 5h 12m */
char       *strip_html(const char *in);                          /* caller frees */
void        show_message(const char *msg);
bool        confirm(const char *msg, const char *yes_label);
/* Run fn in a worker thread behind a spinner with the given message. */
typedef struct {
    const char *path;
    char       *body;
    cJSON      *result;
    char        err[256];
} call_job;
cJSON      *call_with_spinner(const char *message, const char *path, const char *body, char *err, size_t errlen);

/* ── screens ─────────────────────────────────────────────────────────────── */

bool login_screen(void);                 /* true once logged in */
void now_playing_screen(void);
void book_list_screen(const char *title, const char *path, const char *body, bool sortable);
void book_detail_screen(const char *item_id);
void downloads_screen(void);
bool settings_screen(void);            /* true when the user logged out */
void search_screen(void);
void chapters_picker(void);
void speed_picker(void);
void sleep_picker(void);
/* Start playback of an item (optionally at position) and open Now Playing. */
void play_item(const char *item_id, double position, bool use_position);

extern ap_status_bar_opts g_status_bar;

#endif
