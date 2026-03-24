//go:build darwin && cgo && sparkle

#import "updater_bridge.h"

#import <Cocoa/Cocoa.h>
#import <dispatch/dispatch.h>
#include <string.h>

@interface SPUUpdater : NSObject

- (BOOL)startUpdater:(NSError * _Nullable __autoreleasing * _Nullable)error;

@end

@interface SPUStandardUpdaterController : NSObject

@property (nonatomic, readonly) SPUUpdater *updater;

- (instancetype)initWithStartingUpdater:(BOOL)startUpdater
                        updaterDelegate:(id _Nullable)updaterDelegate
                      userDriverDelegate:(id _Nullable)userDriverDelegate;
- (IBAction)checkForUpdates:(id _Nullable)sender;

@end

static SPUStandardUpdaterController *copilotProxyUpdaterController = nil;
static BOOL copilotProxyUpdaterStarted = NO;

static char *copy_error_message(NSString *message)
{
    NSString *fallbackMessage = message ?: @"Sparkle returned an unknown error";
    const char *utf8 = fallbackMessage.UTF8String;
    if (utf8 == NULL) {
        return strdup("Sparkle returned an unknown error");
    }
    return strdup(utf8);
}

static void run_on_main_queue_sync(dispatch_block_t block)
{
    if ([NSThread isMainThread]) {
        block();
        return;
    }

    dispatch_sync(dispatch_get_main_queue(), block);
}

char *copilot_proxy_updater_start(void)
{
    __block char *errorMessage = NULL;

    run_on_main_queue_sync(^{
        if (copilotProxyUpdaterController == nil) {
            copilotProxyUpdaterController =
                [[SPUStandardUpdaterController alloc] initWithStartingUpdater:NO
                                                              updaterDelegate:nil
                                                            userDriverDelegate:nil];
        }

        if (copilotProxyUpdaterStarted) {
            return;
        }

        NSError *error = nil;
        if (![copilotProxyUpdaterController.updater startUpdater:&error]) {
            errorMessage = copy_error_message(error.localizedDescription);
            return;
        }

        copilotProxyUpdaterStarted = YES;
    });

    return errorMessage;
}

char *copilot_proxy_updater_check(void)
{
    __block char *errorMessage = NULL;

    run_on_main_queue_sync(^{
        if (copilotProxyUpdaterController == nil || !copilotProxyUpdaterStarted) {
            errorMessage = copy_error_message(@"The updater is not available yet.");
            return;
        }

        [copilotProxyUpdaterController checkForUpdates:nil];
    });

    return errorMessage;
}
