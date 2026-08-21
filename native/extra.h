#ifndef KPLAYER_EXTRA_H
#define KPLAYER_EXTRA_H

/* API-compatible stub header for the KPlayer Go control layer.
 * Signatures reverse-engineered from core/kplayer.go cgo calls.
 * This is NOT the official libkplayer header; it implements the minimal
 * surface needed to build and run the Go side with a simulated engine.
 */

#include <stdlib.h>
#include <string.h>

typedef void (*MessageCallBack)(int action, char *msg);
typedef void (*ProgressCallBack)(double percent, int bitRate);

void GetInformation(char *buffer, int size);
void PromptMessage(int action, char *json);
int Run(void);
char *GetError(void);
void SetLogLevel(char *path, int level);
void SetCacheOn(int on);
void SetCacheUncheckSource(void);
void SetSkipInvalidResource(int skip);
void ReceiveMessage(MessageCallBack cb);
void ProgressCallback(ProgressCallBack cb);
void Initialization(char *protocol,
                    int video_width, int video_height,
                    int video_bitrate, int video_quality,
                    int video_fps, int audio_sample_rate,
                    int audio_channel_layout, int audio_channels,
                    int video_fill_strategy);
int AddOutput(char *json);
int AddPlugin(char *json);

#endif /* KPLAYER_EXTRA_H */
