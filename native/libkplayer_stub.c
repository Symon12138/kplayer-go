/*
 * libkplayer_stub.c — API-compatible simulated engine for the KPlayer Go
 * control layer. Implements the 13-entry C surface consumed by core/kplayer.go
 * with a no-op engine that replies to every prompt with an asynchronous
 * message callback, so the Go-side providers' keeper-channel waits resolve
 * and the full five-phase console/REST/scheduler stack runs for real.
 * Real encoding/streaming is NOT performed.
 *
 * The stub keeps minimal state (resources / outputs / plugins registered via
 * prompts) so list/current queries return plausible bodies. Message bodies
 * use the exact proto field names consumed by jsonpb.UnmarshalString in
 * types/utils.go — unknown fields would make the Go side call log.Fatal.
 *
 * The async reply (a detached thread with a short delay) is required: the Go
 * caller invokes PromptMessage and then blocks in Wait() on the same
 * goroutine, so a synchronous callback would deadlock on the keeper channel.
 */
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "extra.h"

/* ---- global engine state ------------------------------------------- */
static MessageCallBack g_msg_cb = NULL;
static ProgressCallBack g_prog_cb = NULL;
static char g_error[512] = "";
static int g_running = 0;

#define MAX_ITEMS 64
#define ITEM_LEN 512
static char g_resources[MAX_ITEMS][ITEM_LEN];
static int g_resource_count = 0;
static char g_current_resource[ITEM_LEN] = "";
static char g_outputs[MAX_ITEMS][ITEM_LEN];
static int g_output_count = 0;
static char g_plugins[MAX_ITEMS][ITEM_LEN];
static int g_plugin_count = 0;

/* ---- small helpers -------------------------------------------------- */
static void extract_path(const char *json, char *out, size_t sz) {
    out[0] = '\0';
    if (json == NULL) return;
    const char *key = strstr(json, "\"path\"");
    if (key == NULL) return;
    const char *colon = strchr(key, ':');
    if (colon == NULL) return;
    const char *q1 = strchr(colon, '"');
    if (q1 == NULL) return;
    const char *q2 = strchr(q1 + 1, '"');
    if (q2 == NULL) return;
    size_t len = (size_t)(q2 - q1 - 1);
    if (len >= sz) len = sz - 1;
    memcpy(out, q1 + 1, len);
    out[len] = '\0';
}

static void push_item(char items[][ITEM_LEN], int *count, const char *path) {
    if (*count < MAX_ITEMS && path != NULL && path[0] != '\0') {
        snprintf(items[*count], ITEM_LEN, "%s", path);
        (*count)++;
    }
}

static void build_resource_list_json(char *buf, size_t sz) {
    size_t off = 0;
    off += (size_t)snprintf(buf + off, sz - off, "{\"resources\":[");
    for (int i = 0; i < g_resource_count && off < sz; i++) {
        off += (size_t)snprintf(buf + off, sz - off,
                                "%s{\"path\":\"%s\",\"unique\":\"\",\"seek\":0,\"end\":0,\"groups\":[]}",
                                i > 0 ? "," : "", g_resources[i]);
    }
    snprintf(buf + off, sz - off, "],\"error\":\"\"}");
}

static void build_output_list_json(char *buf, size_t sz) {
    size_t off = 0;
    off += (size_t)snprintf(buf + off, sz - off, "{\"outputs\":[");
    for (int i = 0; i < g_output_count && off < sz; i++) {
        off += (size_t)snprintf(buf + off, sz - off,
                                "%s{\"path\":\"%s\",\"unique\":\"\"}",
                                i > 0 ? "," : "", g_outputs[i]);
    }
    snprintf(buf + off, sz - off, "],\"error\":\"\"}");
}

