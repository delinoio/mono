// SPDX-License-Identifier: FSL-1.1-ALv2

#import <Foundation/Foundation.h>
#import <Virtualization/Virtualization.h>
#include <string.h>

#include "_cgo_export.h"

API_AVAILABLE(macos(12.0))
@interface OwnSandMacOSInstallation : NSObject
@property (nonatomic, strong) dispatch_queue_t queue;
@property (nonatomic, strong) VZMacOSInstaller *installer;
@property (nonatomic) BOOL completed;
@end

@implementation OwnSandMacOSInstallation
@end

static NSURL *fileURL(const char *path) {
    return [NSURL fileURLWithFileSystemRepresentation:path isDirectory:NO relativeToURL:nil];
}

// Match the disk and devices of vzrt.NewMacOSConfiguration for the
// install VM. The install never uses a preset identifier or a network device.
API_AVAILABLE(macos(12.0))
static VZVirtualMachineConfiguration *installConfiguration(const char *disk, const char *aux,
    const void *hardwareModel, size_t modelSize, size_t cpus, uint64_t memory, NSError **error)
{
    NSData *modelData = [NSData dataWithBytes:hardwareModel length:modelSize];
    VZMacHardwareModel *model = [[VZMacHardwareModel alloc] initWithDataRepresentation:modelData];
    if (model == nil || !model.supported) {
        return nil;
    }

    VZMacPlatformConfiguration *platform = [[VZMacPlatformConfiguration alloc] init];
    platform.hardwareModel = model;
    platform.machineIdentifier = [[VZMacMachineIdentifier alloc] init];
    if (@available(macOS 13.0, *)) {
        platform.auxiliaryStorage = [[VZMacAuxiliaryStorage alloc] initWithURL:fileURL(aux)];
    } else {
        // macOS 12 only provides the older initializer.
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
        platform.auxiliaryStorage = [[VZMacAuxiliaryStorage alloc] initWithContentsOfURL:fileURL(aux)];
#pragma clang diagnostic pop
    }

    VZDiskImageStorageDeviceAttachment *attachment = [[VZDiskImageStorageDeviceAttachment alloc]
        initWithURL:fileURL(disk)
        readOnly:NO
        cachingMode:VZDiskImageCachingModeAutomatic
        synchronizationMode:VZDiskImageSynchronizationModeFull
        error:error];
    if (attachment == nil) {
        return nil;
    }

    VZMacGraphicsDeviceConfiguration *graphics = [[VZMacGraphicsDeviceConfiguration alloc] init];
    graphics.displays = @[[[VZMacGraphicsDisplayConfiguration alloc]
        initWithWidthInPixels:1280 heightInPixels:800 pixelsPerInch:80]];

    VZVirtualMachineConfiguration *config = [[VZVirtualMachineConfiguration alloc] init];
    config.platform = platform;
    config.bootLoader = [[VZMacOSBootLoader alloc] init];
    config.CPUCount = cpus;
    config.memorySize = memory;
    config.storageDevices = @[[[VZVirtioBlockDeviceConfiguration alloc] initWithAttachment:attachment]];
    config.socketDevices = @[[[VZVirtioSocketDeviceConfiguration alloc] init]];
    config.entropyDevices = @[[[VZVirtioEntropyDeviceConfiguration alloc] init]];
    config.graphicsDevices = @[graphics];
    return [config validateWithError:error] ? config : nil;
}

void *ownsandMacOSInstallStart(const char *disk, const char *aux, const char *restore,
    const void *hardwareModel, size_t modelSize, size_t cpus, uint64_t memory,
    uintptr_t handle, char **errorMessage)
{
    if (@available(macOS 12.0, *)) {
        @autoreleasepool {
            NSError *error = nil;
            VZVirtualMachineConfiguration *config = installConfiguration(
                disk, aux, hardwareModel, modelSize, cpus, memory, &error);
            if (config == nil) {
                *errorMessage = strdup((error.description ?: @"Unsupported macOS install configuration").UTF8String);
                return NULL;
            }

            OwnSandMacOSInstallation *installation = [[OwnSandMacOSInstallation alloc] init];
            installation.queue = dispatch_queue_create("io.delino.ownsand.macos-install", DISPATCH_QUEUE_SERIAL);
            NSURL *restoreURL = fileURL(restore);
            __weak OwnSandMacOSInstallation *weakInstallation = installation;
            dispatch_sync(installation.queue, ^{
                @autoreleasepool {
                    VZVirtualMachine *vm = [[VZVirtualMachine alloc] initWithConfiguration:config queue:installation.queue];
                    VZMacOSInstaller *installer = [[VZMacOSInstaller alloc] initWithVirtualMachine:vm restoreImageURL:restoreURL];
                    // This is the last cancellation check before callback registration.
                    if (!ownsandMacOSInstallCanStart(handle)) {
                        return;
                    }
                    installation.installer = installer;
                    [installer installWithCompletionHandler:^(NSError *installError) {
                        @autoreleasepool {
                            weakInstallation.completed = YES;
                            ownsandMacOSInstallCompleted(handle, (char *)installError.domain.UTF8String, installError.code,
                                (char *)installError.localizedDescription.UTF8String, (char *)installError.userInfo.description.UTF8String);
                        }
                    }];
                }
            });
            if (installation.installer == nil) {
                return NULL;
            }
            return (__bridge_retained void *)installation;
        }
    }
    *errorMessage = strdup("the macOS installer requires macOS 12 or later");
    return NULL;
}

void ownsandMacOSInstallCancel(void *value) {
    if (@available(macOS 12.0, *)) {
        OwnSandMacOSInstallation *installation = (__bridge OwnSandMacOSInstallation *)value;
        dispatch_sync(installation.queue, ^{
            if (!installation.completed && installation.installer.progress.cancellable) {
                [installation.installer.progress cancel];
            }
        });
    }
}

double ownsandMacOSInstallFractionCompleted(void *value) {
    if (@available(macOS 12.0, *)) {
        OwnSandMacOSInstallation *installation = (__bridge OwnSandMacOSInstallation *)value;
        __block double fraction = 0;
        dispatch_sync(installation.queue, ^{
            fraction = installation.installer.progress.fractionCompleted;
        });
        return fraction;
    }
    return 0;
}

void ownsandMacOSInstallClose(void *value, bool stopVM) {
    if (@available(macOS 12.0, *)) {
        // Keep the VM and installer alive through the optional stop callback.
        __attribute__((objc_precise_lifetime))
        OwnSandMacOSInstallation *installation = (__bridge_transfer OwnSandMacOSInstallation *)value;
        dispatch_semaphore_t stopped = dispatch_semaphore_create(0);
        __block BOOL stopping = NO;
        // Fence the completion callback before releasing the installer.
        dispatch_sync(installation.queue, ^{
            VZVirtualMachine *vm = installation.installer.virtualMachine;
            if (stopVM && installation.completed && vm.canStop) {
                stopping = YES;
                [vm stopWithCompletionHandler:^(NSError *unusedError) {
                    dispatch_semaphore_signal(stopped);
                }];
            }
        });
        if (stopping) {
            dispatch_semaphore_wait(stopped, DISPATCH_TIME_FOREVER);
            dispatch_sync(installation.queue, ^{});
        }
    }
}
