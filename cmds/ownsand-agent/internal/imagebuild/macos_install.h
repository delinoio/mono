// SPDX-License-Identifier: FSL-1.1-ALv2

#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

void *ownsandMacOSInstallStart(const char *disk, const char *aux, const char *restore,
    const void *hardwareModel, size_t modelSize, size_t cpus, uint64_t memory,
    uintptr_t handle, char **errorMessage);
void ownsandMacOSInstallCancel(void *installation);
double ownsandMacOSInstallFractionCompleted(void *installation);
void ownsandMacOSInstallClose(void *installation, bool stopVM);