/* prompt action -> message action mapping (from types/core/proto/keys.pb.go) */
static int prompt_to_message(int prompt) {
    switch (prompt) {
    case 0:  return 21;  /* PLAYER_STOP    -> PLAYER_STOP */
    case 1:  return 1;   /* PLAYER_PAUSE   -> PLAYER_PAUSE */
    case 2:  return 3;   /* PLAYER_SKIP    -> PLAYER_SKIP */
    case 3:  return 2;   /* PLAYER_CONTINUE-> PLAYER_CONTINUE */
    case 36: return 23;  /* SET_QUALITY    -> SET_QUALITY */
    case 37: return 24;  /* SET_BITRATE    -> SET_BITRATE */
    case 38: return 25;  /* OUTPUT_OPTION  -> OUTPUT_OPTION */
    case 5:  return 12;  /* OUTPUT_ADD     -> OUTPUT_ADD */
    case 6:  return 13;  /* OUTPUT_REMOVE  -> OUTPUT_REMOVE */
    case 7:  return 14;  /* OUTPUT_LIST    -> OUTPUT_LIST */
    case 8:  return 9;   /* RESOURCE_ADD   -> RESOURCE_ADD */
    case 9:  return 8;   /* RESOURCE_REMOVE-> RESOURCE_REMOVE */
    case 10: return 10;  /* RESOURCE_LIST  -> RESOURCE_LIST */
    case 34: return 11;  /* RESOURCE_CURRENT -> RESOURCE_CURRENT */
    case 35: return 22;  /* RESOURCE_SEEK  -> RESOURCE_SEEK */
    case 11: return 16;  /* PLUGIN_ADD     -> PLUGIN_ADD */
    case 12: return 17;  /* PLUGIN_REMOVE  -> PLUGIN_REMOVE */
    case 13: return 18;  /* PLUGIN_LIST    -> PLUGIN_LIST */
    case 32: return 19;  /* PLUGIN_UPDATE  -> PLUGIN_UPDATE */
    default: return -1;
    }
}

/* Build the message body matching the message struct the Go side expects.
 * Every message that nests a resource/output/plugin object gets a complete
 * (possibly empty-field) object so the Go providers never dereference nil. */
static void build_body(char *buf, size_t sz, int prompt) {
    switch (prompt) {
    case 34: /* RESOURCE_CURRENT: EventMessageResourceCurrent */
        snprintf(buf, sz,
                 "{\"resource\":{\"path\":\"%s\",\"unique\":\"\",\"seek\":0,\"end\":0,\"groups\":[]},"
                 "\"duration\":0,\"seek\":0,\"hit_cache\":false,\"error\":\"\"}",
                 g_current_resource);
        break;
    case 10: /* RESOURCE_LIST: EventMessageResourceList */
        build_resource_list_json(buf, sz);
        break;
    case 8:  /* RESOURCE_ADD: EventMessageResourceAdd */
        snprintf(buf, sz,
                 "{\"resource\":{\"path\":\"%s\",\"unique\":\"\",\"seek\":0,\"end\":0,\"groups\":[]},\"error\":\"\"}",
                 g_current_resource);
        break;
    case 9:  /* RESOURCE_REMOVE: EventMessageResourceRemove */
    case 35: /* RESOURCE_SEEK: EventMessageResourceSeek */
        snprintf(buf, sz,
                 "{\"resource\":{\"path\":\"%s\",\"unique\":\"\",\"seek\":0,\"end\":0,\"groups\":[]},\"error\":\"\"}",
                 g_current_resource);
        break;
    case 5:  /* OUTPUT_ADD: EventMessageOutputAdd */
        snprintf(buf, sz, "{\"output\":{\"path\":\"%s\",\"unique\":\"\"},\"error\":\"\"}",
                 g_output_count > 0 ? g_outputs[g_output_count - 1] : "");
        break;
    case 6:  /* OUTPUT_REMOVE: EventMessageOutputRemove */
        snprintf(buf, sz, "{\"output\":{\"path\":\"\",\"unique\":\"\"},\"error\":\"\"}");
        break;
    case 7:  /* OUTPUT_LIST: EventMessageOutputList */
        build_output_list_json(buf, sz);
        break;
    case 11: /* PLUGIN_ADD: EventMessagePluginAdd */
        snprintf(buf, sz,
                 "{\"plugin\":{\"path\":\"%s\",\"unique\":\"\",\"name\":\"\",\"author\":\"\","
                 "\"media_type\":0,\"sub_count\":0,\"params\":{}},\"error\":\"\"}",
                 g_plugin_count > 0 ? g_plugins[g_plugin_count - 1] : "");
        break;
    case 12: /* PLUGIN_REMOVE: EventMessagePluginRemove */
    case 32: /* PLUGIN_UPDATE: EventMessagePluginUpdate */
        snprintf(buf, sz,
                 "{\"plugin\":{\"path\":\"\",\"unique\":\"\",\"name\":\"\",\"author\":\"\","
                 "\"media_type\":0,\"sub_count\":0,\"params\":{}},\"error\":\"\"}");
        break;
    case 13: /* PLUGIN_LIST: EventMessagePluginList */
        snprintf(buf, sz, "{\"plugins\":[],\"error\":\"\"}");
        break;
    default: /* player actions and anything else: error-only body */
        snprintf(buf, sz, "{\"error\":\"\"}");
        break;
    }
}

