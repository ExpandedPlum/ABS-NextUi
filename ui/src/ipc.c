/* Minimal HTTP/1.0 client for the absd control socket. */
#include "common.h"

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <sys/un.h>
#include <unistd.h>

static const char *socket_path(void) {
    const char *p = getenv("ABSD_SOCKET");
    return (p && p[0]) ? p : "/tmp/absd.sock";
}

static void set_err(char *err, size_t errlen, const char *msg) {
    if (err && errlen) snprintf(err, errlen, "%s", msg);
}

cJSON *absd_call(const char *path, const char *body, char *err, size_t errlen) {
    if (err && errlen) err[0] = '\0';

    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) {
        set_err(err, errlen, "socket() failed");
        return NULL;
    }
    struct sockaddr_un addr = {0};
    addr.sun_family = AF_UNIX;
    snprintf(addr.sun_path, sizeof(addr.sun_path), "%s", socket_path());
    if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) != 0) {
        close(fd);
        set_err(err, errlen, "Player service is not running");
        return NULL;
    }
    /* Slow server calls (big libraries, downloads metadata) can take a while. */
    struct timeval tv = { .tv_sec = 90, .tv_usec = 0 };
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));

    size_t blen = body ? strlen(body) : 0;
    char head[512];
    int hlen = snprintf(head, sizeof(head),
                        "%s %s HTTP/1.0\r\nHost: absd\r\nContent-Type: application/json\r\n"
                        "Content-Length: %zu\r\n\r\n",
                        body ? "POST" : "GET", path, blen);
    if (write(fd, head, (size_t)hlen) != hlen || (blen && write(fd, body, blen) != (ssize_t)blen)) {
        close(fd);
        set_err(err, errlen, "Could not talk to player service");
        return NULL;
    }

    size_t cap = 16384, len = 0;
    char *buf = malloc(cap);
    if (!buf) {
        close(fd);
        set_err(err, errlen, "Out of memory");
        return NULL;
    }
    for (;;) {
        if (len + 4096 > cap) {
            cap *= 2;
            char *nb = realloc(buf, cap);
            if (!nb) {
                free(buf);
                close(fd);
                set_err(err, errlen, "Out of memory");
                return NULL;
            }
            buf = nb;
        }
        ssize_t n = read(fd, buf + len, cap - len - 1);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) break;
        len += (size_t)n;
    }
    close(fd);
    buf[len] = '\0';

    int status = 0;
    sscanf(buf, "HTTP/%*s %d", &status);
    char *json = strstr(buf, "\r\n\r\n");
    cJSON *root = json ? cJSON_Parse(json + 4) : NULL;
    free(buf);

    if (status != 200) {
        const char *msg = root ? jstr(root, "error") : "";
        set_err(err, errlen, msg[0] ? msg : (status ? "Request failed" : "No response from player service"));
        cJSON_Delete(root);
        return NULL;
    }
    if (!root) set_err(err, errlen, "Bad response from player service");
    return root;
}

bool absd_cmd(const char *path, const char *body) {
    char err[128];
    cJSON *r = absd_call(path, body ? body : "{}", err, sizeof(err));
    if (!r) {
        ap_log("absd %s: %s", path, err);
        return false;
    }
    cJSON_Delete(r);
    return true;
}

cJSON *absd_status(void) {
    return absd_call("/status", NULL, NULL, 0);
}
