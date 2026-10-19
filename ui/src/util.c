#include "common.h"

#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

const char *jstr(const cJSON *obj, const char *key) {
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(obj, key);
    return cJSON_IsString(v) && v->valuestring ? v->valuestring : "";
}

double jnum(const cJSON *obj, const char *key) {
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(obj, key);
    return cJSON_IsNumber(v) ? v->valuedouble : 0;
}

bool jbool(const cJSON *obj, const char *key) {
    return cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(obj, key));
}

void fmt_clock(double seconds, char *buf, size_t len) {
    if (seconds < 0) seconds = 0;
    long s = (long)seconds;
    if (s >= 3600)
        snprintf(buf, len, "%ld:%02ld:%02ld", s / 3600, (s / 60) % 60, s % 60);
    else
        snprintf(buf, len, "%ld:%02ld", s / 60, s % 60);
}

void fmt_duration(double seconds, char *buf, size_t len) {
    if (seconds < 0) seconds = 0;
    long m = (long)(seconds / 60 + 0.5);
    if (m >= 60)
        snprintf(buf, len, "%ldh %02ldm", m / 60, m % 60);
    else if (m > 0)
        snprintf(buf, len, "%ldm", m);
    else
        snprintf(buf, len, "%lds", (long)seconds);
}

/* Audiobookshelf descriptions are HTML. Keep paragraphs, drop tags, decode
 * the common entities. */
char *strip_html(const char *in) {
    if (!in) return strdup("");
    size_t n = strlen(in);
    char *out = malloc(n + 1);
    if (!out) return NULL;
    size_t o = 0;
    for (size_t i = 0; i < n; i++) {
        if (in[i] == '<') {
            const char *end = strchr(in + i, '>');
            if (!end) break;
            size_t tl = (size_t)(end - (in + i));
            if ((tl >= 2 && (strncasecmp(in + i, "<p", 2) == 0 || strncasecmp(in + i, "<br", 3) == 0 ||
                             strncasecmp(in + i, "</p", 3) == 0 || strncasecmp(in + i, "<li", 3) == 0)) &&
                o > 0 && out[o - 1] != '\n') {
                out[o++] = '\n';
            }
            i += tl;
            continue;
        }
        if (in[i] == '&') {
            static const struct { const char *e; char c; } ents[] = {
                {"&amp;", '&'}, {"&lt;", '<'}, {"&gt;", '>'}, {"&quot;", '"'},
                {"&#39;", '\''}, {"&apos;", '\''}, {"&nbsp;", ' '},
            };
            bool done = false;
            for (size_t k = 0; k < sizeof(ents) / sizeof(ents[0]); k++) {
                size_t el = strlen(ents[k].e);
                if (strncmp(in + i, ents[k].e, el) == 0) {
                    out[o++] = ents[k].c;
                    i += el - 1;
                    done = true;
                    break;
                }
            }
            if (done) continue;
        }
        if (in[i] == '\r') continue;
        out[o++] = in[i];
    }
    while (o > 0 && isspace((unsigned char)out[o - 1])) o--;
    out[o] = '\0';
    return out;
}

void show_message(const char *msg) {
    ap_footer_item footer[] = { { .button = AP_BTN_A, .label = "OK", .is_confirm = true } };
    ap_message_opts opts = { .message = msg, .footer = footer, .footer_count = 1 };
    ap_confirm_result r;
    ap_confirmation(&opts, &r);
}

bool confirm(const char *msg, const char *yes_label) {
    ap_footer_item footer[] = {
        { .button = AP_BTN_B, .label = "CANCEL" },
        { .button = AP_BTN_A, .label = yes_label, .is_confirm = true },
    };
    ap_message_opts opts = { .message = msg, .footer = footer, .footer_count = 2 };
    ap_confirm_result r = {0};
    return ap_confirmation(&opts, &r) == AP_OK && r.confirmed;
}

static int call_worker(void *userdata) {
    call_job *job = userdata;
    job->result = absd_call(job->path, job->body ? job->body : "{}", job->err, sizeof(job->err));
    return job->result ? AP_OK : AP_ERROR;
}

cJSON *call_with_spinner(const char *message, const char *path, const char *body, char *err, size_t errlen) {
    call_job job = { .path = path, .body = body ? strdup(body) : NULL };
    ap_process_opts opts = { .message = message };
    ap_process_message(&opts, call_worker, &job);
    free(job.body);
    if (!job.result && err && errlen) snprintf(err, errlen, "%s", job.err);
    return job.result;
}