/* async reply context */
typedef struct {
    int prompt;
} reply_ctx;

static void *reply_thread(void *arg) {
    reply_ctx *ctx = (reply_ctx *)arg;
    usleep(20000); /* let the Go side reach Wait() */
    if (g_msg_cb != NULL) {
        int action = prompt_to_message(ctx->prompt);
        if (action >= 0) {
            char body[4096];
            build_body(body, sizeof(body), ctx->prompt);
            /* The callback signature is (action, body): the Go side wraps the
             * body into a KPMessage itself (see goCallBackMessage). */
            g_msg_cb(action, body);
        }
    }
    free(ctx);
    return NULL;
}

static void fire_reply(int prompt) {
    reply_ctx *ctx = malloc(sizeof(reply_ctx));
    if (ctx == NULL) return;
    ctx->prompt = prompt;
    pthread_t tid;
    if (pthread_create(&tid, NULL, reply_thread, ctx) != 0) {
        free(ctx);
    } else {
        pthread_detach(tid);
    }
}

/* ---- implemented API ------------------------------------------------ */
void GetInformation(char *buffer, int size) {
    if (buffer != NULL && size > 0) {
        snprintf(buffer, (size_t)size, "{}");
    }
}

void PromptMessage(int action, char *json) {
    char path[ITEM_LEN];
    switch (action) {
    case 8: /* RESOURCE_ADD: remember the resource and mark it current */
        extract_path(json, path, sizeof(path));
        push_item(g_resources, &g_resource_count, path);
        if (path[0] != '\0') snprintf(g_current_resource, ITEM_LEN, "%s", path);
        break;
    default:
        break;
    }
    fire_reply(action);
}

int Run(void) {
    g_running = 1;
    while (g_running) {
        usleep(500000);
    }
    return 0;
}

char *GetError(void) {
    return g_error;
}

void SetLogLevel(char *path, int level) {
    (void)path; (void)level;
}

void SetCacheOn(int on) {
    (void)on;
}

void SetCacheUncheckSource(void) {
}

void SetSkipInvalidResource(int skip) {
    (void)skip;
}

void ReceiveMessage(MessageCallBack cb) {
    g_msg_cb = cb;
}

void ProgressCallback(ProgressCallBack cb) {
    g_prog_cb = cb;
}

void Initialization(char *protocol,
                    int video_width, int video_height,
                    int video_bitrate, int video_quality,
                    int video_fps, int audio_sample_rate,
                    int audio_channel_layout, int audio_channels,
                    int video_fill_strategy) {
    (void)protocol; (void)video_width; (void)video_height;
    (void)video_bitrate; (void)video_quality; (void)video_fps;
    (void)audio_sample_rate; (void)audio_channel_layout;
    (void)audio_channels; (void)video_fill_strategy;
    (void)g_prog_cb; /* keep the callback reference for future simulation */
}

int AddOutput(char *json) {
    char path[ITEM_LEN];
    extract_path(json, path, sizeof(path));
    push_item(g_outputs, &g_output_count, path);
    fire_reply(5); /* OUTPUT_ADD prompt */
    return 0;
}

int AddPlugin(char *json) {
    char path[ITEM_LEN];
    extract_path(json, path, sizeof(path));
    push_item(g_plugins, &g_plugin_count, path);
    fire_reply(11); /* PLUGIN_ADD prompt */
    return 0;
}
