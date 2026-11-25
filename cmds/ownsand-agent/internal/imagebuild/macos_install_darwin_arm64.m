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

API_AVAILABLE(macos(12.0))
static BOOL installVMTerminated(VZVirtualMachine *vm) {
    VZVirtualMachineState state = vm.state;
    return state == VZVirtualMachineStateStopped || state == VZVirtualMachineStateError;
}

bool ownsandMacOSInstallClose(void *value, bool stopVM, int64_t timeoutNanos, char **stopError) {
    if (@available(macOS 12.0, *)) {
        __attribute__((objc_precise_lifetime))
        OwnSandMacOSInstallation *installation = (__bridge_transfer OwnSandMacOSInstallation *)value;
        if (!stopVM) {
            // Fence the install completion callback before releasing the installer.
            dispatch_sync(installation.queue, ^{});
            return true;
        }

        dispatch_time_t deadline = dispatch_time(DISPATCH_TIME_NOW, timeoutNanos);
        dispatch_semaphore_t completed = dispatch_semaphore_create(0);
        dispatch_source_t observer = dispatch_source_create(DISPATCH_SOURCE_TYPE_TIMER, 0, 0, installation.queue);
        __block NSError *failure = nil;
        // The stop request, callback and state checks run on the install queue.
        void (^finish)(NSError *) = ^(NSError *error) {
            if (!dispatch_source_testcancel(observer)) {
                failure = error;
                dispatch_source_cancel(observer);
                dispatch_semaphore_signal(completed);
            }
        };
        dispatch_source_set_timer(observer, DISPATCH_TIME_NOW, 10 * NSEC_PER_MSEC, 0);
        dispatch_source_set_event_handler(observer, ^{
            if (installVMTerminated(installation.installer.virtualMachine)) {
                finish(nil);
            }
        });
        dispatch_activate(observer);
        dispatch_async(installation.queue, ^{
            // This runs after the install completion callback has returned.
            if (dispatch_source_testcancel(observer)) {
                return;
            }
            VZVirtualMachine *vm = installation.installer.virtualMachine;
            if (installVMTerminated(vm)) {
                finish(nil);
                return;
            }
            if (vm.canStop && !dispatch_source_testcancel(observer)) {
                [vm stopWithCompletionHandler:^(NSError *error) {
                    // Keep the installer and VM alive through this callback,
                    // including when the caller has already timed out.
                    if (installVMTerminated(installation.installer.virtualMachine)) {
                        finish(nil);
                    } else if (error != nil) {
                        finish(error);
                    }
                }];
            }
        });
        if (dispatch_semaphore_wait(completed, deadline) != 0) {
            dispatch_source_cancel(observer);
            return false;
        }
        if (failure != nil) {
            @autoreleasepool {
                *stopError = strdup(failure.description.UTF8String);
            }
            return false;
        }
        return true;
    }
    return false;
}
